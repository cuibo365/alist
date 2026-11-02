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
}

var quarkShareConfig = driver.Config{
	Name:              "QuarkShare",
	LocalSort:         false,
	OnlyProxy:         false,
	NoUpload:          true,
	DefaultRoot:       "0",
	NoOverwriteUpload: true,
}
