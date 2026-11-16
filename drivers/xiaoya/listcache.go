package xiaoya

import (
	"encoding/json"
	"fmt"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/listcache"
	"github.com/alist-org/alist/v3/internal/model"
)

// The 115-flash-transfer driver lists plain *model.Object, so the shared DTO is
// enough: the download link is resolved from the file id alone.
func (d *AliyundriveShare2Pan115) EncodeObjs(items []driver.ListCacheItem) ([]byte, error) {
	return listcache.EncodeObjects(items)
}

func (d *AliyundriveShare2Pan115) DecodeObjs(data []byte) ([]model.Obj, error) {
	return listcache.DecodeObjects(data)
}

// quarkShareCacheDTO carries the two fields QuarkShare.Link needs. Without
// ShareFidToken the download call is rejected, so they must survive the round
// trip; PdirFid is the parent id used by the same call.
type quarkShareCacheDTO struct {
	listcache.ObjectDTO
	ShareFidToken string `json:"fid_token,omitempty"`
	PdirFid       string `json:"pdir,omitempty"`
}

func (d *QuarkShare) EncodeObjs(items []driver.ListCacheItem) ([]byte, error) {
	out := make([]quarkShareCacheDTO, 0, len(items))
	for _, it := range items {
		o, ok := it.Obj.(*ShareObj)
		if !ok {
			return nil, fmt.Errorf("quarkshare list cache: unexpected obj %T", it.Obj)
		}
		out = append(out, quarkShareCacheDTO{
			ObjectDTO:     listcache.NewObjectDTO(&o.Object, it.Name),
			ShareFidToken: o.ShareFidToken,
			PdirFid:       o.PdirFid,
		})
	}
	return json.Marshal(out)
}

func (d *QuarkShare) DecodeObjs(data []byte) ([]model.Obj, error) {
	var dtos []quarkShareCacheDTO
	if err := json.Unmarshal(data, &dtos); err != nil {
		return nil, err
	}
	objs := make([]model.Obj, 0, len(dtos))
	for i := range dtos {
		dto := &dtos[i]
		objs = append(objs, &ShareObj{
			Object:        *dto.ToObject(),
			ShareFidToken: dto.ShareFidToken,
			PdirFid:       dto.PdirFid,
		})
	}
	return objs, nil
}
