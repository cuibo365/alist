package handles

import (
	"time"

	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type RefreshCacheReq struct {
	Path string `json:"path" binding:"required"`
}

type RefreshCacheResp struct {
	Deleted int64  `json:"deleted"`
	Rebuilt bool   `json:"rebuilt"`
	TookMS  int64  `json:"took_ms"`
	Error   string `json:"error,omitempty"`
}

// RefreshCache drops the listing cache of a path and of everything below it,
// then rebuilds only that single level.
//
// Admin only, and deliberately not folded into /api/fs/list: purging a subtree
// forces *everyone* to hit the upstream again, while the list endpoint's
// refresh flag is available to anyone with write permission.
func RefreshCache(c *gin.Context) {
	var req RefreshCacheReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	path := utils.FixAndCleanPath(req.Path)
	if path == "/" {
		common.ErrorStrResp(c, "refusing to refresh the virtual root, pass a storage mount path instead", 400)
		return
	}
	// Resolve first so an unknown path is reported as a client error instead of
	// looking like a successful purge with nothing rebuilt.
	if _, _, err := op.GetStorageAndActualPath(path); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	start := time.Now()
	deleted, rebuilt, err := op.RefreshListCache(c, path)
	resp := RefreshCacheResp{
		Deleted: deleted,
		Rebuilt: rebuilt,
		TookMS:  time.Since(start).Milliseconds(),
	}
	if err != nil {
		// The purge part already happened; report it rather than pretending
		// nothing was done. The next visit simply lists from the source.
		resp.Error = err.Error()
		log.Warnf("[cache] refresh %s: purged %d, rebuild failed: %v", path, deleted, err)
	} else {
		log.Infof("[cache] refresh %s: purged %d, rebuilt in %dms", path, deleted, resp.TookMS)
	}
	common.SuccessResp(c, resp)
}
