//go:build devnet

// Package e2e_devnet runs a full trade against the public devnet cluster.
//
// This is the evidence that the spending caps and the governed mint table work
// against a real cluster rather than only against a local validator: the same
// preflight the service runs at boot verifies the endpoint's genesis hash and
// every governed mint's authority on devnet, and then two agents complete a
// trade end to end through the HTTP API.
//
// No funds move and no key with value is needed, because the trade settles
// off-chain. It is behind the `devnet` build tag and never runs in the hermetic
// suite, so `make test` stays offline:
//
//	make test-devnet
//	VTESSERA_DEVKNET_RPC_URL=https://api.devnet.solana.com make test-devnet
//
// The tests move no value and hold no funded key: the trades settle off-chain.
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/auth"
	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/httpapi"
	"github.com/douglasdemaio/vtessera/internal/ledger"
	"github.com/douglasdemaio/vtessera/internal/limits"
	"github.com/douglasdemaio/vtessera/internal/preflight"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/store"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/douglasdemaio/vtessera/internal/trade"
	"github.com/gagliardetto/solana-go"
	"golang.org/x/crypto/ed25519"
)

const devnetRPCURL = "https://api.devnet.solana.com"

func devnetEndpoint() string {
	if u := os.Getenv("VTESSERA_DEVKNET_RPC_URL"); u != "" {
		return u
	}
	return devnetRPCURL
}

type devnetAgent struct {
	t       *testing.T
	base    string
	http    *http.Client
	id      string
	private ed25519.PrivateKey
	token   string
}

func newDevnetAgent(t *testing.T, server *httptest.Server) *devnetAgent {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &devnetAgent{
		t: t, base: server.URL, http: server.Client(),
		id: solana.PublicKey(pub).String(), private: priv,
	}
	a.authenticate()
	return a
}

func (a *devnetAgent) do(method, path string, payload any) (int, []byte) {
	a.t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			a.t.Fatal(err)
		}
		body = bytesReader(encoded)
	}
	req, err := http.NewRequest(method, a.base+path, body)
	if err != nil {
		a.t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.http.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		a.t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func (a *devnetAgent) mustDo(method, path string, payload any, want int) []byte {
	a.t.Helper()
	status, body := a.do(method, path, payload)
	if status != want {
		a.t.Fatalf("%s %s = %d: %s", method, path, status, body)
	}
	return body
}

func (a *devnetAgent) authenticate() {
	a.t.Helper()
	var issued struct {
		ChallengeID string `json:"challengeId"`
		Nonce       string `json:"nonce"`
	}
	body := a.mustDo(http.MethodPost, "/v1/auth/challenge",
		map[string]any{"agentId": a.id}, http.StatusCreated)
	if err := json.Unmarshal(body, &issued); err != nil {
		a.t.Fatal(err)
	}
	signature := ed25519.Sign(a.private, auth.Message(issued.ChallengeID, a.id, issued.Nonce))
	body = a.mustDo(http.MethodPost, "/v1/auth/verify", map[string]any{
		"challengeId": issued.ChallengeID,
		"signature":   base64.StdEncoding.EncodeToString(signature),
	}, http.StatusOK)
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		a.t.Fatal(err)
	}
	a.token = session.Token
}

func (a *devnetAgent) registerCard(currencies ...string) {
	a.t.Helper()
	a.mustDo(http.MethodPut, "/v1/agents/"+url.PathEscape(a.id)+"/card", domain.AgentCard{
		Name:        "devnet agent",
		Description: "an agent completing a real trade on devnet",
		URL:         "https://agent.example.com",
		Version:     "0.1.0",
		PublicKey:   a.id,
		Currencies:  currencies,
		Skills:      []domain.AgentSkill{{ID: "summarize", Name: "Summarize", Tags: []string{"text"}}},
	}, http.StatusOK)
}

// devnetMarket is the service wired the way main wires it for devnet: the real
// governed mint table for that cluster, the default spending caps, and the
// endpoint checked before anything is served.
func devnetMarket(t *testing.T) (*httptest.Server, tokens.Registry, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "devnet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	signer, _, err := ledger.LoadOrCreateSigner(filepath.Join(dir, "signer.key"))
	if err != nil {
		t.Fatal(err)
	}
	authSvc, err := auth.New(db, []byte("devnet-e2e-session-secret-value-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	mints, err := tokens.ForCluster(cluster.Devnet)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := devnetEndpoint()

	// The same boot check the service runs, against devnet. This is what makes
	// the rest of the test meaningful: the mint addresses below are the ones the
	// chain has just confirmed to be the tokens they claim to be.
	// The default fee policy, which is what devnet runs with. Nothing is paid here
	// because this trade settles off-chain; the policy is in the preflight because
	// the preflight reports the wallet and its rent minimum, and a fee policy it
	// cannot build is a configuration error.
	report, err := preflight.Check(ctx, preflight.Deps{
		Cluster:   cluster.Devnet,
		Endpoint:  endpoint,
		RPC:       settlement.NewRPCClient(endpoint),
		Mints:     mints.List(),
		FeePolicy: fees.Default(),
	})
	if err != nil {
		t.Fatalf("devnet preflight: %v", err)
	}
	if report.Cluster != cluster.Devnet {
		t.Fatalf("preflight cluster = %s, want devnet", report.Cluster)
	}
	t.Logf("devnet genesis %s, %d governed mints verified, fee wallet %s holds %d lamports",
		report.GenesisHash, len(report.Mints), report.FeeWallet, report.FeeBalance)

	policy := limits.DefaultPolicy(limits.DefaultRates())
	led := ledger.New(db, signer)
	registrySvc := registry.New(db, mints, registry.WithPricer(policy))
	trades := trade.New(db, db, db, led).WithLimits(policy, db)
	api := httpapi.New(httpapi.Options{
		Registry:      registrySvc,
		Trades:        trades,
		Auth:          authSvc,
		Ledger:        led,
		Tokens:        mints,
		Version:       "devnet-e2e",
		Cluster:       report.Cluster,
		GenesisHash:   report.GenesisHash,
		PublicBaseURL: "",
	})
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	return server, mints, report.GenesisHash
}

// devnetUSDC is Circle's devnet USDC, a different address from mainnet's. The
// point of using the devnet one is that a mainnet address here would be priced
// but un-spendable, and the trade would fail for reasons that have nothing to do
// with the cap under test.
const devnetUSDC = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"

func TestAFullTradeCompletesOnDevnet(t *testing.T) {
	server, mints, genesis := devnetMarket(t)

	// The caps are in force on this deployment.
	var health map[string]any
	body := mustGet(t, server, "/healthz")
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatal(err)
	}
	if health["settlementTier"] != "beta" {
		t.Errorf("settlementTier = %v, want beta", health["settlementTier"])
	}
	if health["genesisHash"] != genesis {
		t.Errorf("genesisHash = %v, want the devnet hash the preflight read", health["genesisHash"])
	}

	seller := newDevnetAgent(t, server)
	buyer := newDevnetAgent(t, server)
	seller.registerCard(devnetUSDC)
	buyer.registerCard(devnetUSDC)

	// The buyer reads its caps the way any agent would.
	var caps struct {
		PerTrade string `json:"perTradeUsd"`
		PerDay   string `json:"perDayUsd"`
		Currency string `json:"currency"`
	}
	decodeInto(t, buyer.mustDo(http.MethodGet, "/v1/limits", nil, http.StatusOK), &caps)
	if caps.PerTrade != "5.00" || caps.PerDay != "20.00" {
		t.Fatalf("caps on devnet = %s/%s %s, want 5.00/20.00 USD", caps.PerTrade, caps.PerDay, caps.Currency)
	}

	// Twelve fifty is under the default cap, so the seller publishes an offer and
	// the buyer takes it.
	var offer domain.Offer
	decodeInto(t, seller.mustDo(http.MethodPost,
		"/v1/agents/"+url.PathEscape(seller.id)+"/offers", map[string]any{
			"direction":       "ask",
			"description":     "summarize a document",
			"capabilities":    []string{"summarize:document"},
			"priceAmount":     "4.50",
			"priceMint":       devnetUSDC,
			"settlementModes": []string{"offchain"},
			"idempotencyKey":  "devnet-offer-1",
		}, http.StatusCreated), &offer)

	var tr domain.Trade
	decodeInto(t, buyer.mustDo(http.MethodPost, "/v1/trades", map[string]any{
		"offerId":        offer.ID,
		"settlementMode": "offchain",
		"idempotencyKey": "devnet-trade-1",
	}, http.StatusCreated), &tr)
	if tr.Amount.String() != "4.50" {
		t.Errorf("trade amount = %s, want the offer price 4.50", tr.Amount)
	}
	if tr.Mint != devnetUSDC {
		t.Errorf("trade mint = %s, want devnet USDC", tr.Mint)
	}

	buyer.mustDo(http.MethodPost, "/v1/trades/"+url.PathEscape(tr.ID)+"/negotiate", map[string]any{}, http.StatusOK)
	seller.mustDo(http.MethodPost, "/v1/trades/"+url.PathEscape(tr.ID)+"/accept", map[string]any{}, http.StatusOK)
	buyer.mustDo(http.MethodPost, "/v1/trades/"+url.PathEscape(tr.ID)+"/accept", map[string]any{}, http.StatusOK)

	var recordResponse struct {
		Trade   domain.Trade   `json:"trade"`
		Receipt domain.Receipt `json:"receipt"`
		Tessera struct {
			JWS             string               `json:"jws"`
			VerificationKey string               `json:"verificationKey"`
			Claims          ledger.TesseraClaims `json:"claims"`
		} `json:"tessera"`
	}
	decodeInto(t, buyer.mustDo(http.MethodPost, "/v1/trades/"+url.PathEscape(tr.ID)+"/record",
		map[string]any{}, http.StatusOK), &recordResponse)
	if recordResponse.Trade.State != domain.TradeRecorded {
		t.Fatalf("state = %s, want recorded", recordResponse.Trade.State)
	}
	if recordResponse.Receipt.JWS == "" {
		t.Error("the receipt has no signature")
	}

	// The tessera is what an outside agent verifies, and it names the cluster it
	// settles on. An off-chain receipt names none, which is correct: it settled on
	// no chain, so claiming devnet would be asserting something untrue.
	if recordResponse.Receipt.JWS != recordResponse.Tessera.JWS {
		t.Error("the receipt and the tessera carry different signatures")
	}
	if recordResponse.Tessera.VerificationKey == "" {
		t.Error("the tessera does not say which key signed it")
	}
	if recordResponse.Tessera.Claims.Trade.TradeID != tr.ID {
		t.Errorf("tessera tradeId = %s, want %s", recordResponse.Tessera.Claims.Trade.TradeID, tr.ID)
	}
	if recordResponse.Tessera.Claims.Settlement.Cluster != "" {
		t.Errorf("an off-chain tessera names cluster %q, want none", recordResponse.Tessera.Claims.Settlement.Cluster)
	}

	// The same tessera is readable on its own, which is the path an agent uses to
	// verify a trade afterwards.
	var single struct {
		JWS    string `json:"jws"`
		Claims struct {
			Trade struct {
				TradeID string `json:"tradeId"`
			} `json:"trade"`
		} `json:"claims"`
	}
	decodeInto(t, buyer.mustDo(http.MethodGet, "/v1/tesseras/"+url.PathEscape(tr.ID), nil, http.StatusOK), &single)
	if single.JWS != recordResponse.Receipt.JWS {
		t.Error("the tessera endpoint returns a different signature than the record response")
	}
	if single.Claims.Trade.TradeID != tr.ID {
		t.Errorf("tessera endpoint tradeId = %s, want %s", single.Claims.Trade.TradeID, tr.ID)
	}

	// And the mint the trade used is one devnet actually has.
	if _, err := mints.Enabled(devnetUSDC); err != nil {
		t.Errorf("%s is not enabled on this cluster's registry: %v", devnetUSDC, err)
	}
	t.Logf("trade %s settled off-chain on devnet at genesis %s", tr.ID, genesis)
}

func TestATradeOverTheDefaultCapIsRefusedOnDevnet(t *testing.T) {
	server, _, _ := devnetMarket(t)

	seller := newDevnetAgent(t, server)
	buyer := newDevnetAgent(t, server)
	seller.registerCard(devnetUSDC)
	buyer.registerCard(devnetUSDC)

	var offer domain.Offer
	decodeInto(t, seller.mustDo(http.MethodPost,
		"/v1/agents/"+url.PathEscape(seller.id)+"/offers", map[string]any{
			"direction":       "ask",
			"description":     "priced above the default cap",
			"capabilities":    []string{"summarize:document"},
			"priceAmount":     "12.50",
			"priceMint":       devnetUSDC,
			"settlementModes": []string{"offchain"},
			"idempotencyKey":  "devnet-over-cap",
		}, http.StatusCreated), &offer)

	status, body := buyer.do(http.MethodPost, "/v1/trades", map[string]any{
		"offerId":        offer.ID,
		"settlementMode": "offchain",
		"idempotencyKey": "devnet-over-cap",
	})
	if status != http.StatusConflict {
		t.Fatalf("a 12.50 trade against a 5.00 cap = %d: %s", status, body)
	}
	var refusal struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Code != "SPEND_CAP_EXCEEDED" {
		t.Errorf("code = %s, want SPEND_CAP_EXCEEDED", refusal.Code)
	}
}

func mustGet(t *testing.T, server *httptest.Server, path string) []byte {
	t.Helper()
	resp, err := server.Client().Get(server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, body)
	}
	return body
}

func decodeInto(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
