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
	Name:              "QuarkShare",
	LocalSort:         false,
	OnlyProxy:         false,
	NoUpload:          true,
	DefaultRoot:       "0",
	NoOverwriteUpload: true,
	PersistCache:      "on",
}
