package transfer

import (
	"context"
	"io"
	"time"
)

// Limiter 令牌桶限速器（bytes/sec），零依赖实现。
// kbps<=0 时 NewLimiter 返回 nil，表示不限速（需求18：默认不限速）。
// 注意：Limiter 非并发安全，单连接独占一个实例。
type Limiter struct {
	bytesPerSec int64
	tokens      float64
	last        time.Time
	maxBurst    float64
}

// NewLimiter 创建限速器；kbps<=0 返回 nil。
func NewLimiter(kbps int) *Limiter {
	if kbps <= 0 {
		return nil
	}
	bps := int64(kbps) * 1024
	return &Limiter{
		bytesPerSec: bps,
		tokens:      float64(bps) / 2, // 初始半桶，避免瞬时突发
		last:        time.Now(),
		maxBurst:    float64(bps), // 最多积攒 1 秒的量
	}
}

// Wait 消耗 n 字节的配额，必要时阻塞等待；ctx 取消时提前返回。
func (l *Limiter) Wait(ctx context.Context, n int64) error {
	if l == nil || n <= 0 {
		return nil
	}
	for n > 0 {
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * float64(l.bytesPerSec)
		l.last = now
		if l.tokens > l.maxBurst {
			l.tokens = l.maxBurst
		}
		if l.tokens >= 1 {
			take := float64(n)
			if take > l.tokens {
				take = l.tokens
			}
			l.tokens -= take
			n -= int64(take)
			continue
		}
		// 等待补足一批（至多 32KB），避免长时间睡眠无法响应取消
		need := float64(n)
		if need > 32*1024 {
			need = 32 * 1024
		}
		d := time.Duration((need-l.tokens)/float64(l.bytesPerSec)*float64(time.Second)) + time.Millisecond
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// RateReader 限速读取（用于上传 body / 分片）：先读后按实际字节数扣配额。
type RateReader struct {
	ctx context.Context
	r   io.Reader
	l   *Limiter
}

// NewRateReader 包装读取器；l 为 nil 时直接返回原读取器。
func NewRateReader(ctx context.Context, r io.Reader, l *Limiter) io.Reader {
	if l == nil {
		return r
	}
	return &RateReader{ctx: ctx, r: r, l: l}
}

func (rr *RateReader) Read(p []byte) (int, error) {
	if len(p) > 64*1024 {
		p = p[:64*1024]
	}
	n, err := rr.r.Read(p)
	if n > 0 {
		if werr := rr.l.Wait(rr.ctx, int64(n)); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// RateReadSeeker 限速 ReadSeeker（配合 http.ServeContent 实现带 Range 的限速下载）。
type RateReadSeeker struct {
	ctx context.Context
	rs  io.ReadSeeker
	l   *Limiter
}

// NewRateReadSeeker 包装 ReadSeeker；l 为 nil 时返回原对象。
func NewRateReadSeeker(ctx context.Context, rs io.ReadSeeker, l *Limiter) io.ReadSeeker {
	if l == nil {
		return rs
	}
	return &RateReadSeeker{ctx: ctx, rs: rs, l: l}
}

func (s *RateReadSeeker) Read(p []byte) (int, error) {
	if len(p) > 64*1024 {
		p = p[:64*1024]
	}
	n, err := s.rs.Read(p)
	if n > 0 {
		if werr := s.l.Wait(s.ctx, int64(n)); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (s *RateReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return s.rs.Seek(offset, whence)
}
