// Package xiaoya contains custom drivers ported for xiaoya compatibility.
//
// These drivers are independent of the upstream driver code on purpose:
// they live in their own package so that merging upstream changes never
// touches them. The only shared file is drivers/all.go (one import line).
//
// QuarkShare reads a Quark (夸克) share link directly, without requiring a
// personal Quark account. It reproduces the behavior of the xiaoya build so
// that storage rows exported from a xiaoya instance work unmodified:
//
//	addition: {
//	  "cookie":         "<pan.quark.cn session cookie>",
//	  "root_folder_id": "<fid inside the share, or 0>",
//	  "share_id":       "<share code (pwd_id)>",
//	  "pass_code":      "<extract code>",
//	  "order_by":       "name",
//	  "order_direction":"asc"
//	}
//
// API chain (verified against the rayburst reference implementation):
//
//	share/sharepage/token  -> stoken
//	share/sharepage/detail -> file list (paged, carries share_fid_token)
//	file/download          -> download url (signed against the cookie of
//	                          this very response, including a fresh __puus)
package xiaoya

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/cookie"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

const (
	quarkAPI     = "https://drive.quark.cn/1/clouddrive"
	quarkReferer = "https://pan.quark.cn"
	quarkUA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/2.5.20 Chrome/100.0.4896.160 Electron/18.3.5.4-b478491100 Safari/537.36 Channel/pckk_other_ch"
	quarkAccept  = "application/json, text/plain, */*"
	// quarkMaxSharePages caps listing at 100 pages * 100 items.
	quarkMaxSharePages = 100
	quarkPageSize      = 100
)

// ---------- types ----------

type quarkResp struct {
	Status  int    `json:"status"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type quarkShareTokenResp struct {
	quarkResp
	Data struct {
		Stoken string `json:"stoken"`
	} `json:"data"`
}

type quarkShareFile struct {
	Fid           string `json:"fid"`
	FileName      string `json:"file_name"`
	Size          int64  `json:"size"`
	File          bool   `json:"file"`
	Dir           bool   `json:"dir"`
	Category      int    `json:"category"`
	UpdatedAt     int64  `json:"updated_at"` // milliseconds
	ShareFidToken string `json:"share_fid_token"`
}

type quarkDetailResp struct {
	quarkResp
	Metadata struct {
		Total int64 `json:"_total"`
	} `json:"metadata"`
	Data struct {
		List []quarkShareFile `json:"list"`
	} `json:"data"`
}

type quarkDownResp struct {
	quarkResp
	Data []struct {
		DownloadUrl string `json:"download_url"`
	} `json:"data"`
}

// ShareObj carries the share context needed by the download API.
type ShareObj struct {
	model.Object
	ShareFidToken string
	PdirFid       string
}

// ---------- driver ----------

type QuarkShare struct {
	model.Storage
	QuarkAddition
	cookieMu sync.Mutex
}

func (d *QuarkShare) Config() driver.Config {
	return quarkShareConfig
}

func (d *QuarkShare) GetAddition() driver.Additional {
	return &d.QuarkAddition
}

func (d *QuarkShare) Init(ctx context.Context) error {
	if d.QuarkAddition.ShareId == "" {
		return errors.New("share_id is required")
	}
	return d.getShareToken()
}

func (d *QuarkShare) Drop(ctx context.Context) error {
	return nil
}

func (d *QuarkShare) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	parent := dir.GetID()
	if parent == "" {
		parent = "0"
	}
	files, err := d.getShareFiles(parent)
	if err != nil {
		return nil, err
	}
	objs := make([]model.Obj, 0, len(files))
	for i := range files {
		f := &files[i]
		name := html.UnescapeString(f.FileName)
		mod := time.UnixMilli(f.UpdatedAt)
		obj := &ShareObj{
			ShareFidToken: f.ShareFidToken,
			PdirFid:       parent,
			Object: model.Object{
				ID:       f.Fid,
				Name:     name,
				Size:     f.Size,
				IsFolder: f.Dir || !f.File,
				Modified: mod,
				Ctime:    mod,
			},
		}
		objs = append(objs, obj)
	}
	return objs, nil
}

func (d *QuarkShare) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	// Attempt chain: raw -> refresh stoken -> refresh __puus -> raw again.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt == 1 {
			if err := d.getShareToken(); err != nil {
				log.Warnf("quark_share: refresh stoken failed: %v", err)
			}
		}
		if attempt == 2 {
			if err := d.refreshPuus(); err != nil {
				log.Warnf("quark_share: refresh __puus failed: %v", err)
			}
		}
		link, err := d.tryLink(file)
		if err == nil {
			return link, nil
		}
		lastErr = err
		log.Warnf("quark_share: link attempt %d failed: %v", attempt+1, err)
	}
	return nil, lastErr
}

func (d *QuarkShare) tryLink(file model.Obj) (*model.Link, error) {
	fid := file.GetID()
	pdirFid := "0"
	fidToken := ""
	if so, ok := file.(*ShareObj); ok {
		fidToken = so.ShareFidToken
		if so.PdirFid != "" {
			pdirFid = so.PdirFid
		}
	}
	if fidToken == "" {
		return nil, fmt.Errorf("missing share_fid_token for %s (stale index, please re-enter the folder)", file.GetName())
	}

	// Snapshot the cookie before the request: the download URL is signed
	// against the cookie sent with THIS request, and the response may issue
	// a fresh __puus that must be used for the actual download.
	d.cookieMu.Lock()
	reqCookie := d.QuarkAddition.Cookie
	d.cookieMu.Unlock()

	var resp quarkDownResp
	_, err := d.request("/file/download", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"fids":            []string{fid},
			"stoken":          d.QuarkAddition.ShareToken,
			"pwd_id":          d.QuarkAddition.ShareId,
			"pdir_fid":        pdirFid,
			"share_fid_token": []string{fidToken},
		})
	}, &resp, reqCookie)
	if err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 || resp.Data[0].DownloadUrl == "" {
		return nil, errors.New("no download link found in quark response")
	}

	// Merge any fresh __puus from the download response into the cookie used
	// for downloading (old cookie -> 412, merged -> 206, verified upstream).
	dlCookie := cookie.SetStr(reqCookie, "__puus", cookie.GetStr(reqCookie, "__puus"))
	d.cookieMu.Lock()
	newPuus := cookie.GetStr(d.QuarkAddition.Cookie, "__puus")
	d.cookieMu.Unlock()
	if newPuus != "" {
		dlCookie = cookie.SetStr(dlCookie, "__puus", newPuus)
	}

	return &model.Link{
		URL: resp.Data[0].DownloadUrl,
		Header: http.Header{
			"User-Agent": []string{quarkUA},
			"Referer":    []string{quarkReferer},
			"Cookie":     []string{dlCookie},
		},
	}, nil
}

var _ driver.Driver = (*QuarkShare)(nil)

// ---------- util ----------

// request performs a signed quark API call with the whole cookie forwarded.
func (d *QuarkShare) request(pathname string, method string, callback base.ReqCallback, resp interface{}, cookieOverride string) ([]byte, error) {
	ck := cookieOverride
	if ck == "" {
		d.cookieMu.Lock()
		ck = d.QuarkAddition.Cookie
		d.cookieMu.Unlock()
	}
	u := quarkAPI + pathname
	req := base.RestyClient.R()
	req.SetHeaders(map[string]string{
		"Cookie":     ck,
		"Accept":     quarkAccept,
		"User-Agent": quarkUA,
		"Referer":    quarkReferer,
	})
	req.SetQueryParam("pr", "ucpro")
	req.SetQueryParam("fr", "pc")
	if callback != nil {
		callback(req)
	}
	if resp != nil {
		req.SetResult(resp)
	}
	var e quarkResp
	req.SetError(&e)
	res, err := req.Execute(method, u)
	logResp("quark", res, err)
	if err != nil {
		return nil, err
	}
	d.mergeResponseCookies(res)
	if e.Status >= 400 || e.Code != 0 {
		return res.Body(), errors.New(e.Message)
	}
	return res.Body(), nil
}

func (d *QuarkShare) mergeResponseCookies(res *resty.Response) {
	if res == nil {
		return
	}
	puus := cookie.GetCookie(res.Cookies(), "__puus")
	if puus == nil {
		return
	}
	d.cookieMu.Lock()
	d.QuarkAddition.Cookie = cookie.SetStr(d.QuarkAddition.Cookie, "__puus", puus.Value)
	d.cookieMu.Unlock()
	op.MustSaveDriverStorage(d)
}

func (d *QuarkShare) getShareToken() error {
	var resp quarkShareTokenResp
	_, err := d.request("/share/sharepage/token", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"pwd_id":   d.QuarkAddition.ShareId,
			"passcode": d.QuarkAddition.PassCode,
		})
	}, &resp, "")
	if err != nil {
		return err
	}
	if resp.Data.Stoken == "" {
		return errors.New("empty stoken: " + resp.Message)
	}
	d.QuarkAddition.ShareToken = resp.Data.Stoken
	op.MustSaveDriverStorage(d)
	return nil
}

func (d *QuarkShare) getShareFiles(parentFid string) ([]quarkShareFile, error) {
	files := make([]quarkShareFile, 0)
	orderBy := d.QuarkAddition.OrderBy
	if orderBy == "" {
		orderBy = "file_name"
	}
	orderDir := d.QuarkAddition.OrderDirection
	if orderDir == "" {
		orderDir = "asc"
	}
	sort := "file_type:asc," + orderBy + ":" + orderDir
	for page := 1; page <= quarkMaxSharePages; page++ {
		var resp quarkDetailResp
		_, err := d.request("/share/sharepage/detail", http.MethodGet, func(req *resty.Request) {
			req.SetQueryParams(map[string]string{
				"pwd_id":        d.QuarkAddition.ShareId,
				"stoken":        d.QuarkAddition.ShareToken,
				"pdir_fid":      parentFid,
				"force":         "0",
				"_page":         strconv.Itoa(page),
				"_size":         strconv.Itoa(quarkPageSize),
				"_fetch_banner": "0",
				"_fetch_share":  "0",
				"_fetch_total":  "1",
				"_sort":         sort,
			})
		}, &resp, "")
		if err != nil {
			return nil, err
		}
		files = append(files, resp.Data.List...)
		total := resp.Metadata.Total
		if int64(page*quarkPageSize) >= total || len(resp.Data.List) == 0 {
			break
		}
	}
	return files, nil
}

// refreshPuus renews the session cookie: strip __puus, hit /config, merge the
// fresh value that the server sends back via Set-Cookie.
func (d *QuarkShare) refreshPuus() error {
	d.cookieMu.Lock()
	old := d.QuarkAddition.Cookie
	d.cookieMu.Unlock()
	stripped := cookie.DelStr(old, "__puus")
	_, err := d.request("/config", http.MethodGet, nil, nil, stripped)
	if err != nil {
		log.Warnf("quark_share: refresh __puus failed: %v", err)
		return err
	}
	d.cookieMu.Lock()
	defer d.cookieMu.Unlock()
	if cookie.GetStr(d.QuarkAddition.Cookie, "__puus") == "" {
		d.QuarkAddition.Cookie = old
		return errors.New("quark did not reissue __puus, cookie may be expired")
	}
	op.MustSaveDriverStorage(d)
	return nil
}
