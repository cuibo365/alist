// Addition structs and driver configs for the xiaoya-compatible drivers.
//
// The JSON tags must stay byte-compatible with the addition blobs stored in a
// xiaoya instance's database, so synced storages work without edits.
package xiaoya

import (
	"github.com/alist-org/alist/v3/internal/driver"
)

// QuarkAddition is the addition for the QuarkShare driver.
type QuarkAddition struct {
	Cookie string `json:"cookie" required:"true"`
	driver.RootID
	OrderBy        string `json:"order_by" type:"select" options:"file_type,file_name,name,updated_at" default:"file_name"`
	OrderDirection string `json:"order_direction" type:"select" options:"asc,desc" default:"asc"`
	ShareId        string `json:"share_id" required:"true"`
	PassCode       string `json:"pass_code"`
	ShareToken     string `json:"share_token"` // runtime cache
	// Ranged-download tuning for the proxy path. Quark share links are signed
	// against the cookie of the request that created them, so a WebDAV client
	// cannot fetch them directly and alist proxies them; without these the
	// proxy falls back to one connection. 0 means "use the default" so rows
	// synced from a xiaoya database get the acceleration without edits.
	DownConcurrency int `json:"down_concurrency" type:"number" default:"3" help:"concurrency of ranged download when proxying, 0 = default (3), negative to disable"`
	DownPartSize    int `json:"down_part_size" type:"number" default:"10" help:"part size (MB) of ranged download, 0 = default (10)"`
}

var quarkShareConfig = driver.Config{
	Name:      "QuarkShare",
	LocalSort: false,
	// A Quark share download URL is signed against the cookie of the request
	// that minted it, and the CDN also wants the matching UA and Referer. A
	// 302 hands the client a bare URL, so browsers and players get 412/403
	// and WebDAV clients send none of the headers at all. Serving the link
	// directly only ever worked for the rare file whose URL happened to be
	// public, so the storage always proxies: the headers, the ranged
	// download tuning and the cookie stay on this side.
	OnlyProxy:         true,
	NoUpload:          true,
	DefaultRoot:       "0",
	NoOverwriteUpload: true,
	PersistCache:      "on",
}
