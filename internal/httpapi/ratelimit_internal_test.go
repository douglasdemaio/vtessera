package httpapi

import (
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestABucketAllowsItsBurstThenRefuses(t *testing.T) {
	l := newRateLimiter(0, 3)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("a", now); !ok {
			t.Fatalf("request %d refused inside the burst", i+1)
		}
	}
	ok, retry := l.allow("a", now)
	if ok {
		t.Fatal("a request past the burst was allowed")
	}
	if retry <= 0 {
		t.Errorf("retry after = %s, want a positive delay", retry)
	}
}

func TestBucketsAreIndependentPerKey(t *testing.T) {
	l := newRateLimiter(0, 1)
	now := time.Now()
	if ok, _ := l.allow("a", now); !ok {
		t.Fatal("the first key was refused")
	}
	if ok, _ := l.allow("b", now); !ok {
		t.Fatal("the second key was throttled by the first key's spending")
	}
}

func TestARefilledBucketAllowsAgain(t *testing.T) {
	l := newRateLimiter(1, 1)
	start := time.Now()
	if ok, _ := l.allow("a", start); !ok {
		t.Fatal("the first request was refused")
	}
	if ok, _ := l.allow("a", start); ok {
		t.Fatal("a second request was allowed before the bucket refilled")
	}
	if ok, _ := l.allow("a", start.Add(time.Second)); !ok {
		t.Fatal("a request after a full refill was refused")
	}
}

func TestTheBucketMapDoesNotGrowWithoutBound(t *testing.T) {
	l := newRateLimiter(0, 1)
	l.maxKeys = 4
	l.idle = time.Minute
	base := time.Now()
	// Each key is two minutes newer than the last, so by the time the map is
	// full the oldest buckets have gone idle and can be evicted.
	for i := 0; i < 200; i++ {
		l.allow(strconv.Itoa(i), base.Add(time.Duration(i)*2*time.Minute))
	}
	if len(l.buckets) > l.maxKeys+1 {
		t.Errorf("buckets = %d, want it bounded near %d", len(l.buckets), l.maxKeys)
	}
}

func TestClientAddressPrefersTheProxyHeader(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5000"
	r.Header.Set("Fly-Client-IP", "203.0.113.7")
	if got := clientAddress(r, "Fly-Client-IP"); got != "203.0.113.7" {
		t.Errorf("client address = %q, want the header's value", got)
	}
}

func TestClientAddressFallsBackToTheConnection(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5000"
	if got := clientAddress(r, "Fly-Client-IP"); got != "10.0.0.1" {
		t.Errorf("client address = %q, want the connection's host", got)
	}
}

func TestClientAddressTakesTheFirstOfAList(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.9")
	if got := clientAddress(r, "X-Forwarded-For"); got != "203.0.113.7" {
		t.Errorf("client address = %q, want the leftmost entry", got)
	}
}
