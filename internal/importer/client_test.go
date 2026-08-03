package importer

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestWithRetryBackpressureDoesNotDrop 429 是服务端的协作式背压，不是失败：
// 连续被限流的次数远超错误重试预算，最终仍必须成功——否则批量导入会在
// 服务端说「慢一点」的时候静默丢照片。
func TestWithRetryBackpressureDoesNotDrop(t *testing.T) {
	restore := defaultBackpressureWait
	defaultBackpressureWait = time.Millisecond
	defer func() { defaultBackpressureWait = restore }()

	var calls int32
	err := withRetry(context.Background(), 3, func() error {
		if atomic.AddInt32(&calls, 1) <= 20 { // 远超 attempts=3
			return &APIError{Status: http.StatusTooManyRequests, RetryAfter: 0}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("backpressure should not fail the upload: %v", err)
	}
	if calls != 21 {
		t.Errorf("expected to keep retrying past the error budget, calls=%d", calls)
	}
}

// TestWithRetryErrorBudgetStillBounded 5xx 仍受 attempts 限制，不会无限重试。
func TestWithRetryErrorBudgetStillBounded(t *testing.T) {
	var calls int32
	start := time.Now()
	err := withRetry(context.Background(), 3, func() error {
		atomic.AddInt32(&calls, 1)
		return &APIError{Status: http.StatusInternalServerError}
	})
	if err == nil {
		t.Fatal("persistent 5xx should eventually fail")
	}
	if calls != 3 {
		t.Errorf("expected exactly 3 attempts, got %d", calls)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("backoff too slow: %s", time.Since(start))
	}
}

// TestWithRetryDeterministic4xxFailsFast 确定性 4xx（如 415/409）立即返回，
// 不浪费时间重试。
func TestWithRetryDeterministic4xxFailsFast(t *testing.T) {
	for _, status := range []int{400, 409, 415, 422} {
		var calls int32
		err := withRetry(context.Background(), 5, func() error {
			atomic.AddInt32(&calls, 1)
			return &APIError{Status: status}
		})
		if err == nil {
			t.Errorf("status %d should fail", status)
		}
		if calls != 1 {
			t.Errorf("status %d should not be retried, calls=%d", status, calls)
		}
	}
}

// TestWithRetryRespectsContext Ctrl-C 时应尽快返回，不把退避睡满。
func TestWithRetryRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	err := withRetry(ctx, 5, func() error {
		return &APIError{Status: http.StatusTooManyRequests, RetryAfter: 30}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("should abort promptly on cancel, took %s", d)
	}
}
