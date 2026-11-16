package _115_share

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
)

// fileCacheDTO is the on-disk shape of FileObj. isDir is unexported on the
// struct itself, so it cannot be serialised in place.
type fileCacheDTO struct {
	ID    string `json:"id"`
	Name  string `json:"n"`
	Size  int64  `json:"s"`
	Sha1  string `json:"sha,omitempty"`
	Mod   int64  `json:"m,omitempty"`
	IsDir bool   `json:"d,omitempty"`
	Thumb string `json:"u,omitempty"`
}

func (d *Pan115Share) EncodeObjs(items []driver.ListCacheItem) ([]byte, error) {
	out := make([]fileCacheDTO, 0, len(items))
	for _, it := range items {
		f, ok := it.Obj.(*FileObj)
		if !ok {
			return nil, fmt.Errorf("115share list cache: unexpected obj %T", it.Obj)
		}
		dto := fileCacheDTO{
			ID:    f.FileID,
			Name:  it.Name,
			Size:  f.Size,
			Sha1:  f.Sha1,
			IsDir: f.isDir,
			Thumb: f.ThumbURL,
		}
		if !f.Utm.IsZero() {
			dto.Mod = f.Utm.Unix()
		}
		out = append(out, dto)
	}
	return json.Marshal(out)
}

func (d *Pan115Share) DecodeObjs(data []byte) ([]model.Obj, error) {
	var dtos []fileCacheDTO
	if err := json.Unmarshal(data, &dtos); err != nil {
		return nil, err
	}
	objs := make([]model.Obj, 0, len(dtos))
	for _, dto := range dtos {
		f := &FileObj{
			FileID:   dto.ID,
			FileName: dto.Name,
			Size:     dto.Size,
			Sha1:     dto.Sha1,
			isDir:    dto.IsDir,
			ThumbURL: dto.Thumb,
		}
		if dto.Mod != 0 {
			f.Utm = time.Unix(dto.Mod, 0)
		}
		objs = append(objs, f)
	}
	return objs, nil
}
