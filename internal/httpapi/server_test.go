package httpapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/agp"
	"github.com/douglasdemaio/vtessera/internal/auth"
	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/httpapi"
	"github.com/douglasdemaio/vtessera/internal/ledger"
	"github.com/douglasdemaio/vtessera/internal/limits"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/store"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/douglasdemaio/vtessera/internal/trade"
	"github.com/mr-tron/base58"
)

const (
	usdc     = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	eurc     = "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr"
	sessionK = "0123456789abcdef0123456789abcdef"
)

type agentClient struct {
	t       *testing.T
	base    string
	http    *http.Client
	id      string
	private ed25519.PrivateKey
	token   string
}

type clientOptions struct {
	mint string
	card domain.AgentCard
}

func setupServer(t *testing.T) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	server, led := setupServerAt(t, "")
	return server, led
}

// unconfigured reports whether the harness should behave like a deployment with
// settlement switched off: no governed mint scale and no chain to name.
type unconfigured struct{}

func setupServerAt(t *testing.T, publicBaseURL string, unconfigured ...unconfigured) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	return buildServer(t, serverBuild{publicBaseURL: publicBaseURL, unconfigured: len(unconfigured) > 0})
}

// setupServerWithCaps builds the same harness with spending caps wired, so a test
// exercises the cap routes on the real service rather than on a stub.
func setupServerWithCaps(t *testing.T, policy limits.Policy) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	return buildServer(t, serverBuild{policy: &policy})
}

// setupSandboxServer builds the harness with the sandbox flag set, which is what
// an operator gets with no cluster and no endpoint and --sandbox.
func setupSandboxServer(t *testing.T) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	return buildServer(t, serverBuild{sandbox: true})
}

type serverBuild struct {
	publicBaseURL   string
	unconfigured    bool
	policy          *limits.Policy
	sandbox         bool
	acceptTTL       time.Duration
	clock           func() time.Time
	adminToken      string
	adminOperators  []httpapi.Operator
	requireOfferSig bool
	// rateLimit configures the request limits. Zero leaves both layers off, so
	// every test that is not about limiting behaves as it did before.
	rateLimit httpapi.RateLimitOptions
	// prober is the probe runner the harness installs. It is a field rather than a
	// real runner so that a route test asserts what the route does with a result
	// and never opens a socket.
	prober registry.Prober
	// challenge is a pre-issued auth challenge the harness installs in the store
	// before serving. The published handshake vector needs the server to redeem a
	// fixed challenge ID and nonce that issuance would never produce, and this is
	// the seam that lets the test present exactly that state.
	challenge *domain.Challenge
}

func buildServer(t *testing.T, build serverBuild) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	publicBaseURL, unconfigured, policy := build.publicBaseURL, build.unconfigured, build.policy
	if build.sandbox {
		// Configuration refuses to start a sandbox with a chain, so the harness
		// must not build one either.
		unconfigured = true
	}
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	signer, _, err := ledger.LoadOrCreateSigner(filepath.Join(dir, "signer.key"))
	if err != nil {
		t.Fatal(err)
	}
	authSvc, err := auth.New(db, []byte(sessionK))
	if err != nil {
		t.Fatal(err)
	}
	if build.challenge != nil {
		if err := db.CreateChallenge(ctx, *build.challenge); err != nil {
			t.Fatalf("install fixed challenge: %v", err)
		}
	}
	led := ledger.New(db, signer)
	// The marketplace's attestation key is the ledger's key, as it is in main, so
	// the harness reproduces the property the test depends on: the key a reader
	// reads from /healthz is the key that signed the card.
	marketKey, err := signer.AttestationSigner()
	if err != nil {
		t.Fatal(err)
	}
	// An offer priced on chain is checked for precision against a governed scale,
	// so the default harness governs mainnet mints even though it never settles.
	// A test that needs the settlement-unconfigured routes asks for it explicitly.
	var mints tokens.Registry
	opts := httpapi.Options{}
	if !unconfigured {
		var err error
		if mints, err = tokens.ForCluster(cluster.MainnetBeta); err != nil {
			t.Fatalf("governed mints: %v", err)
		}
		opts.Cluster = cluster.MainnetBeta
	}
	registryOpts := []registry.Option{registry.WithRequiredOfferAttestation(build.requireOfferSig)}
	if policy != nil {
		registryOpts = append(registryOpts, registry.WithPricer(*policy))
	}
	if build.prober != nil {
		registryOpts = append(registryOpts, registry.WithProbeRunner(build.prober))
	}
	opts.Registry = registry.New(db, mints, marketKey, registryOpts...)
	trades := trade.New(db, db, db, led)
	if policy != nil {
		trades = trades.WithLimits(*policy, db)
	}
	if build.acceptTTL > 0 {
		trades = trades.WithAcceptanceTTL(build.acceptTTL)
	}
	if build.clock != nil {
		trades = trades.WithClock(build.clock)
	}
	opts.Trades = trades
	opts.Auth = authSvc
	opts.Ledger = led
	opts.Tokens = mints
	opts.Version = "0.1.0-test"
	opts.PublicBaseURL = publicBaseURL
	opts.Sandbox = build.sandbox
	if build.adminToken != "" {
		opts.AdminToken = []byte(build.adminToken)
	}
	opts.AdminOperators = build.adminOperators
	opts.RateLimit = build.rateLimit
	api := httpapi.New(opts)
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	return server, led
}

func newAgent(t *testing.T, server *httptest.Server, opts ...func(*clientOptions)) *agentClient {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	options := &clientOptions{mint: usdc, card: domain.AgentCard{
		Name:        "trader",
		Description: "a marketplace agent",
		URL:         "https://trader.example.com",
		Version:     "0.1.0",
		Skills:      []domain.AgentSkill{{ID: "summarize", Name: "Summarize", Tags: []string{"text"}}},
	}}
	for _, opt := range opts {
		opt(options)
	}
	client := &agentClient{
		t:       t,
		base:    server.URL,
		http:    server.Client(),
		id:      base58.Encode(public),
		private: private,
	}
	client.authenticate()
	client.card(options.card)
	return client
}

// newCardlessAgent returns an identity that has authenticated and never published
// a card. This is the state every identity is in before its first
// PUT /v1/agents/{id}/card, and it is reachable without any error being reported
// anywhere along the way.
func newCardlessAgent(t *testing.T, server *httptest.Server) *agentClient {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &agentClient{
		t:       t,
		base:    server.URL,
		http:    server.Client(),
		id:      base58.Encode(public),
		private: private,
	}
	client.authenticate()
	return client
}

func (c *agentClient) card(card domain.AgentCard) {
	c.t.Helper()
	card.PublicKey = c.id
	if len(card.Currencies) == 0 {
		card.Currencies = []string{usdc, eurc}
	}
	// Wrapped rather than sent bare: the route takes the card and an optional
	// signature together, so signing is a field on the request rather than a
	// second call that could be forgotten.
	body := c.do(http.MethodPut, "/v1/agents/"+url.PathEscape(c.id)+"/card",
		map[string]any{"card": card}, true)
	if body == nil {
		c.t.Fatal("card registration failed")
	}
}

func (c *agentClient) authenticate() {
	c.t.Helper()
	var issued struct {
		ChallengeID string `json:"challengeId"`
		Nonce       string `json:"nonce"`
	}
	body := c.do(http.MethodPost, "/v1/auth/challenge", map[string]any{"agentId": c.id}, false)
	if body == nil {
		c.t.Fatal("challenge failed")
	}
	if err := json.Unmarshal(body, &issued); err != nil {
		c.t.Fatal(err)
	}
	signature := ed25519.Sign(c.private, auth.Message(issued.ChallengeID, c.id, issued.Nonce))
	var session struct {
		Token string `json:"token"`
	}
	body = c.do(http.MethodPost, "/v1/auth/verify", map[string]any{
		"challengeId": issued.ChallengeID,
		"signature":   base64.StdEncoding.EncodeToString(signature),
	}, false)
	if body == nil {
		c.t.Fatal("verify failed")
	}
	if err := json.Unmarshal(body, &session); err != nil {
		c.t.Fatal(err)
	}
	c.token = session.Token
}

func (c *agentClient) do(method, path string, payload any, authed bool) []byte {
	c.t.Helper()
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authed {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode >= 300 {
		c.t.Fatalf("%s %s = %d: %s", method, path, resp.StatusCode, body)
	}
	return body
}

func (c *agentClient) raw(method, path string, payload any, authed bool) (int, []byte) {
	c.t.Helper()
	var reader io.Reader
	if payload != nil {
		encoded, _ := json.Marshal(payload)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if authed {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// rawBody sends a body as written, for the cases where the point is what the
// bytes are rather than what they mean.
func (c *agentClient) rawBody(method, path, body string, authed bool) (int, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	if authed {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func decodeInto(t *testing.T, body []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(body, dst); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func (c *agentClient) publishOffer(amount, mint string, capabilities ...string) domain.Offer {
	c.t.Helper()
	var offer domain.Offer
	body := c.do(http.MethodPost, "/v1/agents/"+url.PathEscape(c.id)+"/offers", map[string]any{
		"direction":       "ask",
		"description":     "summarize a document",
		"capabilities":    capabilities,
		"priceAmount":     amount,
		"priceMint":       mint,
		"settlementModes": []string{"offchain", "onchain"},
		"idempotencyKey":  fmt.Sprintf("offer-%s-%s", amount, strings.Join(capabilities, "-")),
	}, true)
	decodeInto(c.t, body, &offer)
	return offer
}

func TestAgentCardDeclaresAGPGateway(t *testing.T) {
	server, _ := setupServer(t)
	resp, err := server.Client().Get(server.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var card struct {
		Capabilities struct {
			Extensions []struct {
				URI    string         `json:"uri"`
				Params map[string]any `json:"params"`
			} `json:"extensions"`
		} `json:"capabilities"`
		Skills []map[string]any `json:"skills"`
	}
	decodeInto(t, mustRead(t, resp.Body), &card)
	if len(card.Capabilities.Extensions) == 0 {
		t.Fatal("agent card does not declare the AGP extension")
	}
	ext := card.Capabilities.Extensions[0]
	if ext.URI != agp.ExtensionURI {
		t.Errorf("extension uri = %s, want %s", ext.URI, agp.ExtensionURI)
	}
	if ext.Params["agent_role"] != agp.GatewayRole {
		t.Errorf("agent_role = %v, want %q", ext.Params["agent_role"], agp.GatewayRole)
	}
	versions, ok := ext.Params["supported_agp_versions"].([]any)
	if !ok || len(versions) == 0 || versions[0] != "1.0" {
		t.Errorf("supported_agp_versions = %v, want [1.0]", ext.Params["supported_agp_versions"])
	}
	if len(card.Skills) == 0 {
		t.Error("agent card advertises no skills")
	}
}

func TestAgentCardAdvertisesConfiguredPublicURL(t *testing.T) {
	server, _ := setupServerAt(t, "https://vtessera.test/")
	resp, err := server.Client().Get(server.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var card struct {
		URL           string            `json:"url"`
		ReadEndpoints map[string]string `json:"readEndpoints"`
	}
	decodeInto(t, mustRead(t, resp.Body), &card)
	if card.URL != "https://vtessera.test" {
		t.Errorf("url = %q, want the configured origin without a trailing slash", card.URL)
	}
	if got := card.ReadEndpoints["agents"]; got != "https://vtessera.test/v1/agents" {
		t.Errorf("readEndpoints[agents] = %q", got)
	}
}

func TestAgentCardOmitsURLWhenNoPublicBaseURLConfigured(t *testing.T) {
	server, _ := setupServerAt(t, "")
	resp, err := server.Client().Get(server.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var card map[string]any
	decodeInto(t, mustRead(t, resp.Body), &card)
	if url, ok := card["url"]; ok {
		t.Errorf("url = %v, want it absent rather than pointing at a placeholder", url)
	}
	if _, ok := card["readEndpoints"]; ok {
		t.Error("readEndpoints present with no public base URL to build them from")
	}
}

func mustRead(t *testing.T, r io.Reader) []byte {
	t.Helper()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestHealthReportsVerificationKey(t *testing.T) {
	server, led := setupServer(t)
	resp, err := server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var health struct {
		Status          string `json:"status"`
		VerificationKey string `json:"verificationKey"`
		AGP             struct {
			Version string `json:"version"`
		} `json:"agp"`
	}
	decodeInto(t, mustRead(t, resp.Body), &health)
	if health.Status != "ok" {
		t.Errorf("status = %s", health.Status)
	}
	if health.VerificationKey != led.VerificationKey() {
		t.Errorf("verification key = %s, want %s", health.VerificationKey, led.VerificationKey())
	}
	if health.AGP.Version != agp.Version {
		t.Errorf("agp version = %s, want %s", health.AGP.Version, agp.Version)
	}
}

func TestProtectedRoutesRequireSession(t *testing.T) {
	server, _ := setupServer(t)
	anon := &agentClient{t: t, base: server.URL, http: server.Client()}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/trades/whatever"},
		{http.MethodPost, "/v1/trades"},
		{http.MethodGet, "/v1/tesseras/whatever"},
		{http.MethodPost, "/v1/agents/someone/offers"},
	} {
		status, body := anon.raw(tc.method, tc.path, map[string]any{}, false)
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (%s)", tc.method, tc.path, status, body)
		}
	}
	status, _ := anon.raw(http.MethodGet, "/v1/trades/whatever", nil, true)
	if status != http.StatusUnauthorized {
		t.Errorf("empty bearer token = %d, want 401", status)
	}
}

func TestChallengeIsSingleUse(t *testing.T) {
	server, _ := setupServer(t)
	agent := &agentClient{t: t, base: server.URL, http: server.Client()}
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	agent.id = base58.Encode(public)
	agent.private = private
	body := agent.do(http.MethodPost, "/v1/auth/challenge", map[string]any{"agentId": agent.id}, false)
	var issued struct {
		ChallengeID string `json:"challengeId"`
		Nonce       string `json:"nonce"`
	}
	decodeInto(t, body, &issued)
	signature := ed25519.Sign(private, auth.Message(issued.ChallengeID, agent.id, issued.Nonce))
	payload := map[string]any{"challengeId": issued.ChallengeID, "signature": base64.StdEncoding.EncodeToString(signature)}
	if status, _ := agent.raw(http.MethodPost, "/v1/auth/verify", payload, false); status != http.StatusOK {
		t.Fatalf("first verify = %d, want 200", status)
	}
	if status, body := agent.raw(http.MethodPost, "/v1/auth/verify", payload, false); status != http.StatusConflict {
		t.Errorf("replayed verify = %d, want 409 (%s)", status, body)
	}
}

func TestVerifyRejectsWrongKeySignature(t *testing.T) {
	server, _ := setupServer(t)
	agent := &agentClient{t: t, base: server.URL, http: server.Client()}
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	agent.id = base58.Encode(public)
	body := agent.do(http.MethodPost, "/v1/auth/challenge", map[string]any{"agentId": agent.id}, false)
	var issued struct {
		ChallengeID string `json:"challengeId"`
		Nonce       string `json:"nonce"`
	}
	decodeInto(t, body, &issued)
	_, other, _ := ed25519.GenerateKey(nil)
	signature := ed25519.Sign(other, auth.Message(issued.ChallengeID, agent.id, issued.Nonce))
	status, body := agent.raw(http.MethodPost, "/v1/auth/verify", map[string]any{
		"challengeId": issued.ChallengeID,
		"signature":   base64.StdEncoding.EncodeToString(signature),
	}, false)
	if status != http.StatusUnauthorized {
		t.Errorf("verify with a foreign key = %d, want 401 (%s)", status, body)
	}
}

func TestAGPRoutePicksCheapestCompliantAgent(t *testing.T) {
	server, _ := setupServer(t)
	cheap := newAgent(t, server)
	pricey := newAgent(t, server)
	cheap.publishOffer("2.00", usdc, "summarize:document")
	pricey.publishOffer("9.00", usdc, "summarize:document")

	var result struct {
		Result struct {
			TargetCapability string `json:"target_capability"`
			Considered       int    `json:"considered"`
			Route            struct {
				Path       string  `json:"path"`
				Cost       float64 `json:"cost"`
				AgentID    string  `json:"agent_id"`
				CostAmount string  `json:"cost_amount"`
				Direction  string  `json:"direction"`
			} `json:"route"`
		} `json:"result"`
	}
	body := cheap.do(http.MethodPost, "/agp/route", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "agp/route_intent",
		"params": map[string]any{
			"target_capability": "summarize:document",
			"payload":           map[string]any{"documentId": "doc-1"},
		},
	}, false)
	decodeInto(t, body, &result)
	if result.Result.Route.AgentID != cheap.id {
		t.Errorf("routed to %s, want the cheaper agent %s", result.Result.Route.AgentID, cheap.id)
	}
	if result.Result.Route.CostAmount != "2.00" {
		t.Errorf("cost_amount = %q, want 2.00", result.Result.Route.CostAmount)
	}
	if !strings.HasPrefix(result.Result.Route.Path, "Squad_Marketplace/") {
		t.Errorf("path = %q, want an AGP squad path", result.Result.Route.Path)
	}
	if result.Result.Considered != 2 {
		t.Errorf("considered = %d, want both offers considered", result.Result.Considered)
	}
	if result.Result.TargetCapability != "summarize:document" {
		t.Errorf("target_capability = %q", result.Result.TargetCapability)
	}
	if result.Result.Route.Direction != string(domain.DirectionAsk) {
		t.Errorf("direction = %q, want ask", result.Result.Route.Direction)
	}
}

func TestAGPRouteHonorsPolicyConstraints(t *testing.T) {
	server, _ := setupServer(t)
	usdcAgent := newAgent(t, server)
	eurcAgent := newAgent(t, server)
	usdcAgent.publishOffer("1.00", usdc, "translate:document")
	eurcAgent.publishOffer("50.00", eurc, "translate:document")

	var result struct {
		Result struct {
			Route struct {
				AgentID    string `json:"agent_id"`
				CostAmount string `json:"cost_amount"`
			} `json:"route"`
		} `json:"result"`
	}
	body := usdcAgent.do(http.MethodPost, "/agp/route", map[string]any{
		"jsonrpc": "2.0",
		"id":      "eurc-only",
		"method":  "agp/route_intent",
		"params": map[string]any{
			"target_capability":  "translate:document",
			"payload":            map[string]any{},
			"policy_constraints": map[string]any{"currencies": []string{eurc}},
		},
	}, false)
	decodeInto(t, body, &result)
	if result.Result.Route.AgentID != eurcAgent.id {
		t.Errorf("routed to %s, want the EURC agent %s", result.Result.Route.AgentID, eurcAgent.id)
	}
	if result.Result.Route.CostAmount != "50.00" {
		t.Errorf("cost_amount = %q, want 50.00", result.Result.Route.CostAmount)
	}
}

func TestAGPRouteErrorsUseAGPCodes(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)
	agent.publishOffer("1.00", usdc, "summarize:document")

	cases := []struct {
		name   string
		params map[string]any
		code   int
	}{
		{"unknown capability", map[string]any{"target_capability": "nope:nope", "payload": map[string]any{}}, agp.CodeRouteNotFound},
		{"no payload", map[string]any{"target_capability": "summarize:document"}, -32602},
		{"impossible constraint", map[string]any{
			"target_capability":  "summarize:document",
			"payload":            map[string]any{},
			"policy_constraints": map[string]any{"currencies": []string{"not-a-mint"}},
		}, agp.CodePolicyViolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := agent.do(http.MethodPost, "/agp/route", map[string]any{
				"jsonrpc": "2.0",
				"id":      7,
				"method":  "agp/route_intent",
				"params":  tc.params,
			}, false)
			var response struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Error   struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			decodeInto(t, body, &response)
			if response.Error.Code != tc.code {
				t.Errorf("code = %d, want %d (%s)", response.Error.Code, tc.code, body)
			}
			if response.JSONRPC != "2.0" {
				t.Errorf("jsonrpc = %s, want 2.0", response.JSONRPC)
			}
			if string(response.ID) != "7" {
				t.Errorf("id = %s, want 7", response.ID)
			}
		})
	}
}

func TestAGPTableListsAnnouncements(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)
	agent.publishOffer("3.00", usdc, "summarize:document", "summarize:report")
	var table struct {
		Entries       map[string][]agp.RouteEntry  `json:"entries"`
		Announcements []agp.CapabilityAnnouncement `json:"announcements"`
	}
	body := agent.do(http.MethodGet, "/agp/table", nil, false)
	decodeInto(t, body, &table)
	if len(table.Announcements) != 2 {
		t.Errorf("announcements = %d, want 2", len(table.Announcements))
	}
	if len(table.Entries["summarize:document"]) != 1 {
		t.Errorf("entries missing summarize:document: %v", table.Entries)
	}
	for _, announcement := range table.Announcements {
		if announcement.Policy[agp.PolicySettlementModes] == nil {
			t.Errorf("announcement %s announces no settlement modes", announcement.AnnouncementID)
		}
	}
}

func TestFullTradeLifecycleOverHTTP(t *testing.T) {
	server, led := setupServer(t)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")

	var trade domain.Trade
	body := buyer.do(http.MethodPost, "/v1/trades", map[string]any{
		"offerId":        offer.ID,
		"settlementMode": "offchain",
		"idempotencyKey": "trade-1",
	}, true)
	decodeInto(t, body, &trade)
	if trade.State != domain.TradeProposed {
		t.Fatalf("state = %s", trade.State)
	}
	buyer.do(http.MethodPost, "/v1/trades/"+trade.ID+"/negotiate", map[string]any{}, true)
	seller.do(http.MethodPost, "/v1/trades/"+trade.ID+"/accept", map[string]any{}, true)
	body = buyer.do(http.MethodPost, "/v1/trades/"+trade.ID+"/accept", map[string]any{}, true)
	decodeInto(t, body, &trade)
	if trade.State != domain.TradeAccepted {
		t.Fatalf("state = %s after both accepted", trade.State)
	}
	body = buyer.do(http.MethodPost, "/v1/trades/"+trade.ID+"/record", map[string]any{}, true)
	var recorded struct {
		Trade   domain.Trade  `json:"trade"`
		Tessera tesseraResult `json:"tessera"`
	}
	decodeInto(t, body, &recorded)
	if recorded.Trade.State != domain.TradeRecorded {
		t.Errorf("state = %s", recorded.Trade.State)
	}
	if recorded.Tessera.JWS == "" {
		t.Fatal("no tessera issued")
	}
	if recorded.Tessera.VerificationKey != led.VerificationKey() {
		t.Errorf("verification key = %s", recorded.Tessera.VerificationKey)
	}
	claims, err := led.Verify(recorded.Tessera.JWS)
	if err != nil {
		t.Fatalf("tessera does not verify: %v", err)
	}
	if claims.Trade.TradeID != trade.ID {
		t.Errorf("tessera trade = %s, want %s", claims.Trade.TradeID, trade.ID)
	}
	if claims.Trade.Amount != "12.50" {
		t.Errorf("tessera amount = %s, want the exact string 12.50", claims.Trade.Amount)
	}

	body = buyer.do(http.MethodGet, "/v1/tesseras/"+trade.ID, nil, true)
	var fetched tesseraResult
	decodeInto(t, body, &fetched)
	if fetched.JWS != recorded.Tessera.JWS {
		t.Error("tessera changed between calls")
	}

	body = buyer.do(http.MethodGet, "/v1/ledger", nil, false)
	var ledgerView struct {
		Genesis string               `json:"genesis"`
		Entries []domain.LedgerEntry `json:"entries"`
	}
	decodeInto(t, body, &ledgerView)
	if ledgerView.Genesis != domain.GenesisHash {
		t.Errorf("genesis = %s, want %s", ledgerView.Genesis, domain.GenesisHash)
	}
	if len(ledgerView.Entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(ledgerView.Entries))
	}
	if ledgerView.Entries[0].PrevHash != domain.GenesisHash {
		t.Errorf("first entry prevHash = %s, want the genesis hash", ledgerView.Entries[0].PrevHash)
	}
}

type tesseraResult struct {
	JWS             string `json:"jws"`
	VerificationKey string `json:"verificationKey"`
}

func TestOnchainTradeIsNotImplemented(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("1.00", usdc, "summarize:document")
	status, body := buyer.raw(http.MethodPost, "/v1/trades", map[string]any{
		"offerId":        offer.ID,
		"settlementMode": "onchain",
	}, true)
	if status != http.StatusNotImplemented {
		t.Fatalf("onchain trade = %d, want 501 (%s)", status, body)
	}
	if !strings.Contains(string(body), "ONCHAIN_UNAVAILABLE") {
		t.Errorf("body = %s, want an ONCHAIN_UNAVAILABLE code", body)
	}
}

func TestOfferPrecisionRejectedForOnchainOnly(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)
	status, body := agent.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(agent.id)+"/offers", map[string]any{
		"direction":       "ask",
		"description":     "too precise for a 6 decimal mint",
		"priceAmount":     "1.000000001",
		"priceMint":       usdc,
		"settlementModes": []string{"onchain"},
	}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", status, body)
	}
	if !strings.Contains(string(body), "AMOUNT_TOO_PRECISE") {
		t.Errorf("body = %s, want AMOUNT_TOO_PRECISE", body)
	}
}

func TestSuspiciousRoutesAreForbidden(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("4.00", usdc, "summarize:document")
	body := buyer.do(http.MethodPost, "/v1/trades", map[string]any{"offerId": offer.ID}, true)
	var tr domain.Trade
	decodeInto(t, body, &tr)
	status, respBody := seller.raw(http.MethodPost, "/v1/offers/"+offer.ID+"/close", map[string]any{}, true)
	if status != http.StatusOK {
		t.Fatalf("owner close = %d (%s)", status, respBody)
	}
	outsider := newAgent(t, server)
	status, respBody = outsider.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/negotiate", map[string]any{}, true)
	if status != http.StatusForbidden {
		t.Errorf("outsider negotiate = %d, want 403 (%s)", status, respBody)
	}
}

func TestIllegalTransitionsReturnConflict(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("4.00", usdc, "summarize:document")
	body := buyer.do(http.MethodPost, "/v1/trades", map[string]any{"offerId": offer.ID}, true)
	var tr domain.Trade
	decodeInto(t, body, &tr)
	status, respBody := buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/record", map[string]any{}, true)
	if status != http.StatusConflict {
		t.Fatalf("record from proposed = %d, want 409 (%s)", status, respBody)
	}
	if !strings.Contains(string(respBody), "ILLEGAL_STATE") {
		t.Errorf("body = %s, want ILLEGAL_STATE", respBody)
	}
}

func TestPublicDiscoveryHidesClosedOffers(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	offer := seller.publishOffer("6.00", usdc, "summarize:document")
	var search struct {
		Offers []domain.Offer `json:"offers"`
	}
	body := seller.do(http.MethodGet, "/v1/offers?q=document", nil, false)
	decodeInto(t, body, &search)
	if len(search.Offers) != 1 {
		t.Fatalf("open offers = %d, want 1", len(search.Offers))
	}
	seller.do(http.MethodPost, "/v1/offers/"+offer.ID+"/close", map[string]any{}, true)
	body = seller.do(http.MethodGet, "/v1/offers?q=document", nil, false)
	decodeInto(t, body, &search)
	if len(search.Offers) != 0 {
		t.Errorf("closed offer still listed: %d", len(search.Offers))
	}
}

func TestUnknownRoutesAre404(t *testing.T) {
	server, _ := setupServer(t)
	agent := newAgent(t, server)
	for _, path := range []string{"/v1/agents/nobody", "/v1/offers/nothing", "/v1/trades/nothing"} {
		status, body := agent.raw(http.MethodGet, path, nil, true)
		if status != http.StatusNotFound && status != http.StatusForbidden {
			t.Errorf("GET %s = %d (%s)", path, status, body)
		}
	}
}

func TestMetricsNeedNoCredentials(t *testing.T) {
	server, _ := setupServer(t)
	anon := &agentClient{t: t, base: server.URL, http: server.Client()}
	status, body := anon.raw(http.MethodGet, "/v1/metrics", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/metrics = %d, want 200 without a session (%s)", status, body)
	}
	for _, key := range []string{`"generatedAt"`, `"totals"`, `"agents"`, `"delivered"`, `"asOf"`} {
		if !strings.Contains(string(body), key) {
			t.Errorf("metrics body is missing %s: %s", key, body)
		}
	}
	if !strings.Contains(string(body), `"asOf":null`) {
		t.Errorf("empty marketplace should report a null asOf, got: %s", body)
	}

	var m domain.UsageMetrics
	decodeInto(t, body, &m)
	if m.AsOf != nil {
		t.Errorf("asOf = %v, want nil before any delivery", m.AsOf)
	}
	if m.Totals != (domain.UsageTotals{}) {
		t.Errorf("totals = %+v, want all zero", m.Totals)
	}
	if m.Agents == nil {
		t.Error("agents is nil, want [] so the site never has to nil-check")
	}
	if m.GeneratedAt.IsZero() {
		t.Error("generatedAt is zero, want the service clock")
	}
}

func TestMetricsCountARecordedTradeAsDelivered(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")

	var trade domain.Trade
	decodeInto(t, buyer.do(http.MethodPost, "/v1/trades", map[string]any{
		"offerId":        offer.ID,
		"settlementMode": "offchain",
		"idempotencyKey": "trade-1",
	}, true), &trade)
	buyer.do(http.MethodPost, "/v1/trades/"+trade.ID+"/negotiate", map[string]any{}, true)
	seller.do(http.MethodPost, "/v1/trades/"+trade.ID+"/accept", map[string]any{}, true)
	buyer.do(http.MethodPost, "/v1/trades/"+trade.ID+"/accept", map[string]any{}, true)
	buyer.do(http.MethodPost, "/v1/trades/"+trade.ID+"/record", map[string]any{}, true)

	anon := &agentClient{t: t, base: server.URL, http: server.Client()}
	var m domain.UsageMetrics
	decodeInto(t, anon.do(http.MethodGet, "/v1/metrics", nil, false), &m)

	if m.Totals.Delivered != 1 {
		t.Errorf("delivered = %d, want 1", m.Totals.Delivered)
	}
	if m.Totals.Consumers != 1 {
		t.Errorf("consumers = %d, want 1", m.Totals.Consumers)
	}
	if m.Totals.Services != 1 {
		t.Errorf("services = %d, want 1", m.Totals.Services)
	}
	if m.Totals.Disputed != 0 || m.Totals.Cancelled != 0 {
		t.Errorf("a recorded trade should not be disputed or cancelled, got %+v", m.Totals)
	}
	if m.AsOf == nil {
		t.Fatal("asOf is nil, want the receipt time of the recorded trade")
	}
	if len(m.Agents) != 1 {
		t.Fatalf("agents = %+v, want the selling agent only", m.Agents)
	}
	if m.Agents[0].AgentID != seller.id {
		t.Errorf("agentId = %s, want the seller %s", m.Agents[0].AgentID, seller.id)
	}
	if m.Agents[0].Delivered != 1 {
		t.Errorf("per-agent delivered = %d, want 1", m.Agents[0].Delivered)
	}
}

// A field the caller got wrong is the caller's problem. Reporting it as a 500
// tells an agent the marketplace is broken when its own request was malformed,
// which is the difference between retrying and giving up.
func TestInvalidCardIsABadRequestNotAServerError(t *testing.T) {
	server, _ := setupServer(t)
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	agentID := base58.Encode(public)
	client := &agentClient{t: t, base: server.URL, http: server.Client(), id: agentID, private: private}
	client.authenticate()

	// url is omitempty, so omitting it looks optional in the schema while
	// validation rejects it. That mismatch cost a real agent a 500.
	status, body := client.raw(http.MethodPut, "/v1/agents/"+url.PathEscape(agentID)+"/card", map[string]any{
		"card": domain.AgentCard{
			Name:      "trader",
			PublicKey: agentID,
		},
	}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("card with no url = %d, want 400: %s", status, body)
	}
	var failure struct {
		Code    string `json:"code"`
		Message string `json:"error"`
	}
	if err := json.Unmarshal(body, &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != "INVALID_REQUEST" {
		t.Errorf("code = %q, want INVALID_REQUEST", failure.Code)
	}
	// The message still has to name the offending field, or a 400 is no better
	// than a 500.
	if !strings.Contains(failure.Message, "url") {
		t.Errorf("message %q does not mention the field to fix", failure.Message)
	}
}

func TestInvalidOfferIsABadRequest(t *testing.T) {
	server, _ := setupServer(t)
	client := newAgent(t, server)
	status, body := client.raw(http.MethodPost, "/v1/agents/"+url.PathEscape(client.id)+"/offers", domain.Offer{
		Description: "",
		Direction:   domain.DirectionAsk,
		PriceAmount: money.MustParse("1.00"),
		PriceMint:   usdc,
	}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("offer with no description = %d, want 400: %s", status, body)
	}
}

// The directory publishes vtessera's call shapes so an agent does not have to
// read this source. That only helps if the published examples are the ones the
// service accepts. This pins the values the live service was verified to take,
// so a rename like direction sell to ask cannot drift away from the copy
// another repository hands to agents.
func TestPublishedOfferShapeMatchesWhatTheServiceAccepts(t *testing.T) {
	// Verified against the live service: every field below is load-bearing.
	// A 201 here is the contract; a rejection means the published copy is wrong.
	offer := domain.Offer{
		Direction:   domain.DirectionAsk,
		Description: "summarises a document",
		Capabilities: []string{
			"summarize:document",
		},
		PriceAmount:     money.MustParse("1.00"),
		PriceMint:       "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain},
		// The service assigns status; it appears in the published response, not
		// in the request, so it is set here only to validate the whole object.
		Status: domain.OfferOpen,
	}
	if err := offer.Validate(); err != nil {
		t.Fatalf("the offer shape published in the directory is rejected: %v", err)
	}
	// direction is ask or bid. sell and buy read naturally and are wrong.
	for _, bad := range []string{"sell", "buy", "Sell", ""} {
		o := offer
		o.Direction = domain.OfferDirection(bad)
		if bad == "" {
			// An empty direction means unset, which is a different failure than
			// an unrecognised one; it must not pass either.
			if err := o.Validate(); err == nil {
				t.Errorf("direction %q was accepted", bad)
			}
			continue
		}
		if err := o.Validate(); err == nil {
			t.Errorf("direction %q was accepted", bad)
		}
	}
	// A ticker is not a mint. Identity is the base58 address alone.
	if err := domain.ValidateMint("USDC"); err == nil {
		t.Error(`priceMint "USDC" was accepted; the directory publishes an address`)
	}
}

// expiryServer builds a harness whose accepted trades expire, with a clock the
// test drives. The HTTP surface is where a buyer learns whether a trade can be
// walked away from, so the refusal has to be a status and a code rather than
// something only the Go API can express.
func expiryServer(t *testing.T) (*httptest.Server, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	server, _ := buildServer(t, serverBuild{
		acceptTTL: 24 * time.Hour,
		clock:     func() time.Time { return now },
	})
	return server, &now
}

// acceptOverHTTP takes a trade from published to accepted and returns its ID.
func acceptOverHTTP(t *testing.T, buyer, seller *agentClient, amount string) string {
	t.Helper()
	offer := seller.publishOffer(amount, usdc)
	var tr domain.Trade
	decodeInto(t, buyer.do(http.MethodPost, "/v1/trades", map[string]any{
		"offerId":        offer.ID,
		"settlementMode": string(domain.SettlementOffchain),
		"idempotencyKey": "trade-" + amount,
	}, true), &tr)
	if tr.State != domain.TradeProposed {
		t.Fatalf("state = %s, want proposed", tr.State)
	}
	buyer.do(http.MethodPost, "/v1/trades/"+tr.ID+"/negotiate", map[string]any{}, true)
	seller.do(http.MethodPost, "/v1/trades/"+tr.ID+"/accept", map[string]any{}, true)
	decodeInto(t, buyer.do(http.MethodPost, "/v1/trades/"+tr.ID+"/accept", map[string]any{}, true), &tr)
	if tr.State != domain.TradeAccepted {
		t.Fatalf("state = %s, want accepted", tr.State)
	}
	return tr.ID
}

func TestCancellingAnAcceptedTradeTooEarlyIsRefused(t *testing.T) {
	server, _ := expiryServer(t)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	id := acceptOverHTTP(t, buyer, seller, "10.00")

	status, resp := buyer.raw(http.MethodPost, "/v1/trades/"+id+"/cancel", map[string]any{}, true)
	if status != http.StatusConflict {
		t.Errorf("cancel before the deadline = %d, want 409 (%s)", status, resp)
	}
	var refused struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	decodeInto(t, resp, &refused)
	if refused.Code != "TRADE_NOT_EXPIRED" {
		t.Errorf("code = %s, want TRADE_NOT_EXPIRED", refused.Code)
	}
	// The message has to carry the deadline, or an agent that hit this has no way
	// to learn when it may try again.
	if !strings.Contains(refused.Error, "accepted until") {
		t.Errorf("message %q does not say when the trade can be cancelled", refused.Error)
	}

	// Still accepted: a refusal that left it cancelled would be a race, not a
	// refusal.
	var got domain.Trade
	decodeInto(t, buyer.do(http.MethodGet, "/v1/trades/"+id, nil, true), &got)
	if got.State != domain.TradeAccepted {
		t.Errorf("state = %s, want the refusal to leave it accepted", got.State)
	}
}

func TestCancellingAnExpiredAcceptedTradeSucceeds(t *testing.T) {
	server, now := expiryServer(t)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	id := acceptOverHTTP(t, buyer, seller, "10.00")

	*now = now.Add(24*time.Hour + time.Second)
	var cancelled domain.Trade
	decodeInto(t, buyer.do(http.MethodPost, "/v1/trades/"+id+"/cancel", map[string]any{}, true), &cancelled)
	if cancelled.State != domain.TradeCancelled {
		t.Errorf("state = %s, want cancelled", cancelled.State)
	}
}

func TestACardlessBuyerIsToldToPublishACardRatherThanThatTheOfferIsMissing(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	offer := seller.publishOffer("10", usdc)
	buyer := newCardlessAgent(t, server)

	request := map[string]any{
		"offerId": offer.ID, "settlementMode": "offchain", "idempotencyKey": "cardless-1",
	}
	status, body := buyer.raw(http.MethodPost, "/v1/trades", request, true)

	// The offer is real and the session is real. Answering 404 here told an
	// integrator that an identifier it had just read out of GET /v1/agents was
	// wrong, for a request that had nothing wrong with it.
	if status != http.StatusConflict {
		t.Fatalf("cardless buyer = %d %s, want 409", status, body)
	}
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	decodeInto(t, body, &refusal)
	if refusal.Code != "NO_CARD_PUBLISHED" {
		t.Errorf("code = %s, want NO_CARD_PUBLISHED", refusal.Code)
	}
	// The refusal has to say what to do about it, not merely that something is
	// absent.
	if !strings.Contains(refusal.Error, "/v1/agents/"+buyer.id+"/card") {
		t.Errorf("refusal %q does not name the request that would fix it", refusal.Error)
	}

	// And it is a step rather than a dead end: the same request, unchanged, works
	// once the card is published.
	buyer.card(domain.AgentCard{
		Name: "buyer", Description: "an agent that had not published yet",
		URL: "https://buyer.example.com", Version: "0.1.0",
	})
	if status, body := buyer.raw(http.MethodPost, "/v1/trades", request, true); status != http.StatusCreated {
		t.Fatalf("same request after publishing a card = %d %s, want 201", status, body)
	}
}
