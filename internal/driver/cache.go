package driver

import (
	"context"

	"github.com/alist-org/alist/v3/internal/model"
)

// ListCacheItem is a single directory entry handed to a driver's list-cache
// codec. Obj is already unwrapped (ObjWrapName / ObjWrapStorageClass removed)
// and Name is the display name, so a codec never has to deal with wrappers.
type ListCacheItem struct {
	Name string
	Obj  model.Obj
}

// ListCacheCodec lets a driver persist a directory listing to disk.
//
// It is optional on purpose: a driver that does not implement it keeps the
// memory-only behaviour, so turning the disk layer on never changes how an
// unmodified driver behaves.
//
// DecodeObjs must return objects whose GetName() equals the Name the decoder
// was given, because callers look entries up by name. Any extra field the
// driver needs to build a download link (for example a share fid token) has to
// survive the round trip as well.
type ListCacheCodec interface {
	EncodeObjs(items []ListCacheItem) ([]byte, error)
	DecodeObjs(data []byte) ([]model.Obj, error)
}

// ListCachePurger is implemented by drivers that do not own the listings they
// serve. Alias is the case that matters: it forwards List to other mounts, so
// the cache rows carry the storage_id of those mounts and a purge keyed on the
// forwarding driver's own id removes nothing.
//
// path has the same shape List receives -- relative to this driver's mount --
// and the implementation is expected to translate it and recurse through
// fs.PurgeCache so that a chain of forwarders is followed all the way down.
// Returning 0 without touching anything is fine when the path cannot be mapped.
type ListCachePurger interface {
	PurgeListCache(ctx context.Context, path string) int64
}

// ListCacheRow is one persisted listing, as reported by ListCacheInspector.
// Path is relative to the storage that owns the row, not a mount path.
type ListCacheRow struct {
	StorageID uint   `json:"storage_id"`
	MountPath string `json:"mount_path"`
	Driver    string `json:"driver,omitempty"`
	Path      string `json:"path"`
	// Size is the length of the encoded payload in bytes, not the size of the
	// files it describes.
	Size      int64 `json:"size"`
	UpdatedAt int64 `json:"updated_at"`
}

// ListCacheInspector reports the on-disk list cache entries of a path and of
// every path below it. It exists for the same reason as ListCachePurger: a
// forwarding driver (Alias) holds no rows of its own, so inspecting it by
// storage id always comes back empty while the real rows sit under the storages
// it points at.
type ListCacheInspector interface {
	InspectListCache(ctx context.Context, path string) []ListCacheRow
}
