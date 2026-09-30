package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/auth"
	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/httpapi"
	"github.com/douglasdemaio/vtessera/internal/ledger"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/store"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/douglasdemaio/vtessera/internal/trade"
	"github.com/gagliardetto/solana-go"
)

const onchainSignature = "3n1kM1LPRPRTzXcQKCSGyHZUnQVXPPiQjRHDXpNzWJmEfjJcEbLKLzsjpmpfd1GJPPfM6P2KSPfrRtGYEPrTQ3mB"

// stubBuilder stands in for the RPC-backed builder: the HTTP contract under test
// is what the buyer receives, not how the transaction is assembled.
type stubBuilder struct{}

func (stubBuilder) Build(_ context.Context, terms settlement.Terms) (settlement.Build, error) {
	now := time.Now().UTC()
	return settlement.Build{
		Request: settlement.Request{
			TradeID:    terms.TradeID,
			Blockhash:  "9zjePBqC5DBShJ7wzSjPQFPQjk6jJKAJgYbn3yiv1sPk",
			LastValid:  987654,
			UnsignedTx: "AQID",
			BuyerATA:   "buyer-ata",
			SellerATA:  "seller-ata",
			CreatedATA: true,
			CreatedAt:  now,
			ExpiresAt:  now.Add(90 * time.Second),
		},
		Terms: terms,
	}, nil
}

type stubVerifier struct{ err error }

func (v stubVerifier) Verify(settlement.Terms, *solana.Transaction) error { return v.err }

// stubChain scripts the chain's answer to a confirmation, and answers the
// genesis re-read with the cluster under test so a request is not refused for a
// mismatch the stub invented.
type stubChain struct {
	fetched      settlement.Fetched
	err          error
	calls        int
	genesis      string
	genesisCalls int
	genesisErr   error
}

func (c *stubChain) GetGenesisHash(context.Context) (string, error) {
	c.genesisCalls++
	if c.genesisErr != nil {
		return "", c.genesisErr
	}
	return c.genesis, nil
}

func (c *stubChain) Transaction(context.Context, solana.Signature) (settlement.Fetched, error) {
	c.calls++
	return c.fetched, c.err
}

// settlementSetup builds a server whose trade service settles on chain, so the
// HTTP contract can be exercised without a validator.
func settlementSetup(t *testing.T, verifier stubVerifier, chain *stubChain) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
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
	mints, err := tokens.ForCluster(cluster.MainnetBeta)
	if err != nil {
		t.Fatalf("governed mints: %v", err)
	}
	// A stubChain built by a caller without a genesis answer must not be read as
	// a cluster mismatch, so seed the one the harness declares.
	if chain.genesis == "" {
		chain.genesis = cluster.MainnetBeta.GenesisHash()
	}
	led := ledger.New(db, signer)
	trades := trade.New(db, db, db, led).WithSettlement(trade.SettlementDeps{
		Cluster:  cluster.MainnetBeta,
		Registry: mints,
		// Mint verification is left unset on purpose: these tests cover the HTTP
		// contract, and a stub chain has no mint accounts to verify against.
		// The verifier itself is tested in internal/settlement.
		Policy:    fees.Default(),
		Builder:   stubBuilder{},
		Verifier:  verifier,
		Chain:     chain,
		Genesis:   chain,
		Store:     db,
		TradeList: db,
		Confirm:   trade.ConfirmPolicy{Attempts: 1},
		Sleep:     func(context.Context, time.Duration) error { return nil },
	})
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Registry:    registry.New(db, mints),
		Trades:      trades,
		Auth:        authSvc,
		Ledger:      led,
		Tokens:      mints,
		Version:     "0.1.0-test",
		Cluster:     cluster.MainnetBeta,
		GenesisHash: cluster.MainnetBeta.GenesisHash(),
	}))
	t.Cleanup(server.Close)
	return server, led
}

// getTrade reads a trade as the given party, so assertions read the server's
// own view rather than a local copy.
func getTrade(t *testing.T, c *agentClient, tradeID string) domain.Trade {
	t.Helper()
	var tr domain.Trade
	_, body := c.raw(http.MethodGet, "/v1/trades/"+tradeID, nil, true)
	decodeInto(t, body, &tr)
	return tr
}

// acceptedOnchain drives the negotiation to an accepted on-chain trade. Both
// parties must accept, which is also how the E2E suite reaches settlement.
func acceptedOnchain(t *testing.T, buyer, seller *agentClient, offer domain.Offer) domain.Trade {
	t.Helper()
	var tr domain.Trade
	decodeInto(t, buyer.do(http.MethodPost, "/v1/trades", map[string]any{
		"offerId": offer.ID, "settlementMode": "onchain", "idempotencyKey": "trade-1",
	}, true), &tr)
	buyer.do(http.MethodPost, "/v1/trades/"+tr.ID+"/negotiate", nil, true)
	seller.do(http.MethodPost, "/v1/trades/"+tr.ID+"/accept", nil, true)
	buyer.do(http.MethodPost, "/v1/trades/"+tr.ID+"/accept", nil, true)
	tr = getTrade(t, buyer, tr.ID)
	if tr.State != domain.TradeAccepted {
		t.Fatalf("state = %s, want accepted before settlement tests run", tr.State)
	}
	return tr
}

func TestBuildSettlementReturnsAnUnsignedTransactionAndNeverASignature(t *testing.T) {
	chain := &stubChain{}
	server, _ := settlementSetup(t, stubVerifier{}, chain)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")
	tr := acceptedOnchain(t, buyer, seller, offer)

	status, body := buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/settlement", nil, true)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", status, body)
	}
	var payload struct {
		Settlement struct {
			RequestID   string `json:"requestId"`
			UnsignedTx  string `json:"unsignedTx"`
			Blockhash   string `json:"blockhash"`
			FeeLamports uint64 `json:"feeLamports"`
			FeeWallet   string `json:"feeWallet"`
			CreatedATA  bool   `json:"createdSellerAta"`
			ExpiresAt   string `json:"blockhashExpiresAt"`
			SellerATA   string `json:"sellerAta"`
		} `json:"settlement"`
		Trade domain.Trade `json:"trade"`
	}
	decodeInto(t, body, &payload)
	if payload.Settlement.UnsignedTx == "" || payload.Settlement.RequestID == "" {
		t.Fatalf("settlement = %+v, want an unsigned transaction to sign", payload.Settlement)
	}
	if payload.Settlement.FeeLamports != fees.DefaultLamports || payload.Settlement.FeeWallet != fees.DefaultWallet {
		t.Errorf("fee = %d/%s, want the enforced policy %d/%s", payload.Settlement.FeeLamports,
			payload.Settlement.FeeWallet, fees.DefaultLamports, fees.DefaultWallet)
	}
	if !payload.Settlement.CreatedATA || payload.Settlement.SellerATA == "" {
		t.Error("settlement must disclose the seller ATA it will create, so the buyer can verify it")
	}
	if payload.Trade.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want settlement_pending", payload.Trade.State)
	}
	// The status is discoverable, and it never carries a signature.
	status, body = buyer.raw(http.MethodGet, "/v1/trades/"+tr.ID+"/settlement", nil, true)
	if status != http.StatusOK {
		t.Fatalf("GET settlement = %d, want 200: %s", status, body)
	}
	var current struct {
		RequestID string `json:"requestId"`
		Signature string `json:"signature"`
	}
	decodeInto(t, body, &current)
	if current.RequestID != payload.Settlement.RequestID {
		t.Errorf("requestId = %s, want the live request %s", current.RequestID, payload.Settlement.RequestID)
	}
	if current.Signature != "" {
		t.Errorf("signature = %q, want none: the service never signs", current.Signature)
	}
}

func TestOnlyTheBuyerMayRequestSettlement(t *testing.T) {
	server, _ := settlementSetup(t, stubVerifier{}, &stubChain{})
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")
	tr := acceptedOnchain(t, buyer, seller, offer)

	status, body := seller.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/settlement", nil, true)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: only the buyer funds and signs: %s", status, body)
	}
	var apiErr struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &apiErr)
	if apiErr.Code != "FORBIDDEN" {
		t.Errorf("code = %s, want FORBIDDEN", apiErr.Code)
	}
}

func TestSettlementRoutesRejectUnauthenticatedCallers(t *testing.T) {
	server, _ := settlementSetup(t, stubVerifier{}, &stubChain{})
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")
	tr := acceptedOnchain(t, buyer, seller, offer)

	// A signed-in stranger, and a caller with no session at all.
	outsider := newAgent(t, server)
	for _, tc := range []struct {
		name   string
		client *agentClient
		authed bool
	}{
		{"no session", buyer, false},
		{"another agent", outsider, true},
	} {
		status, _ := tc.client.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/settlement", nil, tc.authed)
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Errorf("%s build settlement = %d, want 401 or 403", tc.name, status)
		}
		status, _ = tc.client.raw(http.MethodGet, "/v1/trades/"+tr.ID+"/settlement", nil, tc.authed)
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Errorf("%s read settlement = %d, want 401 or 403", tc.name, status)
		}
	}
}

func TestConfirmReportsAnInvisibleSignatureAsPendingAndNotDisputed(t *testing.T) {
	chain := &stubChain{err: settlement.ErrTransactionNotFound}
	server, _ := settlementSetup(t, stubVerifier{}, chain)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")
	tr := acceptedOnchain(t, buyer, seller, offer)
	buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/settlement", nil, true)

	status, body := buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/confirm",
		map[string]any{"signature": onchainSignature}, true)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: an unseen signature is pending, not a dispute: %s", status, body)
	}
	var apiErr struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &apiErr)
	if apiErr.Code != "SETTLEMENT_PENDING" {
		t.Errorf("code = %s, want SETTLEMENT_PENDING", apiErr.Code)
	}
	after := getTrade(t, buyer, tr.ID)
	if after.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want settlement_pending", after.State)
	}
}

func TestConfirmMismatchIsAConflictAndIssuesNoTessera(t *testing.T) {
	chain := &stubChain{fetched: settlement.Fetched{Transaction: &solana.Transaction{}}}
	server, led := settlementSetup(t, stubVerifier{err: settlement.ErrMismatch}, chain)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")
	tr := acceptedOnchain(t, buyer, seller, offer)
	buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/settlement", nil, true)

	status, body := buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/confirm",
		map[string]any{"signature": onchainSignature}, true)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a mismatched transaction: %s", status, body)
	}
	var apiErr struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &apiErr)
	if apiErr.Code != "SETTLEMENT_MISMATCH" {
		t.Errorf("code = %s, want SETTLEMENT_MISMATCH", apiErr.Code)
	}
	after := getTrade(t, buyer, tr.ID)
	if after.State != domain.TradeDisputed {
		t.Errorf("state = %s, want disputed", after.State)
	}
	if _, err := led.Receipt(context.Background(), tr.ID); err == nil {
		t.Error("a disputed trade must never carry a tessera")
	}
}

func TestConfirmSettledTransactionIssuesATessera(t *testing.T) {
	chain := &stubChain{fetched: settlement.Fetched{Transaction: &solana.Transaction{}}}
	server, _ := settlementSetup(t, stubVerifier{}, chain)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")
	tr := acceptedOnchain(t, buyer, seller, offer)
	buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/settlement", nil, true)

	status, body := buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/confirm",
		map[string]any{"signature": onchainSignature}, true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var settled struct {
		Receipt domain.Receipt `json:"receipt"`
		Tessera struct {
			JWS             string               `json:"jws"`
			VerificationKey string               `json:"verificationKey"`
			Claims          ledger.TesseraClaims `json:"claims"`
		} `json:"tessera"`
		Trade domain.Trade `json:"trade"`
	}
	decodeInto(t, body, &settled)
	if settled.Receipt.TradeID != tr.ID {
		t.Errorf("receipt trade = %s, want %s", settled.Receipt.TradeID, tr.ID)
	}
	if settled.Tessera.Claims.Settlement.Signature != onchainSignature {
		t.Errorf("tessera signature = %q, want %s", settled.Tessera.Claims.Settlement.Signature, onchainSignature)
	}
	if settled.Tessera.Claims.Settlement.Mode != domain.SettlementOnchain {
		t.Errorf("tessera mode = %s, want onchain", settled.Tessera.Claims.Settlement.Mode)
	}
	if settled.Trade.State != domain.TradeSettled {
		t.Errorf("state = %s, want settled", settled.Trade.State)
	}
	// Confirmation is idempotent: the buyer retrying gets the same tessera.
	status, retry := buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/confirm",
		map[string]any{"signature": onchainSignature}, true)
	if status != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", status, retry)
	}
	var again struct {
		Receipt domain.Receipt `json:"receipt"`
	}
	decodeInto(t, retry, &again)
	if again.Receipt.ID != settled.Receipt.ID {
		t.Errorf("retry receipt = %s, want the original %s", again.Receipt.ID, settled.Receipt.ID)
	}
}

func TestConfirmRejectsAMalformedSignature(t *testing.T) {
	chain := &stubChain{}
	server, _ := settlementSetup(t, stubVerifier{}, chain)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")
	tr := acceptedOnchain(t, buyer, seller, offer)
	buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/settlement", nil, true)

	status, body := buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/confirm",
		map[string]any{"signature": "not-a-signature"}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed signature: %s", status, body)
	}
	var apiErr struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &apiErr)
	if apiErr.Code != "INVALID_SIGNATURE" {
		t.Errorf("code = %s, want INVALID_SIGNATURE", apiErr.Code)
	}
	after := getTrade(t, buyer, tr.ID)
	if after.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want settlement_pending: a bad request is not evidence of a dispute", after.State)
	}
}

func TestOnchainSettlementIsRefusedBeforeAnyTradeExistsWhenTheServiceHasNoRPC(t *testing.T) {
	server, _ := setupServer(t)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")

	// The refusal happens at creation, so an on-chain trade can never exist that
	// the service is unable to settle.
	status, body := buyer.raw(http.MethodPost, "/v1/trades", map[string]any{
		"offerId": offer.ID, "settlementMode": "onchain", "idempotencyKey": "trade-1",
	}, true)
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 so on-chain is never half-handled: %s", status, body)
	}
	var apiErr struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &apiErr)
	if apiErr.Code != "ONCHAIN_UNAVAILABLE" {
		t.Errorf("code = %s, want ONCHAIN_UNAVAILABLE", apiErr.Code)
	}
}

func TestTokensPublishesTheGovernedMintsAndTheFeeTheBuyerPays(t *testing.T) {
	server, _ := settlementSetup(t, stubVerifier{}, &stubChain{})
	// The token list is public discovery, so no session is needed.
	anon := &agentClient{t: t, base: server.URL, http: server.Client()}
	status, body := anon.raw(http.MethodGet, "/v1/tokens", nil, false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 without authentication: %s", status, body)
	}
	var payload struct {
		Tokens []struct {
			Address  string `json:"address"`
			Symbol   string `json:"symbol"`
			Decimals int    `json:"decimals"`
			Enabled  bool   `json:"enabled"`
		} `json:"tokens"`
		SettlementFee struct {
			Lamports uint64 `json:"lamports"`
			Wallet   string `json:"wallet"`
			Payer    string `json:"payer"`
		} `json:"settlementFee"`
	}
	decodeInto(t, body, &payload)
	addresses := map[string]bool{}
	for _, token := range payload.Tokens {
		addresses[token.Address] = token.Enabled
	}
	for _, want := range []string{usdc, eurc} {
		if !addresses[want] {
			t.Errorf("token %s missing from the published allowlist: %s", want, body)
		}
	}
	if payload.SettlementFee.Lamports != fees.DefaultLamports || payload.SettlementFee.Wallet != fees.DefaultWallet {
		t.Errorf("fee = %+v, want the enforced policy", payload.SettlementFee)
	}
	if payload.SettlementFee.Payer != "buyer" {
		t.Errorf("payer = %s, want buyer: the buyer funds the fee", payload.SettlementFee.Payer)
	}
}

func TestTokensAreUnavailableRatherThanMisleadingWhenUnconfigured(t *testing.T) {
	server, _ := setupServerAt(t, "", unconfigured{})
	anon := &agentClient{t: t, base: server.URL, http: server.Client()}
	status, body := anon.raw(http.MethodGet, "/v1/tokens", nil, false)
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 with no token registry: %s", status, body)
	}
	var apiErr struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &apiErr)
	if apiErr.Code != "NOT_CONFIGURED" {
		t.Errorf("code = %s, want NOT_CONFIGURED", apiErr.Code)
	}
}

func TestAnUnreachableChainIs503RatherThan501(t *testing.T) {
	chain := &stubChain{}
	server, _ := settlementSetup(t, stubVerifier{}, chain)
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")
	tr := acceptedOnchain(t, buyer, seller, offer)

	// The endpoint stopped answering. The feature is switched on here, so a
	// client that retries may succeed, and a 501 would tell an operator to fix
	// a configuration that is already correct.
	chain.genesisErr = errors.New("dial tcp: i/o timeout")
	status, body := buyer.raw(http.MethodPost, "/v1/trades/"+tr.ID+"/settlement", nil, true)
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for an unreachable chain: %s", status, body)
	}
	var apiErr struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &apiErr)
	if apiErr.Code != "ONCHAIN_UNAVAILABLE" {
		t.Errorf("code = %s, want ONCHAIN_UNAVAILABLE for an unreachable chain", apiErr.Code)
	}
}

func TestAnUnconfiguredDeploymentRefusesOnchainTradesWith501(t *testing.T) {
	server, _ := setupServerAt(t, "", unconfigured{})
	seller := newAgent(t, server)
	buyer := newAgent(t, server)
	offer := seller.publishOffer("12.50", usdc, "summarize:document")

	// The refusal lands at trade creation rather than at settlement: an on-chain
	// trade that can never settle should not be recorded as agreed. 501 says the
	// capability is absent here, and no amount of retrying will change that.
	status, body := buyer.raw(http.MethodPost, "/v1/trades", map[string]any{
		"offerId": offer.ID, "settlementMode": "onchain", "idempotencyKey": "trade-1",
	}, true)
	if status != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 when settlement is unconfigured: %s", status, body)
	}
	var apiErr struct {
		Code string `json:"code"`
	}
	decodeInto(t, body, &apiErr)
	if apiErr.Code != "ONCHAIN_UNAVAILABLE" {
		t.Errorf("code = %s, want ONCHAIN_UNAVAILABLE when settlement is unconfigured", apiErr.Code)
	}
}

func TestTheTokenListNamesTheClusterItDescribes(t *testing.T) {
	chain := &stubChain{}
	server, _ := settlementSetup(t, stubVerifier{}, chain)
	anon := &agentClient{t: t, base: server.URL, http: server.Client()}
	_, body := anon.raw(http.MethodGet, "/v1/tokens", nil, false)
	var payload struct {
		Cluster     string `json:"cluster"`
		GenesisHash string `json:"genesisHash"`
		Tokens      []struct {
			Address string `json:"address"`
			Cluster string `json:"cluster"`
		} `json:"tokens"`
	}
	decodeInto(t, body, &payload)
	// A mint address names an account on one chain. Without the cluster, the
	// list reintroduces exactly the ambiguity the deployment otherwise removes.
	if payload.Cluster != "mainnet-beta" {
		t.Errorf("cluster = %q, want mainnet-beta", payload.Cluster)
	}
	if payload.GenesisHash != cluster.MainnetBeta.GenesisHash() {
		t.Errorf("genesisHash = %q, want %s", payload.GenesisHash, cluster.MainnetBeta.GenesisHash())
	}
	for _, tok := range payload.Tokens {
		if tok.Cluster != payload.Cluster {
			t.Errorf("mint %s claims cluster %q, list says %q", tok.Address, tok.Cluster, payload.Cluster)
		}
	}
}
