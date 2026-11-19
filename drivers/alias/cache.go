package alias

import (
	"context"
	stdpath "path"
	"sort"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/pkg/utils"
	log "github.com/sirupsen/logrus"
)

// This file makes the mount-path based cache tools see through the alias.
//
// Alias never owns a cache row: NoCache is on and List only fans out to the
// targets, so the rows carry the storage_id of the target storages. Anything
// keyed on the Alias storage id therefore finds nothing, which is why the admin
// endpoints reported an empty/zero result before these two methods existed.
//
// Both translate the sub path with getRootAndPath and recurse through the fs
// helpers, so a chain of aliases is followed as well; the hop counter carried in
// ctx stops a mount that ends up pointing at itself.

// PurgeListCache drops the listing cache of every storage this mount forwards
// to, for the given sub path.
func (d *Alias) PurgeListCache(ctx context.Context, path string) int64 {
	if utils.PathEqual(path, "/") && !d.autoFlatten {
		// The mount root merges several keys, so purge the root of every target.
		var total int64
		for _, dsts := range d.pathMap {
			for _, dst := range dsts {
				total += fs.PurgeCache(ctx, dst)
			}
		}
		if total > 0 {
			log.Infof("[cache] alias purge /: %d row(s) under %d key(s)", total, len(d.pathMap))
		}
		return total
	}

	root, sub := d.getRootAndPath(path)
	dsts, ok := d.pathMap[root]
	if !ok {
		return 0
	}
	var total int64
	for _, dst := range dsts {
		total += fs.PurgeCache(ctx, stdpath.Join(dst, sub))
	}
	log.Infof("[cache] alias purge %s: %d row(s) across %d target(s) of key %q", path, total, len(dsts), root)
	return total
}

// InspectListCache reports the persisted listings of every storage this mount
// forwards to, merged and sorted by mount path then by path.
func (d *Alias) InspectListCache(ctx context.Context, path string) []driver.ListCacheRow {
	var rows []driver.ListCacheRow
	if utils.PathEqual(path, "/") && !d.autoFlatten {
		for _, dsts := range d.pathMap {
			for _, dst := range dsts {
				rows = append(rows, fs.InspectCache(ctx, dst)...)
			}
		}
	} else {
		root, sub := d.getRootAndPath(path)
		for _, dst := range d.pathMap[root] {
			rows = append(rows, fs.InspectCache(ctx, stdpath.Join(dst, sub))...)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].MountPath != rows[j].MountPath {
			return rows[i].MountPath < rows[j].MountPath
		}
		return rows[i].Path < rows[j].Path
	})
	return rows
}
