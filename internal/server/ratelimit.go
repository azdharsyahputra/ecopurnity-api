package server

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// limiter is a per-key token bucket for the credential endpoints (login, register, appeal, password reset).
// ponytail: in-process memory, so each API instance limits on its own; move to Postgres/Redis counters when one
// instance's budget per IP is too generous.
type limiter struct {
	mu      sync.Mutex
	every   time.Duration
	burst   int
	buckets map[string]*bucket
}

type bucket struct {
	l    *rate.Limiter
	seen time.Time
}

func newLimiter(every time.Duration, burst int) *limiter {
	return &limiter{every: every, burst: burst, buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.buckets) > 10_000 { // drop idle buckets so the map can't grow without bound
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{l: rate.NewLimiter(rate.Every(l.every), l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.l.Allow()
}

// limitPaths answers 429 when a client IP exceeds the budget on one of the given POST paths.
func (l *limiter) limitPaths(paths ...string) func(http.Handler) http.Handler {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && set[r.URL.Path] && !l.allow(clientIP(r)+" "+r.URL.Path) {
				w.Header().Set("Retry-After", strconv.Itoa(int(l.every.Seconds())))
				writeError(w, &Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "Terlalu banyak percobaan. Coba lagi sebentar lagi."})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP is the TCP peer. ponytail: behind a load balancer, read the proxy's X-Forwarded-For (trusted hops only).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
