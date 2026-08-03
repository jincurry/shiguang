package importer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shiguang/internal/imgproc"
)

// Options 是一次导入运行的参数。
type Options struct {
	Root        string
	GroupMode   GroupMode
	SingleTitle string
	Concurrency int
	MaxBytes    int64
	DryRun      bool
	// UploadMode 由服务端 /stats 决定：local 走 multipart，s3 走 presign 直传。
	UploadMode string
	// Progress 非 nil 时每完成一张回调一次（用于打印进度）。
	Progress func(Stat)
}

// Stat 是导入进度快照。
type Stat struct {
	Total    int
	Done     int
	Uploaded int
	Skipped  int // 秒传命中：服务端已有同图
	Failed   int
	Current  string
}

// Result 是导入结束后的汇总。
type Result struct {
	Stat
	NodesCreated int
	NodesReused  int
	Errors       []string
	Elapsed      time.Duration
}

// Run 执行导入：扫描 → 分组 → 建/复用节点 → 并发上传。
// 重跑安全：内容寻址去重让已导入的照片返回 409，被计入 Skipped 而非失败，
// 因此中断后重跑只补没传完的部分。
func Run(ctx context.Context, c *Client, opt Options) (*Result, error) {
	start := time.Now()
	if opt.Concurrency < 1 {
		opt.Concurrency = 4
	}
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = 30 << 20
	}

	photos, skipped, err := Scan(opt.Root, opt.MaxBytes)
	if err != nil {
		return nil, err
	}
	res := &Result{Errors: skipped}
	if len(photos) == 0 {
		res.Elapsed = time.Since(start)
		return res, nil
	}
	groups := Plan(photos, opt.GroupMode, opt.SingleTitle)
	res.Total = len(photos)

	if opt.DryRun {
		res.Elapsed = time.Since(start)
		return res, nil
	}

	// 复用已存在的同 (date,title) 节点，让重跑不会造出一堆重复节点
	existing, err := c.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取现有节点失败: %w", err)
	}
	byKey := map[string]string{}
	for _, n := range existing {
		byKey[n.Date+"|"+n.Title] = n.ID
	}

	var stat Stat
	stat.Total = len(photos)
	var mu sync.Mutex
	report := func(current string) {
		if opt.Progress == nil {
			return
		}
		mu.Lock()
		s := stat
		s.Current = current
		mu.Unlock()
		opt.Progress(s)
	}

	for _, g := range groups {
		if ctx.Err() != nil {
			break
		}
		nodeID, ok := byKey[g.Date+"|"+g.Title]
		if ok {
			res.NodesReused++
		} else {
			n, err := c.CreateNode(ctx, g.Date, g.Title, "")
			if err != nil {
				mu.Lock()
				stat.Failed += len(g.Photos)
				stat.Done += len(g.Photos)
				mu.Unlock()
				res.Errors = append(res.Errors,
					fmt.Sprintf("节点「%s」创建失败：%v（该组 %d 张跳过）", g.Title, err, len(g.Photos)))
				continue
			}
			nodeID = n.ID
			byKey[g.Date+"|"+g.Title] = nodeID
			res.NodesCreated++
		}

		if err := uploadGroup(ctx, c, opt, nodeID, g, &stat, &mu, report, res); err != nil {
			return nil, err
		}
	}

	mu.Lock()
	res.Stat = stat
	mu.Unlock()
	res.Current = ""
	res.Elapsed = time.Since(start)
	return res, nil
}

// uploadGroup 并发上传一个节点下的照片。
func uploadGroup(ctx context.Context, c *Client, opt Options, nodeID string, g *Group,
	stat *Stat, mu *sync.Mutex, report func(string), res *Result) error {

	sem := make(chan struct{}, opt.Concurrency)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var canceled atomic.Bool

	for _, p := range g.Photos {
		if ctx.Err() != nil {
			canceled.Store(true)
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(p *Photo) {
			defer wg.Done()
			defer func() { <-sem }()

			err := uploadOne(ctx, c, opt, nodeID, p)

			mu.Lock()
			stat.Done++
			switch {
			case err == nil:
				stat.Uploaded++
			case isDuplicate(err):
				stat.Skipped++
			default:
				stat.Failed++
			}
			mu.Unlock()

			if err != nil && !isDuplicate(err) {
				errMu.Lock()
				if len(res.Errors) < 200 { // 错误列表封顶，避免刷屏
					res.Errors = append(res.Errors, fmt.Sprintf("%s：%v", p.Rel, err))
				}
				errMu.Unlock()
			}
			report(p.Rel)
		}(p)
	}
	wg.Wait()
	if canceled.Load() || ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// uploadOne 上传单张：local 走 multipart，s3 走 presign → PUT → confirm。
func uploadOne(ctx context.Context, c *Client, opt Options, nodeID string, p *Photo) error {
	caption := strings.TrimSuffix(p.Name, filepath.Ext(p.Name))
	if opt.UploadMode == "s3" {
		var uploadURL, photoID string
		if err := withRetry(ctx, 5, func() error {
			var err error
			uploadURL, photoID, err = c.PresignPut(ctx, nodeID, p.Name, p.Size,
				imgproc.ContentTypeForExt(p.Ext))
			return err
		}); err != nil {
			return err
		}
		if err := withRetry(ctx, 3, func() error {
			return c.PutObject(ctx, uploadURL, p.Path, imgproc.ContentTypeForExt(p.Ext), p.Size)
		}); err != nil {
			return err
		}
		return withRetry(ctx, 5, func() error {
			_, err := c.ConfirmUpload(ctx, photoID)
			return err
		})
	}
	return withRetry(ctx, 5, func() error {
		_, err := c.UploadLocal(ctx, nodeID, p.Path, caption)
		return err
	})
}

// isDuplicate 判断错误是否为秒传冲突（视为"已存在"，不算失败）。
func isDuplicate(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsDuplicate()
}
