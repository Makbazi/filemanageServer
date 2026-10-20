// Package balancer 实现内置负载均衡模式（需求27）：
// 同机多个 worker 实例共同处理请求，本节点作为反向代理轮询分发，
// 附被动+主动健康检查（探测 /api/v1/health），故障节点自动摘除、恢复后自动加回。
package balancer

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"

	"filemanageServer/internal/logx"
)

// Balancer 轮询反向代理。
type Balancer struct {
	workers []*worker
	idx     atomic.Uint64
	probe   *http.Client
}

type worker struct {
	addr  string
	url   *url.URL
	proxy *httputil.ReverseProxy
	up    atomic.Bool
}

// New 创建 Balancer。workers 为 host:port 列表。
func New(workers []string) (*Balancer, error) {
	b := &Balancer{
		probe: &http.Client{Timeout: 3 * time.Second},
	}
	for _, addr := range workers {
		u := &url.URL{Scheme: "http", Host: normalizeAddr(addr)}
		w := &worker{addr: u.Host, url: u}
		w.up.Store(true)
		wp := httputil.NewSingleHostReverseProxy(u)
		defaultDirector := wp.Director
		wp.Director = func(req *http.Request) {
			defaultDirector(req)
			req.Host = u.Host
			if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
				if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
					req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
				} else {
					req.Header.Set("X-Forwarded-For", clientIP)
				}
			}
		}
		wp.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
			w.up.Store(false) // 被动健康检查：转发失败即摘除，等待主动探测恢复
			logx.Warn("balancer: worker %s 失败，暂时摘除: %v", w.addr, err)
			rw.WriteHeader(http.StatusBadGateway)
			_, _ = rw.Write([]byte(`{"error":"all workers unavailable"}`))
		}
		w.proxy = wp
		b.workers = append(b.workers, w)
	}
	if len(b.workers) == 0 {
		return nil, errors.New("balancer: at least one worker is required")
	}
	return b, nil
}

func normalizeAddr(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, "8484")
}

// ServeHTTP 轮询选择健康 worker 转发；全部摘除时仍按轮询尝试（给恢复机会）。
func (b *Balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := len(b.workers)
	start := int(b.idx.Add(1) % uint64(n))
	// 优先健康节点
	for i := 0; i < n; i++ {
		wk := b.workers[(start+i)%n]
		if wk.up.Load() {
			wk.proxy.ServeHTTP(w, r)
			return
		}
	}
	// 全部不健康：仍轮询转发一次，触发被动恢复
	b.workers[start].proxy.ServeHTTP(w, r)
}

// StartHealthCheck 启动主动健康探测（后台 goroutine，ctx 取消时退出）。
func (b *Balancer) StartHealthCheck(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, wk := range b.workers {
					up := b.probeWorker(wk)
					if up != wk.up.Load() {
						if up {
							log.Printf("balancer: worker %s 恢复", wk.addr)
						} else {
							log.Printf("balancer: worker %s 不可用", wk.addr)
						}
					}
					wk.up.Store(up)
				}
			}
		}
	}()
}

func (b *Balancer) probeWorker(wk *worker) bool {
	req, err := http.NewRequest(http.MethodGet, "http://"+wk.addr+"/api/v1/health", nil)
	if err != nil {
		return false
	}
	resp, err := b.probe.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Run 以负载均衡模式监听（独立于 worker 模式，不访问数据库/文件系统）。
func Run(listen string, workers []string, lanOnly bool) error {
	b, err := New(workers)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.StartHealthCheck(ctx, 5*time.Second)
	var handler http.Handler = b
	if lanOnly {
		handler = lanFilter(b)
	}
	handler = logging(handler)
	log.Printf("balancer 启动: listen=%s workers=%v lanOnly=%v", listen, workers, lanOnly)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	return http.Serve(ln, handler)
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[balancer] %s %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

// lanFilter 与 worker 模式一致的局域网过滤。
func lanFilter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"access denied: server only allows LAN clients"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
