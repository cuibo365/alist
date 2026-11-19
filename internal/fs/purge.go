package fs

import (
	"context"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	log "github.com/sirupsen/logrus"
)

// forwardHopsKey counts how many forwarding drivers a cache operation has
// already walked through. Without it an Alias that points back at itself
// (directly or through another Alias) would recurse until the stack gives out.
type forwardHopsKey struct{}

const maxForwardHops = 8

// PurgeCache drops the cached listings of a mount path and of every path below
// it, and reports how many on-disk rows were removed.
//
// Unlike op.PurgeCache, which works on a resolved storage, this one starts from
// a mount path and follows forwarding drivers. An Alias mount owns no cache row
// of its own -- its List only fans out to the targets -- so purging it by
// storage id deletes nothing while the rows that keep being served live under
// the target storages' ids.
func PurgeCache(ctx context.Context, path string) int64 {
	hops, _ := ctx.Value(forwardHopsKey{}).(int)
	if hops >= maxForwardHops {
		log.Warnf("[cache] purge %s: forwarding chain longer than %d hops, stopping", path, maxForwardHops)
		return 0
	}
	storage, actualPath, err := op.GetStorageAndActualPath(path)
	if err != nil {
		log.Warnf("[cache] purge %s: %v", path, err)
		return 0
	}
	if purger, ok := storage.(driver.ListCachePurger); ok {
		return purger.PurgeListCache(context.WithValue(ctx, forwardHopsKey{}, hops+1), actualPath)
	}
	return op.PurgeCache(storage, actualPath)
}

// InspectCache reports the persisted listings of a mount path and of every path
// below it, following forwarding drivers the same way PurgeCache does.
//
// Rows are returned per storage: the "path" of a row is relative to the storage
// that owns it, and MountPath says which storage that is.
func InspectCache(ctx context.Context, path string) []driver.ListCacheRow {
	hops, _ := ctx.Value(forwardHopsKey{}).(int)
	if hops >= maxForwardHops {
		log.Warnf("[cache] inspect %s: forwarding chain longer than %d hops, stopping", path, maxForwardHops)
		return nil
	}
	storage, actualPath, err := op.GetStorageAndActualPath(path)
	if err != nil {
		log.Warnf("[cache] inspect %s: %v", path, err)
		return nil
	}
	if inspector, ok := storage.(driver.ListCacheInspector); ok {
		return inspector.InspectListCache(context.WithValue(ctx, forwardHopsKey{}, hops+1), actualPath)
	}
	return op.InspectListCache(storage, actualPath)
}

// RefreshListCache purges path and everything below it, then rebuilds only that
// one level. Descendants stay cold on purpose: once the parent is cached again,
// opening a child costs a single listing instead of a walk up the whole path.
func RefreshListCache(ctx context.Context, rawPath string) (int64, bool, error) {
	storage, actualPath, err := op.GetStorageAndActualPath(rawPath)
	if err != nil {
		return 0, false, err
	}
	deleted := PurgeCache(ctx, rawPath)
	if _, err := op.List(ctx, storage, actualPath, model.ListArgs{Refresh: true, ReqPath: rawPath}); err != nil {
		return deleted, false, err
	}
	return deleted, true, nil
}
