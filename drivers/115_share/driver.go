package _115_share

import (
	"context"
	"net/http"

	driver115 "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"golang.org/x/time/rate"
)

type Pan115Share struct {
	model.Storage
	Addition
	client  *driver115.Pan115Client
	limiter *rate.Limiter
}

func (d *Pan115Share) Config() driver.Config {
	return config
}

func (d *Pan115Share) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Pan115Share) Init(ctx context.Context) error {
	if d.LimitRate > 0 {
		d.limiter = rate.NewLimiter(rate.Limit(d.LimitRate), 1)
	}

	return d.login()
}

func (d *Pan115Share) WaitLimit(ctx context.Context) error {
	if d.limiter != nil {
		return d.limiter.Wait(ctx)
	}
	return nil
}

func (d *Pan115Share) Drop(ctx context.Context) error {
	return nil
}

func (d *Pan115Share) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	if err := d.WaitLimit(ctx); err != nil {
		return nil, err
	}

	ua := ua115()
	files := make([]shareFile, 0)
	fileResp, err := d.getShareSnapWithUA(ua, dir.GetID(), driver115.QueryLimit(int(d.PageSize)))
	if err != nil {
		return nil, err
	}
	files = append(files, fileResp.Data.List...)
	total := fileResp.Data.Count
	count := len(fileResp.Data.List)
	for total > count {
		fileResp, err := d.getShareSnapWithUA(ua, dir.GetID(), driver115.QueryLimit(int(d.PageSize)), driver115.QueryOffset(count))
		if err != nil {
			return nil, err
		}
		files = append(files, fileResp.Data.List...)
		count += len(fileResp.Data.List)
	}

	return utils.SliceConvert(files, transFunc)
}

func (d *Pan115Share) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if err := d.WaitLimit(ctx); err != nil {
		return nil, err
	}
	// The share downurl API rejects any UA that is not 115Browser's (errno
	// 50029 "当前版本过低"), so the caller's UA — args.Header carries the
	// browser's, i.e. Chrome — must NOT be forwarded. Always resolve our own.
	ua := ua115()
	downloadInfo, err := d.downloadByShareCodeWithUA(ua, file.GetID())
	if err != nil {
		return nil, err
	}

	// The CDN link is bound to the requesting UA and refuses requests without
	// the account cookie (403 "no cookie value"), so hand both to the downloader.
	return &model.Link{
		URL: downloadInfo.URL.URL,
		Header: http.Header{
			"User-Agent": []string{ua},
			"Cookie":     []string{d.Cookie},
		},
	}, nil
}

func (d *Pan115Share) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	return errs.NotSupport
}

func (d *Pan115Share) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NotSupport
}

func (d *Pan115Share) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	return errs.NotSupport
}

func (d *Pan115Share) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NotSupport
}

func (d *Pan115Share) Remove(ctx context.Context, obj model.Obj) error {
	return errs.NotSupport
}

func (d *Pan115Share) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	return errs.NotSupport
}

var _ driver.Driver = (*Pan115Share)(nil)
