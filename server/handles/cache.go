package handles

import (
	"fmt"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type RefreshCacheReq struct {
	Path string `json:"path" form:"path" binding:"required"`
}

type RefreshCacheResp struct {
	Deleted int64  `json:"deleted"`
	Rebuilt bool   `json:"rebuilt"`
	TookMS  int64  `json:"took_ms"`
	Error   string `json:"error,omitempty"`
}

// pathFromRequest reads the path from ?path= first and falls back to the body,
// so the same endpoint works from curl (query) and from the panel (JSON).
func pathFromRequest(c *gin.Context) (string, error) {
	if p := c.Query("path"); p != "" {
		return p, nil
	}
	var req RefreshCacheReq
	if err := c.ShouldBind(&req); err != nil {
		return "", err
	}
	return req.Path, nil
}

// RefreshCache drops the listing cache of a path and of everything below it,
// then rebuilds only that single level.
//
// Admin only, and deliberately not folded into /api/fs/list: purging a subtree
// forces *everyone* to hit the upstream again, while the list endpoint's
// refresh flag is available to anyone with write permission.
func RefreshCache(c *gin.Context) {
	rawPath, err := pathFromRequest(c)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	path := utils.FixAndCleanPath(rawPath)
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
	deleted, rebuilt, err := fs.RefreshListCache(c, path)
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
	if wantsText(c) {
		var b strings.Builder
		fmt.Fprintf(&b, "purged %d row(s), rebuilt=%v, %dms\n", resp.Deleted, resp.Rebuilt, resp.TookMS)
		if resp.Error != "" {
			fmt.Fprintf(&b, "rebuild failed: %s\n", resp.Error)
		}
		c.String(200, b.String())
		return
	}
	common.SuccessResp(c, resp)
}

type InspectCacheResp struct {
	Path       string                `json:"path"`
	Count      int                   `json:"count"`
	TotalBytes int64                 `json:"total_bytes"`
	Rows       []driver.ListCacheRow `json:"rows"`
}

// InspectCache lists the on-disk listing cache of a path and of every path
// below it. Forwarding drivers (Alias) are followed down to the storages that
// actually own the rows, so a mount path reports what it would really serve.
//
// Read-only: unlike /cache/refresh nothing is dropped and nothing is rebuilt.
func InspectCache(c *gin.Context) {
	rawPath, err := pathFromRequest(c)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	path := utils.FixAndCleanPath(rawPath)
	if path == "/" {
		common.ErrorStrResp(c, "refusing to inspect the virtual root, pass a storage mount path instead", 400)
		return
	}
	if _, _, err := op.GetStorageAndActualPath(path); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	rows := fs.InspectCache(c, path)
	resp := InspectCacheResp{Path: path, Count: len(rows), Rows: rows}
	for _, r := range rows {
		resp.TotalBytes += r.Size
	}
	log.Infof("[cache] inspect %s: %d row(s), %s", path, resp.Count, humanBytes(resp.TotalBytes))
	if wantsText(c) {
		c.String(200, formatCacheRows(resp))
		return
	}
	common.SuccessResp(c, resp)
}

func wantsText(c *gin.Context) bool {
	f := c.Query("format")
	return f == "text" || f == "plain"
}

// formatCacheRows prints one row per line with the path last, so a Chinese
// folder name never has to line up with a column to its right.
func formatCacheRows(resp InspectCacheResp) string {
	var b strings.Builder
	fmt.Fprintf(&b, "path  : %s\n", resp.Path)
	fmt.Fprintf(&b, "rows  : %d\n", resp.Count)
	fmt.Fprintf(&b, "payload: %s\n", humanBytes(resp.TotalBytes))
	for _, r := range resp.Rows {
		fmt.Fprintf(&b, "  %s  %9s  st=%-5d  %-8s  %s\n",
			time.Unix(r.UpdatedAt, 0).Format("2006-01-02 15:04"),
			humanBytes(r.Size),
			r.StorageID,
			r.Driver,
			r.Path,
		)
	}
	return b.String()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
