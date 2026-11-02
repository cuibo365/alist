package xiaoya

import (
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/op"
)

// Driver registration. Names must match the `driver` column of xiaoya
// storage rows exactly (QuarkShare / AliyundriveShare2Pan115).

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &QuarkShare{}
	})
	op.RegisterDriver(func() driver.Driver {
		return &AliyundriveShare2Pan115{}
	})
}
