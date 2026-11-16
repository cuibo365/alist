package bootstrap

import (
	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/listcache"
)

// InitListCache opens the persistent list cache (data/cache.db). Failure only
// disables the disk layer, so it never blocks startup.
func InitListCache() {
	listcache.Init(flags.DataDir)
}
