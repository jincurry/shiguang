// Package jobs 调度后台任务：启动恢复、reaper（5 分钟）、GC（每日）。
package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"shiguang/internal/service"
)

// Runner 周期执行维护任务。
type Runner struct {
	svc    *service.Service
	log    *slog.Logger
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 创建 Runner。
func New(svc *service.Service, log *slog.Logger) *Runner {
	return &Runner{svc: svc, log: log}
}

// Start 执行一次启动恢复，然后启动周期任务。
func (r *Runner) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel

	// 启动恢复：崩溃遗留的 processing/pending 全部重新入队（staleAfter=0）
	if _, err := r.svc.RecoverStuck(ctx, 0); err != nil {
		r.log.Error("startup recovery", "err", err)
	}

	r.wg.Add(1)
	go func() { // reaper + 卡死重扫：每 5 分钟
		defer r.wg.Done()
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := r.svc.Reap(ctx); err != nil {
					r.log.Error("reaper", "err", err)
				}
				// processing 超过 10 分钟视为卡死，重新入队
				if _, err := r.svc.RecoverStuck(ctx, 10*time.Minute); err != nil {
					r.log.Error("stuck rescan", "err", err)
				}
			}
		}
	}()

	r.wg.Add(1)
	go func() { // GC：每日
		defer r.wg.Done()
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := r.svc.GC(ctx); err != nil {
					r.log.Error("gc", "err", err)
				}
			}
		}
	}()
}

// Stop 停止全部周期任务并等待退出。
func (r *Runner) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
}
