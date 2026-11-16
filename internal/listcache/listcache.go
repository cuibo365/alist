// Package listcache persists directory listings in a standalone sqlite file so
// that a restart does not force every already-visited directory to be listed
// again.
//
// The design is deliberately minimal, matching the agreed requirements:
//
//   - No TTL and no generation column. A row lives until it is purged: by a
//     write operation (ClearCache), by saving or deleting a storage, by the
//     admin refresh API, or by the read-time 404 fallback in op.Get.
//   - One row per (storage_id, path), overwritten in place. Renaming a mount
//     path therefore keeps the cache; editing the storage clears it.
//   - Writes are encoded on the caller side (so a job never holds a live
//     object) and committed asynchronously by a single goroutine.
//   - Only drivers implementing driver.ListCacheCodec are persisted. Everyone
//     else keeps the memory-only behaviour.
package listcache

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	log "github.com/sirupsen/logrus"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

const (
	// maxObjsPerDir keeps a single huge directory (thousands of episodes) from
	// bloating the cache file in one go.
	maxObjsPerDir = 2000
	writerQueue   = 1024
	writerBatch   = 32
)

type row struct {
	StorageID uint   `gorm:"column:storage_id;primaryKey;autoIncrement:false"`
	Path      string `gorm:"column:path;primaryKey"`
	Objs      []byte `gorm:"column:objs"`
	Size      int64  `gorm:"column:size"`
	UpdatedAt int64  `gorm:"column:updated_at"`
}

func (row) TableName() string {
	return "x_list_cache"
}

var (
	once    sync.Once
	db      *gorm.DB
	enabled bool
	jobs    chan row
)

// Init opens (or creates) the cache database. Any failure disables the disk
// layer for the whole process instead of blocking startup: a missing cache is
// only a performance regression, never a correctness problem.
func Init(dataDir string) {
	once.Do(func() {
		path := filepath.Join(dataDir, "cache.db")
		dsn := fmt.Sprintf("file:%s?_journal=WAL&_busy_timeout=5000&_synchronous=NORMAL", path)
		gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			log.Errorf("[listcache] open %s failed, disk list cache disabled: %v", path, err)
			return
		}
		if err := gdb.AutoMigrate(&row{}); err != nil {
			log.Errorf("[listcache] migrate failed, disk list cache disabled: %v", err)
			return
		}
		db = gdb
		enabled = true
		jobs = make(chan row, writerQueue)
		go writer()
		log.Infof("[listcache] disk list cache: %s", path)
	})
}

// Enabled reports whether the disk layer is usable.
func Enabled() bool {
	return enabled
}

func cacheable(storage driver.Driver) bool {
	s := storage.GetStorage()
	return s != nil && !storage.Config().NoCache && s.CacheExpiration > 0
}

func codecOf(storage driver.Driver) (driver.ListCacheCodec, bool) {
	c, ok := storage.(driver.ListCacheCodec)
	return c, ok
}

// Get returns a persisted listing, if the driver supports it and the entry
// exists. Callers are responsible for refilling the memory layer.
func Get(storage driver.Driver, path string) ([]model.Obj, bool) {
	if !enabled || !cacheable(storage) {
		return nil, false
	}
	codec, ok := codecOf(storage)
	if !ok {
		return nil, false
	}
	var r row
	err := db.Where("storage_id = ? AND path = ?", storage.GetStorage().ID, path).Take(&r).Error
	if err != nil {
		return nil, false
	}
	objs, err := codec.DecodeObjs(r.Objs)
	if err != nil {
		log.Warnf("[listcache] decode %s failed, dropping the row: %v", path, err)
		Delete(storage, path)
		return nil, false
	}
	return objs, true
}

// Set persists a listing. It never blocks on the database: the payload is
// encoded here (so the queued job holds no live objects) and committed by the
// writer goroutine.
func Set(storage driver.Driver, path string, objs []model.Obj) {
	if !enabled || len(objs) == 0 || !cacheable(storage) {
		return
	}
	codec, ok := codecOf(storage)
	if !ok {
		return
	}
	if len(objs) > maxObjsPerDir {
		log.Debugf("[listcache] skip %s: %d entries > %d", path, len(objs), maxObjsPerDir)
		Delete(storage, path)
		return
	}
	items := make([]driver.ListCacheItem, 0, len(objs))
	for _, o := range objs {
		items = append(items, driver.ListCacheItem{Name: o.GetName(), Obj: model.UnwrapObj(o)})
	}
	data, err := codec.EncodeObjs(items)
	if err != nil {
		log.Warnf("[listcache] encode %s failed: %v", path, err)
		return
	}
	select {
	case jobs <- row{
		StorageID: storage.GetStorage().ID,
		Path:      path,
		Objs:      data,
		Size:      int64(len(data)),
		UpdatedAt: time.Now().Unix(),
	}:
	default:
		log.Debugf("[listcache] writer queue full, dropping %s", path)
	}
}

// Delete removes one entry. Synchronous: it is used on the read path, where a
// stale row must be gone before the next request arrives.
func Delete(storage driver.Driver, path string) {
	if !enabled {
		return
	}
	if err := db.Where("storage_id = ? AND path = ?", storage.GetStorage().ID, path).
		Delete(&row{}).Error; err != nil {
		log.Warnf("[listcache] delete %s failed: %v", path, err)
	}
}

// DeletePrefix removes path and every descendant of it, returning how many rows
// were dropped.
//
// The descendant match is a byte range (path+"/" .. path+"0") rather than a
// LIKE, so it needs no escaping and stays index friendly even when folder names
// contain '%' or '_'.
func DeletePrefix(storage driver.Driver, path string) int64 {
	if !enabled {
		return 0
	}
	id := storage.GetStorage().ID
	q := db.Where("storage_id = ?", id)
	if path != "" && path != "/" {
		q = q.Where("path = ? OR (path >= ? AND path < ?)", path, path+"/", path+"0")
	}
	res := q.Delete(&row{})
	if res.Error != nil {
		log.Warnf("[listcache] purge %s failed: %v", path, res.Error)
		return 0
	}
	return res.RowsAffected
}

// DeleteStorage removes every row of a storage. Needed because sqlite may reuse
// an id after the row is deleted, which would otherwise hand a new storage the
// old storage's listings.
func DeleteStorage(storageID uint) int64 {
	if !enabled {
		return 0
	}
	res := db.Where("storage_id = ?", storageID).Delete(&row{})
	if res.Error != nil {
		log.Warnf("[listcache] purge storage %d failed: %v", storageID, res.Error)
		return 0
	}
	return res.RowsAffected
}

func writer() {
	for {
		first := <-jobs
		batch := make([]row, 0, writerBatch)
		batch = append(batch, first)
	fill:
		for len(batch) < writerBatch {
			select {
			case j := <-jobs:
				batch = append(batch, j)
			default:
				break fill
			}
		}
		if err := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&batch).Error; err != nil {
			log.Warnf("[listcache] persist %d rows failed: %v", len(batch), err)
		}
	}
}

// ObjectDTO is the on-disk shape shared by drivers whose listings are plain
// *model.Object. Drivers with extra fields embed it.
type ObjectDTO struct {
	ID       string `json:"id,omitempty"`
	Path     string `json:"p,omitempty"`
	Name     string `json:"n"`
	Size     int64  `json:"s,omitempty"`
	Modified int64  `json:"m,omitempty"`
	Ctime    int64  `json:"c,omitempty"`
	IsFolder bool   `json:"d,omitempty"`
	Hash     string `json:"h,omitempty"`
}

// NewObjectDTO converts an object plus its display name. Kept separate from
// NewObjectDTOFromItem so drivers whose objects embed model.Object can reuse it.
func NewObjectDTO(o *model.Object, name string) ObjectDTO {
	dto := ObjectDTO{
		ID:       o.ID,
		Path:     o.Path,
		Name:     name,
		Size:     o.Size,
		IsFolder: o.IsFolder,
	}
	if !o.Modified.IsZero() {
		dto.Modified = o.Modified.Unix()
	}
	if !o.Ctime.IsZero() {
		dto.Ctime = o.Ctime.Unix()
	}
	if h := o.GetHash().String(); h != "" && h != "{}" && h != "null" {
		dto.Hash = h
	}
	return dto
}

// ToObject rebuilds a *model.Object. Missing times stay zero instead of being
// turned into the Unix epoch.
func (d ObjectDTO) ToObject() *model.Object {
	o := &model.Object{
		ID:       d.ID,
		Path:     d.Path,
		Name:     d.Name,
		Size:     d.Size,
		IsFolder: d.IsFolder,
	}
	if d.Modified != 0 {
		o.Modified = time.Unix(d.Modified, 0)
	}
	if d.Ctime != 0 {
		o.Ctime = time.Unix(d.Ctime, 0)
	}
	if d.Hash != "" {
		o.HashInfo = utils.FromString(d.Hash)
	}
	return o
}

// EncodeObjects is the default codec body for drivers that list *model.Object.
func EncodeObjects(items []driver.ListCacheItem) ([]byte, error) {
	out := make([]ObjectDTO, 0, len(items))
	for _, it := range items {
		o, ok := it.Obj.(*model.Object)
		if !ok {
			return nil, fmt.Errorf("list cache: unexpected obj %T", it.Obj)
		}
		out = append(out, NewObjectDTO(o, it.Name))
	}
	return json.Marshal(out)
}

// DecodeObjects is the counterpart of EncodeObjects.
func DecodeObjects(data []byte) ([]model.Obj, error) {
	var dtos []ObjectDTO
	if err := json.Unmarshal(data, &dtos); err != nil {
		return nil, err
	}
	objs := make([]model.Obj, 0, len(dtos))
	for i := range dtos {
		objs = append(objs, dtos[i].ToObject())
	}
	return objs, nil
}
