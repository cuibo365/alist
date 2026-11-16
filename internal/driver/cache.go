package driver

import "github.com/alist-org/alist/v3/internal/model"

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
