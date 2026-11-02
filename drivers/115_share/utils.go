package _115_share

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	m115 "github.com/SheltonZhu/115driver/pkg/crypto/m115"
	driver115 "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	appVerURL115 = "https://appversion.115.com/1/web/1.0/api/chrome"
	// appVerFallback115 is used when the live appversion.115.com query fails.
	// 115 only accepts its own browser UA — the share downurl API answers a
	// generic UA with errno 50029 — and a *stale* version is blocked by the
	// Aliyun WAF with a 405 page (the SDK bundled 27.0.5.7 is blocked today).
	// Keep this a version 115 still accepts; current live version: 36.0.1.
	appVerFallback115 = "36.0.1"
)

// appVerResp mirrors appversion.115.com/1/web/1.0/api/chrome.
type appVerResp struct {
	Data struct {
		Win struct {
			VersionCode string `json:"version_code"`
		} `json:"win"`
	} `json:"data"`
}

var (
	appVerOnce   sync.Once
	appVerCached string
)

// latestAppVer resolves the current 115Browser version once per process. The
// CDN link is bound to the UA, so every request path must use the same value.
// On any failure it falls back to appVerFallback115, a version 115 accepts.
func latestAppVer() string {
	appVerOnce.Do(func() {
		appVerCached = appVerFallback115
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := base.RestyClient.R().SetContext(ctx).Get(appVerURL115)
		// The endpoint declares "Content-Type: text/html" while returning JSON,
		// so resty's SetResult would silently leave the struct empty: parse the
		// raw body instead.
		var resp appVerResp
		if err != nil || res == nil || res.StatusCode() != 200 ||
			json.Unmarshal(res.Body(), &resp) != nil || resp.Data.Win.VersionCode == "" {
			log.Infof("[115share/appver] live query failed, using fallback %s", appVerFallback115)
			return
		}
		appVerCached = resp.Data.Win.VersionCode
		log.Infof("[115share/appver] latest 115Browser version: %s", appVerCached)
	})
	return appVerCached
}

// ua115 is the UA the share endpoints expect; the CDN link returned by the
// share downurl API is bound to it, so it must also ride on the download.
func ua115() string {
	return "Mozilla/5.0 115Browser/" + latestAppVer()
}

var _ model.Obj = (*FileObj)(nil)

type FileObj struct {
	Size     int64
	Sha1     string
	Utm      time.Time
	FileName string
	isDir    bool
	FileID   string
	ThumbURL string
}

func (f *FileObj) CreateTime() time.Time {
	return f.Utm
}

func (f *FileObj) GetHash() utils.HashInfo {
	return utils.NewHashInfo(utils.SHA1, f.Sha1)
}

func (f *FileObj) GetSize() int64 {
	return f.Size
}

func (f *FileObj) GetName() string {
	return f.FileName
}

func (f *FileObj) ModTime() time.Time {
	return f.Utm
}

func (f *FileObj) IsDir() bool {
	return f.isDir
}

func (f *FileObj) GetID() string {
	return f.FileID
}

func (f *FileObj) GetPath() string {
	return ""
}

func (f *FileObj) Thumb() string {
	return f.ThumbURL
}

type shareFile struct {
	FileID     string                 `json:"fid"`
	UID        int                    `json:"uid"`
	CategoryID driver115.IntString    `json:"cid"`
	FileName   string                 `json:"n"`
	Type       string                 `json:"ico"`
	Sha1       string                 `json:"sha"`
	Size       driver115.StringInt64  `json:"s"`
	Labels     []*driver115.LabelInfo `json:"fl"`
	UpdateTime string                 `json:"t"`
	IsFile     int                    `json:"fc"`
	ParentID   string                 `json:"pid"`
	ThumbURL   string                 `json:"u"`
}

type shareSnapResp struct {
	driver115.BasicResp
	Data struct {
		Count int         `json:"count"`
		List  []shareFile `json:"list"`
	} `json:"data"`
}

func transFunc(sf shareFile) (model.Obj, error) {
	timeInt, err := strconv.ParseInt(sf.UpdateTime, 10, 64)
	if err != nil {
		return nil, err
	}
	var (
		utm    = time.Unix(timeInt, 0)
		isDir  = (sf.IsFile == 0)
		fileID = string(sf.FileID)
	)
	if isDir {
		fileID = string(sf.CategoryID)
	}
	return &FileObj{
		Size:     int64(sf.Size),
		Sha1:     sf.Sha1,
		Utm:      utm,
		FileName: string(sf.FileName),
		isDir:    isDir,
		FileID:   fileID,
		ThumbURL: sf.ThumbURL,
	}, nil
}

func buildShareReferer(shareCode, receiveCode string) string {
	return fmt.Sprintf("https://115cdn.com/s/%s?password=%s&", shareCode, receiveCode)
}

func (d *Pan115Share) getShareSnapWithUA(ua, dirID string, queries ...driver115.Query) (*shareSnapResp, error) {
	result := shareSnapResp{}
	query := map[string]string{
		"share_code":   d.ShareCode,
		"receive_code": d.ReceiveCode,
		"cid":          dirID,
		"limit":        "20",
		"asc":          "0",
		"offset":       "0",
		"format":       "json",
	}
	for _, q := range queries {
		q(&query)
	}

	req := d.client.NewRequest().
		SetQueryParams(query).
		SetHeader("referer", buildShareReferer(d.ShareCode, d.ReceiveCode)).
		ForceContentType("application/json;charset=UTF-8").
		SetResult(&result)
	if ua != "" {
		req = req.SetHeader("User-Agent", ua)
	}

	resp, err := req.Get(driver115.ApiShareSnap)
	if err := driver115.CheckErr(err, &result, resp); err != nil {
		return nil, err
	}
	return &result, nil
}

// appShareDownURL is the *app* share-downurl endpoint (what 115's own browser
// and apps call). The web endpoint the SDK ships
// (driver115.ApiDownloadGetShareUrl = 115cdn.com/webapi/share/downurl) now
// answers every web client with errno 50029 "当前版本过低" regardless of the UA,
// so the app endpoint is the only one that still resolves share files.
const appShareDownURL = "https://proapi.115.com/app/share/downurl"

// downloadByShareCodeWithUA resolves a share file's CDN URL through the app
// endpoint: the request body is an m115-encrypted JSON payload and the account
// cookie must ride along, and the response carries the download info encrypted
// the same way. It also returns the Set-Cookie values issued by the response,
// which the CDN expects in addition to the account cookie.
func (d *Pan115Share) downloadByShareCodeWithUA(ua, fileID string) (*driver115.SharedDownloadInfo, []string, error) {
	key := m115.GenerateKey()
	payload, err := json.Marshal(map[string]string{
		"share_code":   d.ShareCode,
		"receive_code": d.ReceiveCode,
		"file_id":      fileID,
	})
	if err != nil {
		return nil, nil, err
	}

	result := struct {
		driver115.BasicResp
		Data string `json:"data"`
	}{}
	req := d.client.NewRequest().
		SetQueryParam("t", driver115.Now().String()).
		SetFormData(map[string]string{"data": m115.Encode(payload, key)}).
		// 115's app endpoints are called with a JSON content type even though the
		// body is form encoded (this is what 115's own clients do).
		SetHeader("Content-Type", "application/json;charset=UTF-8").
		SetHeader("Cookie", d.Cookie).
		SetHeader("Accept", "application/json;charset=UTF-8").
		ForceContentType("application/json").
		SetResult(&result)
	if ua != "" {
		req = req.SetHeader("User-Agent", ua)
	}

	resp, err := req.Post(appShareDownURL)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to call 115 share downurl")
	}
	if resp == nil {
		return nil, nil, errors.New("empty response from 115 share downurl")
	}
	if err := driver115.CheckErr(nil, &result, resp); err != nil {
		body := strings.ReplaceAll(resp.String(), "\n", " ")
		if len(body) > 200 {
			body = body[:200] + "..."
		}
		log.Infof("[115share/downurl] share=%s file=%s -> %d %s", d.ShareCode, fileID, resp.StatusCode(), body)
		return nil, nil, err
	}

	plain, err := m115.Decode(result.Data, key)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to decrypt 115 share download info")
	}
	info := &driver115.SharedDownloadInfo{}
	if err := json.Unmarshal(plain, info); err != nil {
		return nil, nil, errors.Wrap(err, "failed to parse 115 share download info")
	}
	if info.URL.URL == "" {
		return nil, nil, fmt.Errorf("115 returned no download url for file %s", fileID)
	}
	return info, resp.Header().Values("Set-Cookie"), nil
}

// mergeCookie joins the account cookie with the Set-Cookie values issued by
// the share-downurl response; the CDN accepts only requests carrying both.
func mergeCookie(account string, setCookies []string) string {
	parts := make([]string, 0, len(setCookies)+1)
	if c := strings.TrimSpace(account); c != "" {
		parts = append(parts, c)
	}
	for _, sc := range setCookies {
		if kv := strings.TrimSpace(strings.SplitN(sc, ";", 2)[0]); kv != "" {
			parts = append(parts, kv)
		}
	}
	return strings.Join(parts, "; ")
}

func (d *Pan115Share) login() error {
	var err error
	opts := []driver115.Option{
		driver115.UA(ua115()),
	}
	d.client = driver115.New(opts...)
	if _, err = d.getShareSnapWithUA(ua115(), ""); err != nil {
		return errors.Wrap(err, "failed to get share snap")
	}
	cr := &driver115.Credential{}
	if d.QRCodeToken != "" {
		s := &driver115.QRCodeSession{
			UID: d.QRCodeToken,
		}
		if cr, err = d.client.QRCodeLoginWithApp(s, driver115.LoginApp(d.QRCodeSource)); err != nil {
			return errors.Wrap(err, "failed to login by qrcode")
		}
		d.Cookie = fmt.Sprintf("UID=%s;CID=%s;SEID=%s;KID=%s", cr.UID, cr.CID, cr.SEID, cr.KID)
		d.QRCodeToken = ""
	} else if d.Cookie != "" {
		if err = cr.FromCookie(d.Cookie); err != nil {
			return errors.Wrap(err, "failed to login by cookies")
		}
		d.client.ImportCredential(cr)
	} else {
		return errors.New("missing cookie or qrcode account")
	}

	return d.client.LoginCheck()
}
