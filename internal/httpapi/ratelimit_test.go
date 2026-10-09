package httpapi_test

import (
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/httpapi"
	"github.com/mr-tron/base58"
)

func getWithHeader(t *testing.T, server *httptest.Server, path, header, value string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if header != "" {
		req.Header.Set(header, value)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestTheAddressLimitRefusesASecondRequestFromOneAddress(t *testing.T) {
	server, _ := buildServer(t, serverBuild{rateLimit: httpapi.RateLimitOptions{IPRPS: 0, IPBurst: 1}})

	if status := getWithHeader(t, server, "/v1/metrics", "", ""); status != http.StatusOK {
		t.Fatalf("first read = %d, want 200", status)
	}
	// The second is refused at the edge, before the route ever runs.
	if status := getWithHeader(t, server, "/v1/metrics", "", ""); status != http.StatusTooManyRequests {
		t.Errorf("second read from one address = %d, want 429", status)
	}
}

func TestTheAddressLimitKeysOnTheProxyHeader(t *testing.T) {
	server, _ := buildServer(t, serverBuild{rateLimit: httpapi.RateLimitOptions{
		IPRPS: 0, IPBurst: 1, IPHeader: "Fly-Client-IP",
	}})

	// Two callers arriving through the same proxy connection but carrying
	// different client addresses must not share a bucket. Keying on RemoteAddr
	// here would throttle every visitor to the deployment together.
	if status := getWithHeader(t, server, "/v1/metrics", "Fly-Client-IP", "203.0.113.7"); status != http.StatusOK {
		t.Fatalf("first address = %d, want 200", status)
	}
	if status := getWithHeader(t, server, "/v1/metrics", "Fly-Client-IP", "203.0.113.8"); status != http.StatusOK {
		t.Errorf("a different client address = %d, want its own bucket", status)
	}
	// The same address is still bounded.
	if status := getWithHeader(t, server, "/v1/metrics", "Fly-Client-IP", "203.0.113.7"); status != http.StatusTooManyRequests {
		t.Errorf("a repeat from the first address = %d, want 429", status)
	}
}

func TestStartingAHandshakeIsLimited(t *testing.T) {
	// Each challenge stores a pending row, so the unauthenticated handshake
	// start is exactly the cheap-to-attempt write the limiter has to bound.
	server, _ := buildServer(t, serverBuild{rateLimit: httpapi.RateLimitOptions{IPRPS: 0, IPBurst: 1}})
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	anon := &agentClient{t: t, base: server.URL, http: server.Client()}
	agentID := base58.Encode(pub)
	if status, body := anon.raw(http.MethodPost, "/v1/auth/challenge", map[string]any{"agentId": agentID}, false); status != http.StatusCreated {
		t.Fatalf("first challenge = %d %s, want 201", status, body)
	}
	if status, _ := anon.raw(http.MethodPost, "/v1/auth/challenge", map[string]any{"agentId": agentID}, false); status != http.StatusTooManyRequests {
		t.Errorf("second challenge = %d, want 429", status)
	}
}

func TestARefusalTellsTheCallerWhenToRetry(t *testing.T) {
	server, _ := buildServer(t, serverBuild{rateLimit: httpapi.RateLimitOptions{IPRPS: 0, IPBurst: 1}})
	client := server.Client()

	resp, err := client.Get(server.URL + "/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 429 carried no Retry-After, so the caller cannot tell when to try again")
	}
	body, _ := io.ReadAll(resp.Body)
	if code := decode(t, body)["code"]; code != "RATE_LIMITED" {
		t.Errorf("code = %v, want RATE_LIMITED", code)
	}
}

func TestTheAgentLimitRefusesASecondRequestFromOneAgent(t *testing.T) {
	server, _ := buildServer(t, serverBuild{rateLimit: httpapi.RateLimitOptions{AgentBurst: 1}})
	alice := newCardlessAgent(t, server)
	bob := newCardlessAgent(t, server)

	// A cardless agent makes no authenticated request during setup, so alice's
	// first authenticated call is the one under test. The route answers 501 with
	// no caps configured; what matters is that it was not throttled.
	if status, _ := alice.raw(http.MethodGet, "/v1/limits", nil, true); status == http.StatusTooManyRequests {
		t.Fatalf("alice's first request = %d, want it allowed", status)
	}
	if status, _ := alice.raw(http.MethodGet, "/v1/limits", nil, true); status != http.StatusTooManyRequests {
		t.Errorf("alice's second request = %d, want 429", status)
	}
	if status, _ := bob.raw(http.MethodGet, "/v1/limits", nil, true); status == http.StatusTooManyRequests {
		t.Errorf("bob = %d, want his own bucket rather than alice's", status)
	}
}

func TestRateLimitingCanBeDisabled(t *testing.T) {
	server, _ := buildServer(t, serverBuild{}) // zero bursts: both layers off
	for i := 0; i < 5; i++ {
		if status := getWithHeader(t, server, "/v1/metrics", "", ""); status != http.StatusOK {
			t.Fatalf("read %d = %d, want 200 with limiting off", i+1, status)
		}
	}
}
