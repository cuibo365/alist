// AliyundriveShare2Pan115 — xiaoya compatible driver.
//
// Port of the xiaoya private driver of the same name, so that storage rows
// exported from a xiaoya instance work unmodified:
//
//	addition: {
//	  "RefreshToken":         "<aliyun web refresh_token>",
//	  "RefreshTokenOpen":     "<open refresh_token — kept, unused>",
//	  "oauth_token_url":      "<xiaoya relay — kept, unused>",
//	  "client_id": "", "client_secret": "",
//	  "TempTransferFolderID": "<aliyun folder to copy share files into>",
//	  "share_id": "...", "share_pwd": "",
//	  "root_folder_id": "<share folder to start from>",
//	  "cookie": "<115 web cookie>",
//	  "dir_id": "<115 flash directory cid — rapid uploads land here>",
//	  "purge_pan115_temp": false,
//	  "order_by": "name", "order_direction": "ASC"
//	}
//
// The share list API does not return content_hash (SHA1), which 115 rapid
// upload requires. So downloading a file goes through:
//
//  1. copy the share file into the configured aliyun folder (free)
//  2. poll the own-drive listing until the copy reports content_hash
//  3. 115 rapid upload with that SHA1 (uppercase) -> pickcode
//  4. 115 downurl -> CDN link (bound to the 115Browser UA + cookie)
//
// Shortcuts: if the 115 account already holds a file with the same name and
// size in the landing directory, its pickcode is reused directly. If the 115
// path is unavailable, the aliyun download URL is returned as a (throttled)
// fallback so playback degrades gracefully instead of failing.
package xiaoya

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	cipher "github.com/SheltonZhu/115driver/pkg/crypto/ec115"
	driver115 "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const (
	aliTokenURL      = "https://auth.alipan.com/v2/account/token"
	aliShareTokenURL = "https://api.alipan.com/v2/share_link/get_share_token"
	aliShareListURL  = "https://api.alipan.com/adrive/v3/file/list"
	aliCopyURL       = "https://api.alipan.com/v2/file/copy"
	aliOwnListURL    = "https://api.aliyundrive.com/adrive/v3/file/list"
	aliOwnFileURL    = "https://api.aliyundrive.com/v2/file/get"
	aliUserGetURL    = "https://user.aliyundrive.com/v2/user/get"
	aliDownURL       = "https://api.alipan.com/v2/file/get_download_url"

	canaryShare = "client=web,app=share,version=v2.3.1"
	canaryOwn   = "client=windows,app=adrive,version=v6.0.0"

	webAPI115    = "https://webapi.115.com"
	appVerURL115 = "https://appversion.115.com/1/web/1.0/api/chrome"
	// appVerFallback115 is used when the live appversion.115.com query fails.
	// 115 only accepts its own browser UA — the share downurl API answers a
	// generic UA with errno 50029 — and a *stale* version is blocked by the
	// Aliyun WAF with a 405 page (the SDK bundled 27.0.5.7 is blocked today).
	// Keep this a version 115 still accepts; current live version: 36.0.1.
	appVerFallback115 = "36.0.1"

	// sdkAppVer115 must equal the 115driver package-level `appVer` constant
	// (pkg/driver/upload.go), because GenerateToken() hashes that constant into
	// the initupload `token` while we send `appversion` ourselves: any mismatch
	// makes 115 answer statusmsg "token invalid". 27.0.5.7 is also rejected
	// outright these days (statuscode 403 "请升级到最新版本"), so we compute
	// the token ourselves with the UA version instead — see token115.
	md5Salt115 = "Qclm8MGWUv59TnrR0XPg"

	copyPollTries = 20                     // attempts to read content_hash of the fresh copy
	copyPollDelay = 300 * time.Millisecond // between those attempts
	aliPageSize   = 200
	page115       = 115
	maxOffset115  = 5000
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

// latest115AppVer returns the current 115Browser version (rayburst m115_ua()
// style): the CDN link is bound to the UA, so list / flash-upload / download
// must all use the same value. On any failure it falls back to
// appVerFallback115, which is a version 115 still accepts.
func latest115AppVer() string {
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
			log.Infof("[xiaoya/115:appver] live query failed, using fallback %s", appVerFallback115)
			return
		}
		appVerCached = resp.Data.Win.VersionCode
		log.Infof("[xiaoya/115:appver] latest 115Browser version: %s", appVerCached)
	})
	return appVerCached
}

// ua115 is the UA bound to 115 CDN links — the same string must ride on the
// flash-upload client, the directory listing, the appversion form field and
// the download header.
func ua115() string {
	return "Mozilla/5.0 115Browser/" + latest115AppVer()
}

// ---------- addition ----------

type AliyunShare115Addition struct {
	// Required; JSON names follow the xiaoya addition exactly.
	RefreshToken string `json:"RefreshToken" required:"true"`
	Cookie       string `json:"cookie" required:"true"` // 115 web cookie
	ShareId      string `json:"share_id" required:"true"`
	SharePwd     string `json:"share_pwd"`
	driver.RootID

	// Optional. RefreshTokenOpen / oauth_token_url / client_id / client_secret
	// (the xiaoya open-token pair) are kept ONLY for addition compatibility
	// with xiaoya storage rows — they are deliberately unused: this driver is
	// web-domain only and refreshes RefreshToken directly at the official
	// endpoint, same as rayburst.
	RefreshTokenOpen     string `json:"RefreshTokenOpen"`
	OauthTokenURL        string `json:"oauth_token_url"`
	ClientID             string `json:"client_id"`
	ClientSecret         string `json:"client_secret"`
	TempTransferFolderID string `json:"TempTransferFolderID"`

	// 115 flash directory (cid): share files are rapid-uploaded here and the
	// name+size pre-lookup scans it. Explicit in the xiaoya addition.
	DirID string `json:"dir_id"`
	// Kept for addition compatibility (stored as a JSON string, e.g. "false");
	// semantics unknown, not implemented.
	PurgePan115Temp string `json:"purge_pan115_temp"`

	OrderBy        string `json:"order_by" type:"select" options:"name,size,updated_at,created_at" default:"name"`
	OrderDirection string `json:"order_direction" type:"select" options:"ASC,DESC" default:"ASC"`

	// Runtime cache, persisted back into the storage addition.
	AccessToken       string `json:"access_token"`
	TokenExpiresAt    int64  `json:"access_token_expires_at"`
	ShareToken        string `json:"share_token"`
	ShareTokenExpires int64  `json:"share_token_expires_at"`
	OwnDriveId        string `json:"own_drive_id"`
	ShareDriveId      string `json:"share_drive_id"`
}

var aliyunShare115Config = driver.Config{
	Name:         "AliyundriveShare2Pan115",
	LocalSort:    false,
	OnlyProxy:    false,
	NoUpload:     true,
	DefaultRoot:  "root",
	PersistCache: "on",
}

// ---------- types ----------

type aliShareTokenResp struct {
	ShareToken string `json:"share_token"`
	ExpiresIn  int64  `json:"expires_in"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

type aliWebTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Code         string `json:"code"`
	Message      string `json:"message"`
}

type aliFileItem struct {
	FileId      string `json:"file_id"`
	Name        string `json:"name"`
	Type        string `json:"type"` // "folder" | "file"
	Size        int64  `json:"size"`
	UpdatedAt   string `json:"updated_at"`
	DriveId     string `json:"drive_id"`
	ContentHash string `json:"content_hash"`
}

type aliListResp struct {
	Items      []aliFileItem `json:"items"`
	NextMarker string        `json:"next_marker"`
	Message    string        `json:"message"`
}

// aliOwnFileResp is the /v2/file/get answer: the copy is a server-side op, so
// asking for the copied file directly yields content_hash without paging the
// whole destination folder.
type aliOwnFileResp struct {
	FileId      string `json:"file_id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentHash string `json:"content_hash"`
	Message     string `json:"message"`
}

type aliUserResp struct {
	ResourceDriveId string `json:"resource_drive_id"`
	DefaultDriveId  string `json:"default_drive_id"`
	DriveId         string `json:"drive_id"`
	Message         string `json:"message"`
}

type aliCopyResp struct {
	FileId  string `json:"file_id"`
	DriveId string `json:"drive_id"`
	Tasks   []struct {
		FileId string `json:"file_id"`
	} `json:"tasks"`
	Message string `json:"message"`
}

type aliDownUrlResp struct {
	Url     string `json:"url"`
	Message string `json:"message"`
}

type ali115FileEntry struct {
	Name string `json:"n"`
	Size int64  `json:"s"`
	Pick string `json:"pc"`
	// 115 returns cid as a *string* ("3009883674856110511", and "0" for files);
	// declaring it as an int made the whole list unmarshal fail, which resty
	// swallows, leaving State=false and looking like "115 file list rejected".
	Cid string `json:"cid"`
}

type ali115FileListResp struct {
	State bool              `json:"state"`
	Data  []ali115FileEntry `json:"data"`
	Error string            `json:"error"`
	ErrNo int               `json:"errNo"`
}

// ---------- driver ----------

type AliyundriveShare2Pan115 struct {
	model.Storage
	AliyunShare115Addition
	mu               sync.Mutex
	client           *driver115.Pan115Client
	lastCopiedFileId string
	// pickCache memoises (name,size) -> 115 pickcode so replaying a file skips
	// the 115 directory listing entirely.
	pickCache map[string]string
}

func (d *AliyundriveShare2Pan115) Config() driver.Config {
	return aliyunShare115Config
}

func (d *AliyundriveShare2Pan115) GetAddition() driver.Additional {
	return &d.AliyunShare115Addition
}

func (d *AliyundriveShare2Pan115) Init(ctx context.Context) error {
	if d.AliyunShare115Addition.ShareId == "" {
		return errors.New("share_id is required")
	}
	if d.AliyunShare115Addition.RefreshToken == "" {
		return errors.New("RefreshToken is required")
	}
	if d.AliyunShare115Addition.Cookie == "" {
		return errors.New("cookie (115) is required")
	}
	// Cold start on purpose: Init does no network at all.
	//
	// alipan throttles `v2/share_link/get_share_token` to roughly ten calls per
	// window per IP (measured: a 5 req/s serial run starts returning 429 at the
	// 11th request), and alist initialises storages one after another, so a
	// fleet of ~100 of these storages used to spend the whole quota on the
	// first ten and mark every later one as broken.
	//
	// Both tokens (web + share) and the share drive id are therefore resolved
	// lazily by the first List/Link; ensureShareToken also paces every caller
	// through one process-wide gate and retries on 429. The 115 cookie stays
	// lazily validated on the first Link.
	return nil
}

func (d *AliyundriveShare2Pan115) Drop(ctx context.Context) error {
	return nil
}

func (d *AliyundriveShare2Pan115) rootID() string {
	if d.AliyunShare115Addition.RootFolderID != "" {
		return d.AliyunShare115Addition.RootFolderID
	}
	return "root"
}

func (d *AliyundriveShare2Pan115) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	parent := dir.GetID()
	if parent == "" {
		parent = d.rootID()
	}
	objs := make([]model.Obj, 0, 100)
	marker := ""
	for page := 0; page < 100; page++ {
		items, next, err := d.listShare(ctx, parent, marker)
		if err != nil {
			// share token may have expired; refresh once and retry
			if rerr := d.ensureShareToken(); rerr == nil {
				items, next, err = d.listShare(ctx, parent, marker)
			}
			if err != nil {
				return nil, err
			}
		}
		for i := range items {
			it := &items[i]
			mod, _ := time.Parse(time.RFC3339, it.UpdatedAt)
			objs = append(objs, &model.Object{
				ID:       it.FileId,
				Name:     it.Name,
				Size:     it.Size,
				IsFolder: it.Type == "folder",
				Modified: mod,
			})
		}
		if next == "" {
			break
		}
		marker = next
	}
	return objs, nil
}

func (d *AliyundriveShare2Pan115) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	t0, last := time.Now(), time.Now()
	var marks []string
	mark := func(name string) {
		now := time.Now()
		marks = append(marks, fmt.Sprintf("%s=%dms", name, now.Sub(last).Milliseconds()))
		last = now
	}
	defer func() {
		log.Infof("[xiaoya/115:timing] %s total=%dms", strings.Join(marks, " "), time.Since(t0).Milliseconds())
	}()

	// The CDN link is stamped with the UA that asked for it. drivers/115 hands
	// the caller's own UA (the browser's) to the download endpoint, so the link
	// it returns opens directly in that same browser. Do the same here; fall
	// back to the 115Browser UA when the caller sent none (webdav/s3/tasks).
	ua := ua115()
	if args.Header != nil {
		if v := args.Header.Get("User-Agent"); v != "" {
			ua = v
		}
	}
	log.Infof("[xiaoya/115:ua] download UA = %s", ua)

	// 1. context: tokens + drive ids + 115 client
	if err := d.ensureWebToken(); err != nil {
		return nil, err
	}
	if err := d.ensureShareToken(); err != nil {
		return nil, err
	}
	if err := d.ensureOwnDriveId(); err != nil {
		return nil, err
	}
	client, err := d.ensure115Client()
	mark("init")
	if err != nil {
		log.Warnf("share2pan115: 115 client failed: %v (fall back to aliyun link)", err)
		return d.aliyunFallbackLink(ctx, file)
	}
	recvCid, err := d.ensureRecvCid()
	if err != nil {
		log.Warnf("share2pan115: resolve 115 recv dir failed: %v (fall back to aliyun link)", err)
		return d.aliyunFallbackLink(ctx, file)
	}

	// 2. shortcut: 115 already holds a file with the same name+size. The result
	// is memoised per (name,size) so replaying a file skips the 115 directory
	// listing (which pages 115 entries at a time) entirely.
	key := cacheKey115(file.GetName(), file.GetSize())
	pick := d.cachedPickcode(key)
	if pick != "" {
		if l, err := d.downlink(client, pick, ua); err == nil {
			mark("cached-down")
			return l, nil
		}
		d.dropPickcode(key) // stale entry: the file is gone from 115
		pick = ""
	}
	if pick == "" {
		pick, err = findPickcodeByName(d.AliyunShare115Addition.Cookie, recvCid, file.GetName(), file.GetSize())
		mark("lookup")
		if err != nil {
			log.Warnf("share2pan115: 115 lookup failed: %v", err)
		}
	}
	if pick != "" {
		d.storePickcode(key, pick)
		l, err := d.downlink(client, pick, ua)
		mark("down")
		return l, err
	}

	// 3. copy the share file into our own aliyun folder, get its SHA1
	sha1Upper, err := d.obtainSha1(ctx, file)
	mark("sha1")
	if err != nil {
		log.Warnf("share2pan115: obtain sha1 failed: %v (fall back to aliyun link)", err)
		return d.aliyunFallbackLink(ctx, file)
	}

	// 4. 115 rapid upload with the SHA1
	pick, err = d.rapidUpload(client, file.GetName(), file.GetSize(), recvCid, sha1Upper)
	mark("rapid")
	if err != nil {
		log.Warnf("share2pan115: rapid upload failed: %v (fall back to aliyun link)", err)
		return d.aliyunFallbackLink(ctx, file)
	}
	d.storePickcode(key, pick)
	l, err := d.downlink(client, pick, ua)
	mark("down")
	return l, err
}

// ---------- pickcode memo ----------

func cacheKey115(name string, size int64) string {
	return name + "\x00" + strconv.FormatInt(size, 10)
}

func (d *AliyundriveShare2Pan115) cachedPickcode(key string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pickCache[key]
}

func (d *AliyundriveShare2Pan115) storePickcode(key, pick string) {
	if pick == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pickCache == nil {
		d.pickCache = make(map[string]string)
	}
	d.pickCache[key] = pick
}

func (d *AliyundriveShare2Pan115) dropPickcode(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.pickCache, key)
}

var _ driver.Driver = (*AliyundriveShare2Pan115)(nil)

// ---------- aliyun web-domain API ----------

func (d *AliyundriveShare2Pan115) save() {
	op.MustSaveDriverStorage(d)
}

// ---------- account-wide web token ----------
//
// All xiaoya rows point at the same aliyun account, but each carries its own
// RefreshToken: alipan rotates the refresh_token on every redemption and every
// row persisted its own copy. Refreshing per storage therefore meant N logins
// for one account on a cold start — N times the rate limit and N times the
// chance of the account being flagged, plus N different access_tokens.
//
// So the web token is shared per account: the first storage that needs one
// refreshes it, parks the result in a process-wide table, and writes the new
// RefreshToken / access_token back into the addition of every sibling storage.
// Siblings then reuse the table instead of refreshing again. Keeping the rows
// in sync also means any row can seed the group after a restart.

type accountTokenGroup struct {
	mu           sync.Mutex
	accessToken  string
	refreshToken string
	expiresAt    int64
}

var (
	accountTokenGroupsMu sync.Mutex
	accountTokenGroups   = map[string]*accountTokenGroup{}
)

func accountTokenGroupFor(key string) *accountTokenGroup {
	accountTokenGroupsMu.Lock()
	defer accountTokenGroupsMu.Unlock()
	g, ok := accountTokenGroups[key]
	if !ok {
		g = &accountTokenGroup{}
		accountTokenGroups[key] = g
	}
	return g
}

// tokenGroupKey identifies the aliyun account this storage belongs to.
// RefreshToken cannot serve as the key because it rotates on every use; the
// open-platform RefreshTokenOpen is copied verbatim from the xiaoya row and is
// never rewritten by this driver, which makes it a stable per-account marker.
// Storages without it keep the original one-refresh-per-storage behaviour.
func (d *AliyundriveShare2Pan115) tokenGroupKey() string {
	return tokenGroupKeyFor(d.AliyunShare115Addition.RefreshTokenOpen)
}

func tokenGroupKeyFor(refreshTokenOpen string) string {
	if refreshTokenOpen == "" {
		return ""
	}
	sum := sha1.Sum([]byte(refreshTokenOpen))
	return hex.EncodeToString(sum[:8])
}

// setTokenFields writes the runtime token fields. Callers serialise themselves
// (d.mu in single-storage mode, the group mutex otherwise), so this must not
// take a lock: ensureWebToken already holds one of the two.
func (d *AliyundriveShare2Pan115) setTokenFields(access, refresh string, expiresAt int64) {
	d.AliyunShare115Addition.AccessToken = access
	d.AliyunShare115Addition.RefreshToken = refresh
	d.AliyunShare115Addition.TokenExpiresAt = expiresAt
}

func (d *AliyundriveShare2Pan115) ensureWebToken() error {
	// Fast path: a live access_token on this instance needs no group lookup.
	if d.AliyunShare115Addition.AccessToken != "" &&
		time.Now().Unix() < d.AliyunShare115Addition.TokenExpiresAt-60 {
		return nil
	}
	key := d.tokenGroupKey()
	if key == "" {
		// No account marker in the addition: keep the original behaviour.
		d.mu.Lock()
		defer d.mu.Unlock()
		if err := d.refreshWebToken(nil); err != nil {
			return err
		}
		d.save()
		return nil
	}
	g := accountTokenGroupFor(key)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.accessToken != "" && time.Now().Unix() < g.expiresAt-60 {
		// A sibling storage already refreshed for this account. Adopt the
		// token in memory only: the broadcast that produced it already wrote
		// every sibling row, so writing again here would just turn a burst of
		// first hits (a library scan) into a burst of concurrent UPDATEs.
		d.setTokenFields(g.accessToken, g.refreshToken, g.expiresAt)
		return nil
	}
	if err := d.refreshWebToken(g); err != nil {
		return err
	}
	d.broadcastAccountToken(key, g.refreshToken, g.accessToken, g.expiresAt)
	return nil
}

// refreshWebToken redeems this storage's refresh_token and leaves the result in
// the receiver (and in g when non-nil). One attempt only: a rejected token is
// reported as-is instead of being papered over with a sibling's copy — siblings
// hold independently rotated snapshots, and picking one of theirs would consume
// a token the user cannot see being used.
// It takes no lock: the caller holds d.mu (g == nil) or g.mu and is responsible
// for persisting — broadcastAccountToken does it for the group mode so the row
// is written exactly once.
func (d *AliyundriveShare2Pan115) refreshWebToken(g *accountTokenGroup) error {
	access, refresh, expiresAt, err := requestWebToken(d.AliyunShare115Addition.RefreshToken)
	if err != nil {
		return err
	}
	d.setTokenFields(access, refresh, expiresAt)
	if g != nil {
		g.accessToken, g.refreshToken, g.expiresAt = access, refresh, expiresAt
	}
	return nil
}

// requestWebToken redeems rt at the official web endpoint — the xiaoya relay
// (oauth_token_url) and the open-token pair are deliberately unused.
func requestWebToken(rt string) (access, refresh string, expiresAt int64, err error) {
	if rt == "" {
		return "", "", 0, errors.New("missing refresh_token")
	}
	body := base.Json{
		"refresh_token": rt,
		"grant_type":    "refresh_token",
	}
	var resp aliWebTokenResp
	res, reqErr := base.RestyClient.R().
		SetBody(body).
		SetResult(&resp).
		Post(aliTokenURL)
	logResp("ali:refresh-token", res, reqErr)
	if reqErr != nil {
		return "", "", 0, reqErr
	}
	if resp.AccessToken == "" && res != nil {
		// same resty quirk as above: non-2xx bodies are not parsed for us
		_ = json.Unmarshal(res.Body(), &resp)
	}
	if resp.AccessToken == "" {
		status := 0
		if res != nil {
			status = res.StatusCode()
		}
		return "", "", 0,
			fmt.Errorf("token refresh rejected (HTTP %d, code %q): %s", status, resp.Code, resp.Message)
	}
	refresh = rt
	if resp.RefreshToken != "" {
		// refresh_token rotates on every use; keep the new one
		refresh = resp.RefreshToken
	}
	exp := resp.ExpiresIn
	if exp <= 0 {
		exp = 7200
	}
	if exp > 600 {
		exp -= int64(rand.Intn(600))
	}
	return resp.AccessToken, refresh, time.Now().Unix() + exp, nil
}

// broadcastAccountToken writes the refreshed token set into every storage row of
// the same account, so a restart does not trigger another round of refreshes.
// Sibling memory copies are left untouched on purpose: they pick the token up
// from the group table on their next ensureWebToken, which keeps this path free
// of cross-instance locking.
//
// Rows come from the database, not from the loaded drivers: a sibling's
// in-memory addition still holds the old JSON, and a storage that is currently
// disabled is loaded nowhere. One SELECT loads every row of this driver, then
// the matching ones are updated inside a single transaction (one fsync instead
// of one per row).
func (d *AliyundriveShare2Pan115) broadcastAccountToken(key, refresh, access string, expiresAt int64) {
	var rows []model.Storage
	if err := db.GetDb().Select("id", "addition").
		Where("driver = ?", aliyunShare115Config.Name).Find(&rows).Error; err != nil {
		log.Warnf("[xiaoya/115] read %s storages failed: %v", aliyunShare115Config.Name, err)
		return
	}
	type target struct {
		id  uint
		add map[string]interface{}
	}
	targets := make([]target, 0, len(rows))
	for _, row := range rows {
		add := map[string]interface{}{}
		if row.Addition != "" {
			if err := json.Unmarshal([]byte(row.Addition), &add); err != nil {
				log.Warnf("[xiaoya/115] storage #%d addition is not JSON: %v", row.ID, err)
				continue
			}
		}
		open, _ := add["RefreshTokenOpen"].(string)
		if tokenGroupKeyFor(open) != key {
			continue
		}
		add["RefreshToken"] = refresh
		add["access_token"] = access
		add["access_token_expires_at"] = expiresAt
		targets = append(targets, target{id: row.ID, add: add})
	}
	if len(targets) == 0 {
		return
	}
	err := db.GetDb().Transaction(func(tx *gorm.DB) error {
		for _, t := range targets {
			raw, err := json.Marshal(t.add)
			if err != nil {
				log.Warnf("[xiaoya/115] storage #%d addition marshal failed: %v", t.id, err)
				continue
			}
			if err := tx.Model(&model.Storage{}).Where("id = ?", t.id).
				Update("addition", string(raw)).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Warnf("[xiaoya/115] broadcast token to %d storage(s) failed: %v", len(targets), err)
		return
	}
	log.Infof("[xiaoya/115] account token refreshed, written to %d storage(s)", len(targets))
}

// share token gate: alipan allows roughly ten get_share_token calls per window
// per IP, so every storage in the process shares one pace. Without this, a
// library scan that touches dozens of storages at once just moves the 429 storm
// from Init to the first List.
const (
	shareTokenMinGap  = 1500 * time.Millisecond
	shareTokenTries   = 3
	shareTokenBackoff = 12 * time.Second
)

var (
	shareTokenGateMu sync.Mutex
	shareTokenNextAt time.Time
)

func shareTokenSlot() {
	shareTokenGateMu.Lock()
	defer shareTokenGateMu.Unlock()
	if wait := time.Until(shareTokenNextAt); wait > 0 {
		time.Sleep(wait)
	}
	shareTokenNextAt = time.Now().Add(shareTokenMinGap)
}

func (d *AliyundriveShare2Pan115) ensureShareToken() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.AliyunShare115Addition.ShareToken != "" && time.Now().Unix() < d.AliyunShare115Addition.ShareTokenExpires-60 {
		return nil
	}
	body := base.Json{"share_id": d.AliyunShare115Addition.ShareId}
	if d.AliyunShare115Addition.SharePwd != "" {
		body["share_pwd"] = d.AliyunShare115Addition.SharePwd
	}
	var lastErr error
	for attempt := 0; attempt < shareTokenTries; attempt++ {
		if attempt > 0 {
			wait := shareTokenBackoff * time.Duration(attempt)
			log.Warnf("[xiaoya/115] get_share_token rate limited, retry %d/%d in %s",
				attempt, shareTokenTries-1, wait)
			time.Sleep(wait)
		}
		shareTokenSlot()
		var resp aliShareTokenResp
		res, err := base.RestyClient.R().
			SetBody(body).
			SetResult(&resp).
			Post(aliShareTokenURL)
		logResp("ali:share-token", res, err)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.ShareToken == "" && res != nil {
			// resty skips SetResult on non-2xx responses, so a 429
			// {"code":"TooManyRequests","message":"too many requests"} or a
			// 400 InvalidResource.SharePwd arrives with an empty struct and
			// the reason would be lost -- parse the raw body instead.
			_ = json.Unmarshal(res.Body(), &resp)
		}
		if resp.ShareToken != "" {
			d.AliyunShare115Addition.ShareToken = resp.ShareToken
			exp := resp.ExpiresIn
			if exp <= 0 {
				exp = 7200
			}
			// jitter: keep a fleet of storages from expiring in the same
			// second and hitting the gate together two hours later
			if exp > 600 {
				exp -= int64(rand.Intn(600))
			}
			d.AliyunShare115Addition.ShareTokenExpires = time.Now().Unix() + exp
			d.save()
			return nil
		}
		status := 0
		if res != nil {
			status = res.StatusCode()
		}
		if status == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("share token rate limited (HTTP 429)")
			continue
		}
		// resty leaves Result empty on non-2xx, so the body has to be read
		// from the raw response or the reason is lost entirely
		return fmt.Errorf("share token rejected (HTTP %d, code %q): %s", status, resp.Code, resp.Message)
	}
	if lastErr == nil {
		lastErr = errors.New("share token unavailable")
	}
	return lastErr
}

func (d *AliyundriveShare2Pan115) ensureOwnDriveId() error {
	d.mu.Lock()
	if d.AliyunShare115Addition.OwnDriveId != "" {
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()
	if err := d.ensureWebToken(); err != nil {
		return err
	}
	var resp aliUserResp
	res, err := base.RestyClient.R().
		SetHeader("Authorization", "Bearer "+d.AliyunShare115Addition.AccessToken).
		SetBody(base.Json{}).
		SetResult(&resp).
		Post(aliUserGetURL)
	logResp("ali:user-get", res, err)
	if err != nil {
		return err
	}
	id := resp.ResourceDriveId
	if id == "" {
		id = resp.DefaultDriveId
	}
	if id == "" {
		id = resp.DriveId
	}
	if id == "" {
		return fmt.Errorf("cannot resolve own drive id: %s", resp.Message)
	}
	d.mu.Lock()
	d.AliyunShare115Addition.OwnDriveId = id
	d.mu.Unlock()
	d.save()
	return nil
}

// shareHeaders returns the odd-but-required header set of the share domain.
func shareHeaders(token, shareToken string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer\t" + token, // tab on purpose, matches upstream behavior
		"x-share-token": shareToken,
		"X-Canary":      canaryShare,
	}
}

func (d *AliyundriveShare2Pan115) listShare(ctx context.Context, parent, marker string) ([]aliFileItem, string, error) {
	if err := d.ensureWebToken(); err != nil {
		return nil, "", err
	}
	if err := d.ensureShareToken(); err != nil {
		return nil, "", err
	}
	var resp aliListResp
	res, err := base.RestyClient.R().
		SetHeaders(shareHeaders(d.AliyunShare115Addition.AccessToken, d.AliyunShare115Addition.ShareToken)).
		SetBody(base.Json{
			"share_id":        d.AliyunShare115Addition.ShareId,
			"parent_file_id":  parent,
			"limit":           aliPageSize,
			"marker":          marker,
			"order_by":        orDefault(d.AliyunShare115Addition.OrderBy, "name"),
			"order_direction": orDefault(d.AliyunShare115Addition.OrderDirection, "ASC"),
		}).
		SetResult(&resp).
		Post(aliShareListURL)
	logResp("ali:share-list", res, err)
	if err != nil {
		return nil, "", err
	}
	if resp.Message != "" && len(resp.Items) == 0 && resp.NextMarker == "" {
		return nil, "", errors.New(resp.Message)
	}
	if d.AliyunShare115Addition.ShareDriveId == "" {
		for _, it := range resp.Items {
			if it.DriveId != "" {
				d.AliyunShare115Addition.ShareDriveId = it.DriveId
				d.save()
				break
			}
		}
	}
	return resp.Items, resp.NextMarker, nil
}

// obtainSha1 copies the share file into TempTransferFolderID and polls until
// the copy reports content_hash. Sets lastCopiedFileId for the range proof.
func (d *AliyundriveShare2Pan115) obtainSha1(ctx context.Context, file model.Obj) (string, error) {
	if d.AliyunShare115Addition.TempTransferFolderID == "" {
		return "", errors.New("TempTransferFolderID is empty")
	}
	if err := d.ensureOwnDriveId(); err != nil {
		return "", err
	}
	// No pre-copy dedup lookup (aligned with rayburst): the copy API uses the
	// default check_name_mode=refuse, so when the file is already inside
	// TempTransferFolderID the copy is refused without creating a duplicate,
	// and the refusal branch below re-finds the existing file by name+size.
	// copy
	if d.AliyunShare115Addition.ShareDriveId == "" {
		items, _, lerr := d.listShare(ctx, d.rootID(), "")
		if lerr != nil {
			return "", lerr
		}
		for _, it := range items {
			if it.DriveId != "" {
				d.AliyunShare115Addition.ShareDriveId = it.DriveId
				d.save()
				break
			}
		}
		if d.AliyunShare115Addition.ShareDriveId == "" {
			return "", errors.New("cannot determine share drive_id")
		}
	}
	var copyResp aliCopyResp
	res, err := base.RestyClient.R().
		SetHeaders(shareHeaders(d.AliyunShare115Addition.AccessToken, d.AliyunShare115Addition.ShareToken)).
		SetBody(base.Json{
			"drive_id":          d.AliyunShare115Addition.ShareDriveId,
			"file_id":           file.GetID(),
			"share_id":          d.AliyunShare115Addition.ShareId,
			"to_drive_id":       d.AliyunShare115Addition.OwnDriveId,
			"to_parent_file_id": d.AliyunShare115Addition.TempTransferFolderID,
		}).
		SetResult(&copyResp).
		Post(aliCopyURL)
	logResp("ali:copy", res, err)
	if err != nil {
		return "", err
	}
	copied := copyResp.FileId
	if copied == "" && len(copyResp.Tasks) > 0 {
		copied = copyResp.Tasks[0].FileId
	}
	if copied == "" {
		// maybe it already exists (copy refused); try lookup again
		if sha1v, fid, ok, lerr := d.findOwnFile(ctx, file.GetName(), file.GetSize()); lerr == nil && ok {
			d.lastCopiedFileId = fid
			return strings.ToUpper(sha1v), nil
		}
		return "", fmt.Errorf("copy failed: %s", copyResp.Message)
	}
	d.lastCopiedFileId = copied

	// Ask for the copied file itself: /v2/file/get is O(1) and returns
	// content_hash as soon as the server-side copy lands, whereas the old poll
	// paged the whole destination folder (and slept a full second first).
	for i := 0; i < copyPollTries; i++ {
		if i > 0 {
			time.Sleep(copyPollDelay)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		sha1v, ok, lerr := d.ownFileHash(copied)
		if lerr != nil {
			log.Warnf("share2pan115: file/get failed: %v", lerr)
		} else if ok {
			return strings.ToUpper(sha1v), nil
		}
	}

	// Fallback: the copy id is unusable for this account (or hash still empty),
	// so scan the folder by name+size as before.
	if sha1v, fid, ok, lerr := d.findOwnFile(ctx, file.GetName(), file.GetSize()); lerr == nil && ok && sha1v != "" {
		d.lastCopiedFileId = fid
		return strings.ToUpper(sha1v), nil
	}
	return "", fmt.Errorf("copy of %s did not expose content_hash in time", file.GetName())
}

// ownFileHash returns content_hash of one of our own files (empty when the
// copy has not been indexed yet).
func (d *AliyundriveShare2Pan115) ownFileHash(fileId string) (string, bool, error) {
	var resp aliOwnFileResp
	res, err := base.RestyClient.R().
		SetHeaders(map[string]string{
			"Authorization": "Bearer " + d.AliyunShare115Addition.AccessToken,
			"X-Canary":      canaryOwn,
		}).
		SetBody(base.Json{"drive_id": d.AliyunShare115Addition.OwnDriveId, "file_id": fileId}).
		SetResult(&resp).
		Post(aliOwnFileURL)
	logResp("ali:file-get", res, err)
	if err != nil {
		return "", false, err
	}
	if resp.ContentHash == "" {
		return "", false, nil
	}
	return resp.ContentHash, true, nil
}

// findOwnFile scans TempTransferFolderID for a name+size match.
func (d *AliyundriveShare2Pan115) findOwnFile(ctx context.Context, name string, size int64) (sha1, fileId string, found bool, err error) {
	marker := ""
	for page := 0; page < 20; page++ {
		var resp aliListResp
		res, err := base.RestyClient.R().
			SetHeaders(map[string]string{
				"Authorization": "Bearer\t" + d.AliyunShare115Addition.AccessToken,
				"X-Canary":      canaryOwn,
			}).
			SetBody(base.Json{
				"drive_id":        d.AliyunShare115Addition.OwnDriveId,
				"parent_file_id":  d.AliyunShare115Addition.TempTransferFolderID,
				"limit":           aliPageSize,
				"marker":          marker,
				"order_by":        "name",
				"order_direction": "ASC",
			}).
			SetResult(&resp).
			Post(aliOwnListURL)
		logResp("ali:own-list", res, err)
		if err != nil {
			return "", "", false, err
		}
		for _, it := range resp.Items {
			if it.Name == name && it.Size == size && it.ContentHash != "" {
				return it.ContentHash, it.FileId, true, nil
			}
		}
		if resp.NextMarker == "" {
			break
		}
		marker = resp.NextMarker
	}
	return "", "", false, nil
}

// aliyunFallbackLink returns the (throttled) aliyun direct link when the 115
// path is unavailable.
func (d *AliyundriveShare2Pan115) aliyunFallbackLink(ctx context.Context, file model.Obj) (*model.Link, error) {
	if d.AliyunShare115Addition.TempTransferFolderID == "" {
		return nil, errors.New("aliyun fallback unavailable: TempTransferFolderID is empty")
	}
	_, fileId, ok, err := d.findOwnFile(ctx, file.GetName(), file.GetSize())
	if err != nil {
		return nil, err
	}
	if !ok {
		_, err = d.obtainSha1(ctx, file)
		if err != nil {
			return nil, err
		}
		fileId = d.lastCopiedFileId
	}
	if fileId == "" {
		return nil, errors.New("aliyun fallback: no copied file")
	}
	return d.aliyunDownloadLink(fileId)
}

func (d *AliyundriveShare2Pan115) aliyunDownloadLink(fileId string) (*model.Link, error) {
	var resp aliDownUrlResp
	res, err := base.RestyClient.R().
		SetHeaders(map[string]string{
			"Authorization": "Bearer " + d.AliyunShare115Addition.AccessToken,
			"X-Canary":      canaryOwn,
			"Referer":       "https://www.alipan.com/",
		}).
		SetBody(base.Json{"drive_id": d.AliyunShare115Addition.OwnDriveId, "file_id": fileId}).
		SetResult(&resp).
		Post(aliDownURL)
	logResp("ali:download-url", res, err)
	if err != nil {
		return nil, err
	}
	if resp.Url == "" {
		return nil, fmt.Errorf("aliyun download url failed: %s", resp.Message)
	}
	// A "callback=" query param means the URL points at the PDS download-audit
	// channel: the CDN walks a callback before serving the first byte, which is
	// what makes a 140 KB sign-proof read take seconds.
	if strings.Contains(resp.Url, "callback=") {
		log.Infof("[xiaoya/ali:download-url] link uses the callback channel (slow range reads)")
	}
	return &model.Link{
		URL: resp.Url,
		Header: http.Header{
			"Referer": []string{"https://www.alipan.com/"},
		},
	}, nil
}

// ---------- 115 side ----------

func (d *AliyundriveShare2Pan115) ensure115Client() (*driver115.Pan115Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		return d.client, nil
	}
	client := driver115.New(driver115.UA(ua115()), func(c *driver115.Pan115Client) {
		c.Client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true})
	})
	cr := &driver115.Credential{}
	if err := cr.FromCookie(d.AliyunShare115Addition.Cookie); err != nil {
		return nil, fmt.Errorf("invalid 115 cookie: %w", err)
	}
	client.ImportCredential(cr)
	if err := client.LoginCheck(); err != nil {
		return nil, fmt.Errorf("115 login check failed (cookie expired?): %w", err)
	}
	// userkey/userid are required to sign rapid-upload requests
	if err := client.GetUploadInfo(); err != nil {
		return nil, fmt.Errorf("115 upload info failed: %w", err)
	}
	d.client = client
	return client, nil
}

// ensureRecvCid returns the 115 flash directory: the addition's dir_id field
// (explicit in xiaoya storage rows). No auto-resolution — an empty dir_id
// makes the 115 path unavailable and Link falls back to the aliyun link.
func (d *AliyundriveShare2Pan115) ensureRecvCid() (string, error) {
	cid := strings.TrimSpace(d.AliyunShare115Addition.DirID)
	if cid == "" {
		return "", errors.New("dir_id (115 flash directory) is not configured")
	}
	return cid, nil
}

// findPickcodeByName looks up an existing 115 file by name+size inside dirID.
func findPickcodeByName(cookie, dirID, name string, size int64) (string, error) {
	if dirID == "" {
		return "", errors.New("empty dir id")
	}
	for offset := 0; offset <= maxOffset115; offset += page115 {
		res, err := base.RestyClient.R().
			SetHeader("Cookie", cookie).
			SetHeader("User-Agent", ua115()).
			SetHeader("Referer", "https://115.com/").
			SetQueryParams(map[string]string{
				"cid": dirID, "limit": strconv.Itoa(page115), "offset": strconv.Itoa(offset),
				"o": "user_ptime", "asc": "0", "fc_mix": "0", "format": "json",
			}).
			Get(webAPI115 + "/files")
		logResp("115:dir-files", res, err)
		if err != nil {
			return "", err
		}
		// 115 answers this endpoint with "Content-Type: text/html" while the body
		// is JSON, and resty's SetResult only deserialises known content types —
		// the struct would stay empty and every lookup looked "rejected".
		var list ali115FileListResp
		if err := json.Unmarshal(res.Body(), &list); err != nil {
			return "", fmt.Errorf("115 file list decode failed: %w", err)
		}
		if !list.State && len(list.Data) == 0 {
			return "", fmt.Errorf("115 file list rejected: errno=%d error=%q", list.ErrNo, list.Error)
		}
		for _, f := range list.Data {
			if f.Name == name && f.Size == size {
				return f.Pick, nil
			}
		}
		if len(list.Data) < page115 {
			break
		}
	}
	return "", nil
}

// rapidUpload runs the 115 flash-upload (秒传) with a remote SHA1. When 115
// answers status=7 it asks for a byte-range proof, which we serve from the
// aliyun copy's download URL.
func (d *AliyundriveShare2Pan115) rapidUpload(client *driver115.Pan115Client, name string, size int64, dirID, sha1Upper string) (string, error) {
	c, err := d.ensure115Client()
	if err != nil {
		return "", err
	}
	fileID := strings.ToUpper(sha1Upper)
	target := "U_1_" + dirID

	signKey, signVal := "", ""
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := d.init115Upload(c, name, size, target, fileID, signKey, signVal)
		if err != nil {
			return "", err
		}
		switch resp.Status {
		case 2:
			return resp.PickCode, nil
		case 7:
			// prove ownership of the bytes: read the requested range from our
			// aliyun copy and return its SHA1 (uppercase)
			signKey = resp.SignKey
			signVal, err = d.digestAliRange(resp.SignCheck)
			if err != nil {
				return "", fmt.Errorf("sign_check range read failed: %w", err)
			}
		default:
			return "", fmt.Errorf("115 initupload status=%d (no flash-upload match)", resp.Status)
		}
	}
	return "", errors.New("115 rapid upload did not succeed after sign_check")
}

// init115Upload performs the encrypted initupload.php call, mirroring the
// upstream drivers/115 rapidUpload implementation but reading the range proof
// from a remote aliyun URL instead of a local stream.
func (d *AliyundriveShare2Pan115) init115Upload(client *driver115.Pan115Client, name string, size int64, target, fileID, signKey, signVal string) (*driver115.UploadInitResp, error) {
	ecdhCipher, err := cipher.NewEcdhCipher()
	if err != nil {
		return nil, err
	}
	sizeStr := strconv.FormatInt(size, 10)
	appVer := latest115AppVer()
	form := url.Values{}
	form.Set("appid", "0")
	form.Set("appversion", appVer)
	form.Set("userid", strconv.FormatInt(client.UserID, 10))
	form.Set("filename", name)
	form.Set("filesize", sizeStr)
	form.Set("fileid", fileID)
	form.Set("target", target)
	form.Set("sig", client.GenerateSignature(fileID, target))
	form.Set("topupload", "true")

	t := time.Now().UnixMilli()
	encodedToken, err := ecdhCipher.EncodeToken(t)
	if err != nil {
		return nil, err
	}
	form.Set("t", strconv.FormatInt(t, 10))
	form.Set("token", token115(client.UserID, fileID, sizeStr, signKey, signVal, strconv.FormatInt(t, 10), appVer))
	if signKey != "" && signVal != "" {
		form.Set("sign_key", signKey)
		form.Set("sign_val", signVal)
	}

	encrypted, err := ecdhCipher.Encrypt([]byte(form.Encode()))
	if err != nil {
		return nil, err
	}
	resp, err := client.NewRequest().
		SetQueryParams(map[string]string{"k_ec": encodedToken}).
		SetBody(encrypted).
		SetHeaderVerbatim("Content-Type", "application/x-www-form-urlencoded").
		SetDoNotParseResponse(true).
		Post(driver115.ApiUploadInit)
	if err != nil {
		logResp("115:init-upload", nil, err)
		return nil, err
	}
	status := 0
	if resp.RawResponse != nil {
		status = resp.RawResponse.StatusCode
	}
	defer resp.RawBody().Close()
	bodyBytes, err := io.ReadAll(resp.RawBody())
	if err != nil {
		logBodyInfo("115:init-upload", "POST", driver115.ApiUploadInit, status, "body read error: "+err.Error())
		return nil, err
	}
	decrypted, err := ecdhCipher.Decrypt(bodyBytes)
	if err != nil {
		logBodyInfo("115:init-upload", "POST", driver115.ApiUploadInit, status, "decrypt error: "+err.Error())
		return nil, err
	}
	logBodyInfo("115:init-upload", "POST", driver115.ApiUploadInit, status, string(decrypted))
	result := driver115.UploadInitResp{}
	if err := json.Unmarshal(decrypted, &result); err != nil {
		return nil, err
	}
	// status 2 (hit) and 7 (sign_check round) are both normal: 7 still carries
	// statusmsg "sig invalid" / statuscode 701, so UploadInitResp.Err() must not
	// be treated as fatal here or the sign_check round never happens.
	switch result.Status {
	case 2, 7:
	default:
		if result.ErrorMsg != "" {
			return nil, fmt.Errorf("115 initupload rejected: status=%d code=%d msg=%s",
				result.Status, result.ErrorCode, result.ErrorMsg)
		}
		return nil, fmt.Errorf("115 initupload failed: status=%d code=%d", result.Status, result.ErrorCode)
	}
	return &result, nil
}

// token115 reimplements 115driver's GenerateToken, which hashes the SDK's
// hardcoded appVer ("27.0.5.7") into the digest. 115 now rejects that version
// (statuscode 403 "请升级到最新版本") and also expects the token to be signed
// with the same appversion we submit, so the version has to be a parameter.
func token115(userID int64, fileID, fileSize, signKey, signVal, timeStamp, appVer string) string {
	uid := strconv.FormatInt(userID, 10)
	uidMD5 := md5.Sum([]byte(uid))
	sum := md5.Sum([]byte(md5Salt115 + fileID + fileSize + signKey + signVal + uid + timeStamp + hex.EncodeToString(uidMD5[:]) + appVer))
	return hex.EncodeToString(sum[:])
}

// digestAliRange fetches "start-end" bytes of our aliyun copy and hashes them.
func (d *AliyundriveShare2Pan115) digestAliRange(rangeSpec string) (string, error) {
	var start, end int64
	if _, err := fmt.Sscanf(rangeSpec, "%d-%d", &start, &end); err != nil {
		return "", err
	}
	if d.lastCopiedFileId == "" {
		return "", errors.New("no copied file for range proof")
	}
	dl, err := d.aliyunDownloadLink(d.lastCopiedFileId)
	if err != nil {
		return "", err
	}
	tURL := time.Now()
	req, _ := http.NewRequest(http.MethodGet, dl.URL, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	// ask for the exact bytes: a gzip/br attempt on a range request only adds a
	// round trip and breaks the byte mapping
	req.Header.Set("Accept-Encoding", "identity")
	if dl.Header.Get("Referer") != "" {
		req.Header.Set("Referer", dl.Header.Get("Referer"))
	}
	res, err := base.HttpClient.Do(req)
	if err != nil {
		log.Infof("[xiaoya/ali:range-proof] GET %s error: %v", shortURL(dl.URL), err)
		return "", err
	}
	defer res.Body.Close()
	tBody := time.Now()
	// 206 only: a 200 means the server ignored Range and started streaming the
	// whole file, which would hash the wrong bytes and pull gigabytes we never
	// asked for.
	if res.StatusCode != 206 {
		log.Infof("[xiaoya/ali:range-proof] GET %s -> %d", shortURL(dl.URL), res.StatusCode)
		return "", fmt.Errorf("range read status %d", res.StatusCode)
	}
	want := end - start + 1
	h := sha1.New()
	n, err := io.Copy(h, io.LimitReader(res.Body, want))
	if err != nil {
		return "", err
	}
	if n != want {
		return "", fmt.Errorf("short range read: got %d of %d bytes", n, want)
	}
	log.Infof("[xiaoya/ali:range-proof] GET %s -> %d bytes=%d ttfb=%dms read=%dms (range %d-%d)",
		shortURL(dl.URL), res.StatusCode, n, tBody.Sub(tURL).Milliseconds(), time.Since(tBody).Milliseconds(), start, end)
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

// shortURL keeps logs readable: the pre-signed aliyun URL is ~1.5 KB.
func shortURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	p := u.Path
	if len(p) > 24 {
		p = p[:24] + "…"
	}
	return u.Host + p
}

func (d *AliyundriveShare2Pan115) downlink(client *driver115.Pan115Client, pick, ua string) (*model.Link, error) {
	// The plain web endpoint answers with JSON and, more importantly, hands
	// back the WAF cookies it plants (acw_tc + a rotating 32-hex one). The
	// app/chrome endpoint encrypts the body and keeps those in the SDK jar,
	// where alist's proxy request cannot see them.
	if l, err := d.downlinkWebAPI(client, pick, ua); err == nil {
		return l, nil
	} else {
		log.Infof("[xiaoya/115:downurl] webapi pickcode %s failed: %v (fallback to sdk)", pick, err)
	}
	info, err := client.DownloadWithUA(pick, ua)
	if err != nil {
		log.Infof("[xiaoya/115:downurl] pickcode %s error: %v", pick, err)
		return nil, err
	}
	log.Infof("[xiaoya/115:downurl] pickcode %s -> %s", pick, info.Url.Url)
	header := info.Header
	if header == nil {
		header = http.Header{}
	}
	// A bare account cookie is not enough: the CDN answers 403 "no cookie
	// value" without the WAF cookies, and it binds them to the UA the link was
	// requested with.
	header.Set("Cookie", mergeSetCookie(d.AliyunShare115Addition.Cookie, nil))
	header.Set("User-Agent", ua)
	return &model.Link{URL: info.Url.Url, Header: header}, nil
}

// downlinkWebAPI asks webapi.115.com/files/download for the link. One round
// trip yields both the url and the Set-Cookie values the CDN requires.
func (d *AliyundriveShare2Pan115) downlinkWebAPI(client *driver115.Pan115Client, pick, ua string) (*model.Link, error) {
	resp, err := client.NewRequest().
		SetQueryParam("pickcode", pick).
		SetHeader("Cookie", d.AliyunShare115Addition.Cookie).
		SetHeader("User-Agent", ua).
		Get(webAPI115 + "/files/download")
	if err != nil {
		return nil, err
	}
	var r struct {
		State   bool   `json:"state"`
		FileURL string `json:"file_url"`
		ErrNo   int    `json:"errno"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(resp.Body(), &r); err != nil {
		return nil, err
	}
	if !r.State || r.FileURL == "" {
		return nil, fmt.Errorf("state=%v errno=%d error=%s", r.State, r.ErrNo, r.Error)
	}
	cookie := mergeSetCookie(d.AliyunShare115Addition.Cookie, resp.Header().Values("Set-Cookie"))
	return &model.Link{
		URL: r.FileURL,
		Header: http.Header{
			"Cookie":     {cookie},
			"User-Agent": {ua},
		},
	}, nil
}

// mergeSetCookie flattens the account cookie and the Set-Cookie values from
// the download response into one Cookie header. Later values win, because the
// server-issued acw_tc must replace any stale copy carried by the account.
func mergeSetCookie(account string, setCookies []string) string {
	order := make([]string, 0, len(setCookies)+4)
	values := map[string]string{}
	add := func(kv string) {
		i := strings.Index(kv, "=")
		if i <= 0 {
			return
		}
		name := strings.TrimSpace(kv[:i])
		if name == "" {
			return
		}
		if _, ok := values[name]; !ok {
			order = append(order, name)
		}
		values[name] = strings.TrimSpace(kv[i+1:])
	}
	for _, p := range strings.Split(account, ";") {
		add(p)
	}
	for _, sc := range setCookies {
		add(strings.SplitN(sc, ";", 2)[0])
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		if v := values[name]; v != "" {
			parts = append(parts, name+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// resty import guard: base.RestyClient is used through R() chains above.
var _ *resty.Client = base.RestyClient
