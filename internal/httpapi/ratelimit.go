package httpapi

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimitOptions bounds how often a caller may hit the API. Two independent
// limits are measured: one per authenticated agent, and one per client address
// for everything else, because the cheap-to-attempt requests (starting a
// challenge, reading the board) have no agent behind them yet.
//
// A zero burst turns that limiter off; a zero rate means the bucket never
// refills, so a burst is a fixed quota. Both are deliberate so an operator can
// disable a layer or run a strict fixed allowance.
type RateLimitOptions struct {
	AgentRPS   float64
	AgentBurst int
	IPRPS      float64
	IPBurst    int
	// IPHeader is the header the proxy in front of this service sets to the
	// client address. Fly sets Fly-Client-IP and makes the service port
	// unreachable directly, so a client cannot forge it. Empty falls back to
	// the connection's RemoteAddr, which is correct only when nothing proxies.
	IPHeader string
}

// rateLimiter is an in-memory token bucket per key. It is deliberately not
// shared or persisted: this deployment is one process, and a restart resetting
// the buckets is an acceptable cost of not holding per-request state in the
// database. It is documented as a known limit in the threat model.
type rateLimiter struct {
	rps   float64
	burst float64

	mu      sync.Mutex
	buckets map[string]*tokenBucket
	// idle is how long an untouched bucket is kept before eviction, and maxKeys
	// bounds the map. Without both, an attacker rotating source addresses would
	// grow the map without limit, which is the denial of service the limiter is
	// meant to blunt.
	idle    time.Duration
	maxKeys int
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	return &rateLimiter{
		rps:     rps,
		burst:   float64(burst),
		buckets: make(map[string]*tokenBucket),
		idle:    10 * time.Minute,
		maxKeys: 4096,
	}
}

// allow consumes one token for key. When it refuses, it also reports how long
// the caller should wait before a token is available, so the response can carry
// an honest Retry-After.
func (l *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			l.evictLocked(now)
		}
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
			b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rps)
			b.last = now
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	if l.rps <= 0 {
		return false, time.Hour
	}
	return false, time.Duration((1-b.tokens)/l.rps*float64(time.Second)) + time.Second
}

func (l *rateLimiter) evictLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > l.idle {
			delete(l.buckets, k)
		}
	}
}

// clientAddress resolves the caller's address for the per-address limit. It
// prefers the header the proxy sets, because behind Fly every connection shares
// the proxy's address and keying on that would throttle every caller together.
// It falls back to RemoteAddr when the header is absent, which is the correct
// answer for a deployment with no proxy in front.
func clientAddress(r *http.Request, header string) string {
	if header != "" {
		if v := strings.TrimSpace(r.Header.Get(header)); v != "" {
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			if v != "" {
				return v
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// writeRateLimited refuses a request with 429 and a Retry-After the caller can
// act on rather than a bare failure.
func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeErrorStatus(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests; retry after the Retry-After delay")
}
