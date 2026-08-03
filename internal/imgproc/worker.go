package imgproc

import (
	"context"
	"log/slog"
	"sync"
)

// Handler 处理一张照片（由 service 注入，负责 store/blob 编排）。
type Handler func(ctx context.Context, photoID string)

// Pool 是进程内固定大小 worker 池：缓冲队列 512，队列满时 Enqueue 返回 false
// （调用方不得因此拒收上传，靠周期恢复扫描兜底重入队）。
type Pool struct {
	queue   chan string
	handler Handler
	wg      sync.WaitGroup
	log     *slog.Logger

	mu       sync.Mutex
	inflight map[string]bool // 进程内去重：同一照片不并发处理
	closed   bool
}

// NewPool 创建并启动 n 个 worker。
func NewPool(n int, handler Handler, log *slog.Logger) *Pool {
	p := &Pool{
		queue:    make(chan string, 512),
		handler:  handler,
		log:      log,
		inflight: map[string]bool{},
	}
	for i := 0; i < n; i++ {
		p.wg.Add(1)
		go p.run()
	}
	return p
}

func (p *Pool) run() {
	defer p.wg.Done()
	for id := range p.queue {
		// worker 生命周期独立于请求，用 Background 上下文。
		p.handler(context.Background(), id)
		p.mu.Lock()
		delete(p.inflight, id)
		p.mu.Unlock()
	}
}

// Enqueue 尝试入队；队列满或已在处理中返回 false（不阻塞上传路径）。
func (p *Pool) Enqueue(photoID string) bool {
	p.mu.Lock()
	if p.closed || p.inflight[photoID] {
		p.mu.Unlock()
		return false
	}
	p.inflight[photoID] = true
	p.mu.Unlock()

	select {
	case p.queue <- photoID:
		return true
	default:
		p.mu.Lock()
		delete(p.inflight, photoID)
		p.mu.Unlock()
		p.log.Warn("imgproc queue full, photo left for recovery scan", "photo_id", photoID)
		return false
	}
}

// Close 关闭队列并等待在途任务清空（优雅退出：drain worker）。
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	close(p.queue)
	p.wg.Wait()
}
