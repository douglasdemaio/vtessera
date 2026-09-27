//go:build solana

package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/auth"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/httpapi"
	"github.com/douglasdemaio/vtessera/internal/ledger"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/store"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/douglasdemaio/vtessera/internal/trade"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/mr-tron/base58"
)

const sessionSecret = "e2e-session-secret-e2e-session-secret"

// agent is a real marketplace agent whose Ed25519 key is also its Solana wallet.
// The agent ID is the base58 public key, so the same key signs the API session
// and the settlement transaction: the service never sees the private half.
type agent struct {
	t     *testing.T
	base  string
	http  *http.Client
	id    string
	key   solana.PrivateKey
	edKey ed25519.PrivateKey
	token string
}

func newAgent(t *testing.T, server *httptest.Server) *agent {
	t.Helper()
	// A 32-byte Ed25519 seed is a valid Solana private key, so one key does both
	// jobs: authenticating the API session and signing the settlement. The agent
	// ID is that key's public half, which is the wallet the service will use.
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	private := ed25519.NewKeyFromSeed(seed)
	a := &agent{
		t:     t,
		base:  server.URL,
		http:  server.Client(),
		id:    base58.Encode(private.Public().(ed25519.PublicKey)),
		key:   solana.PrivateKey(private),
		edKey: private,
	}
	a.authenticate()
	return a
}

func (a *agent) authenticate() {
	a.t.Helper()
	var issued struct {
		ChallengeID string `json:"challengeId"`
		Nonce       string `json:"nonce"`
	}
	body := a.do(http.MethodPost, "/v1/auth/challenge", map[string]any{"agentId": a.id}, false, http.StatusCreated)
	if err := json.Unmarshal(body, &issued); err != nil {
		a.t.Fatal(err)
	}
	signature := ed25519.Sign(a.edKey, auth.Message(issued.ChallengeID, a.id, issued.Nonce))
	body = a.do(http.MethodPost, "/v1/auth/verify", map[string]any{
		"challengeId": issued.ChallengeID,
		"signature":   base64.StdEncoding.EncodeToString(signature),
	}, false, http.StatusOK)
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		a.t.Fatal(err)
	}
	a.token = session.Token
}

// registerCard publishes an agent card that accepts the test mint.
func (a *agent) registerCard(mint string) {
	a.t.Helper()
	a.do(http.MethodPut, "/v1/agents/"+url.PathEscape(a.id)+"/card", domain.AgentCard{
		Name:            "settling agent",
		Description:     "settles on chain",
		URL:             "https://agent.example.com",
		Version:         "0.1.0",
		PublicKey:       a.id,
		Capabilities:    []string{"summarize:document"},
		Currencies:      []string{mint},
		SettlementModes: []domain.SettlementMode{domain.SettlementOffchain, domain.SettlementOnchain},
	}, true, http.StatusOK)
}

func (a *agent) do(method, path string, payload any, authed bool, wantStatus int) []byte {
	a.t.Helper()
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			a.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, a.base+path, reader)
	if err != nil {
		a.t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authed {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		a.t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		a.t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, wantStatus, body)
	}
	return body
}

// errorCode is the marketplace error code from a failed call, for assertions.
func (a *agent) errorCode(method, path string, payload any, authed bool, wantStatus int) string {
	a.t.Helper()
	body := a.do(method, path, payload, authed, wantStatus)
	var decoded struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		a.t.Fatalf("decode error body %s: %v", body, err)
	}
	return decoded.Code
}

type settlementResponse struct {
	Settlement struct {
		RequestID          string `json:"requestId"`
		UnsignedTx         string `json:"unsignedTx"`
		Blockhash          string `json:"blockhash"`
		BlockhashExpiresAt string `json:"blockhashExpiresAt"`
		FeeLamports        uint64 `json:"feeLamports"`
		FeeWallet          string `json:"feeWallet"`
		BuyerAta           string `json:"buyerAta"`
		SellerAta          string `json:"sellerAta"`
		CreatedSellerAta   bool   `json:"createdSellerAta"`
	} `json:"settlement"`
	Trade domain.Trade `json:"trade"`
}

type confirmResponse struct {
	Trade   domain.Trade   `json:"trade"`
	Receipt domain.Receipt `json:"receipt"`
}

// buildSettlement runs the §6.2 build flow and returns the unsigned transaction
// the service issued.
func (a *agent) buildSettlement(tradeID string) settlementResponse {
	a.t.Helper()
	body := a.do(http.MethodPost, "/v1/trades/"+url.PathEscape(tradeID)+"/settlement", nil, true, http.StatusCreated)
	var out settlementResponse
	if err := json.Unmarshal(body, &out); err != nil {
		a.t.Fatal(err)
	}
	return out
}

func (a *agent) confirm(tradeID, signature string) (confirmResponse, int, string) {
	a.t.Helper()
	body := a.do(http.MethodPost, "/v1/trades/"+url.PathEscape(tradeID)+"/confirm",
		map[string]any{"signature": signature}, true, http.StatusOK)
	var out confirmResponse
	if err := json.Unmarshal(body, &out); err != nil {
		a.t.Fatal(err)
	}
	return out, http.StatusOK, ""
}

// market is a running marketplace wired for on-chain settlement against the
// test validator, with a single governed test mint.
type market struct {
	server *httptest.Server
	trades *trade.Service
	ledger *ledger.Ledger
	mints  tokens.Registry
	policy fees.Policy
	mint   solana.PublicKey
	bank   solana.PrivateKey
}

// newMarket funds its own bank, for tests that need no chain setup of their own.
func newMarket(t *testing.T, mint solana.PublicKey, policy fees.Policy) *market {
	t.Helper()
	bank := bank(t)
	return newMarketWithBank(t, mint, policy, bank)
}

func newMarketWithBank(t *testing.T, mint solana.PublicKey, policy fees.Policy, bank solana.PrivateKey) *market {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	signer, _, err := ledger.LoadOrCreateSigner(filepath.Join(dir, "signer.key"))
	if err != nil {
		t.Fatal(err)
	}
	authSvc, err := auth.New(db, []byte(sessionSecret))
	if err != nil {
		t.Fatal(err)
	}
	mints, err := tokens.New([]tokens.Token{{
		Address:  mint.String(),
		Symbol:   "TUSD",
		Decimals: 6,
		Enabled:  true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	led := ledger.New(db, signer)
	registrySvc := registry.New(db, registry.WithMints(mints))
	client := settlement.NewRPCClient(testRPCURL())
	trades := trade.New(db, db, db, led).WithSettlement(trade.SettlementDeps{
		Registry: mints,
		Policy:   policy,
		Builder:  settlement.NewBuilder(client),
		Verifier: settlement.NewVerifier(),
		Chain:    client,
		Store:    db,
	})
	api := httpapi.New(httpapi.Options{
		Registry: registrySvc,
		Trades:   trades,
		Auth:     authSvc,
		Ledger:   led,
		Tokens:   mints,
		Version:  "e2e",
	})
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	// On a real cluster the service fee wallet already exists and holds SOL. A
	// fresh local validator has no such account, and a 500000 lamport transfer
	// cannot create one, so the account is opened here exactly as it exists in
	// production. This also lets the tests assert the exact fee received.
	fundAccount(t, bank, policy.Wallet(), 1_000_000_000)
	return &market{
		server: server,
		trades: trades,
		ledger: led,
		mints:  mints,
		policy: policy,
		mint:   mint,
		bank:   bank,
	}
}

// openOnchainTrade walks two funded agents through the whole lifecycle up to the
// point where the service will issue a settlement transaction.
func (m *market) openOnchainTrade(t *testing.T, buyer, seller *agent, amount string) domain.Trade {
	t.Helper()
	buyer.registerCard(m.mint.String())
	seller.registerCard(m.mint.String())
	body := seller.do(http.MethodPost, "/v1/agents/"+url.PathEscape(seller.id)+"/offers", map[string]any{
		"direction":       string(domain.DirectionAsk),
		"description":     "summarize a document",
		"capabilities":    []string{"summarize:document"},
		"priceAmount":     amount,
		"priceMint":       m.mint.String(),
		"settlementModes": []string{"onchain"},
	}, true, http.StatusCreated)
	var offer domain.Offer
	if err := json.Unmarshal(body, &offer); err != nil {
		t.Fatal(err)
	}
	body = buyer.do(http.MethodPost, "/v1/trades", map[string]any{
		"offerId":        offer.ID,
		"settlementMode": string(domain.SettlementOnchain),
		"idempotencyKey": "e2e-" + offer.ID,
	}, true, http.StatusCreated)
	var tr domain.Trade
	if err := json.Unmarshal(body, &tr); err != nil {
		t.Fatal(err)
	}
	buyer.do(http.MethodPost, "/v1/trades/"+url.PathEscape(tr.ID)+"/negotiate", nil, true, http.StatusOK)
	seller.do(http.MethodPost, "/v1/trades/"+url.PathEscape(tr.ID)+"/accept", nil, true, http.StatusOK)
	buyer.do(http.MethodPost, "/v1/trades/"+url.PathEscape(tr.ID)+"/accept", nil, true, http.StatusOK)
	body = buyer.do(http.MethodGet, "/v1/trades/"+url.PathEscape(tr.ID), nil, true, http.StatusOK)
	if err := json.Unmarshal(body, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.State != domain.TradeAccepted {
		t.Fatalf("state = %s, want accepted before settlement can be built", tr.State)
	}
	return tr
}

// fundAccount opens a rent-exempt account at the address, mirroring a wallet that
// already exists on chain. Transfers are used rather than further airdrops
// because a validator's faucet is the slowest thing in the test.
func fundAccount(t *testing.T, from solana.PrivateKey, address solana.PublicKey, lamports uint64) {
	t.Helper()
	c := client(t)
	accountExists(t, c, address)
	tx, err := solana.NewTransaction([]solana.Instruction{
		system.NewTransferInstruction(lamports, from.PublicKey(), address).Build(),
	}, solana.Hash{}, solana.TransactionPayer(from.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	confirm(t, c, send(t, c, tx, from))
}

// feeBalance reads the service wallet balance, for asserting the exact fee paid.
func feeBalance(t *testing.T, c *rpc.Client, wallet solana.PublicKey) uint64 {
	t.Helper()
	res, err := c.GetBalance(context.Background(), wallet, rpc.CommitmentFinalized)
	if err != nil {
		t.Fatal(err)
	}
	return res.Value
}

// testMint creates a 6-decimal SPL mint on the validator and returns it, standing
// in for USDC, which does not exist on a local cluster.
func testMint(t *testing.T, bank solana.PrivateKey) (mint solana.PublicKey, authority solana.PrivateKey) {
	t.Helper()
	c := client(t)
	authority = newKey(t)
	// The mint authority must fund the mint account and still keep its own
	// rent-exempt minimum, so it needs more than the mint's rent.
	fundAccount(t, bank, authority.PublicKey(), 2*solana.LAMPORTS_PER_SOL)
	mintKey := newKey(t)
	mintTx, err := solana.NewTransaction([]solana.Instruction{
		system.NewCreateAccountInstruction(solana.LAMPORTS_PER_SOL, 82, solana.TokenProgramID,
			authority.PublicKey(), mintKey.PublicKey()).Build(),
		token.NewInitializeMint2Instruction(6, authority.PublicKey(), solana.PublicKey{}, mintKey.PublicKey()).Build(),
	}, solana.Hash{}, solana.TransactionPayer(authority.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	confirm(t, c, send(t, c, mintTx, authority, mintKey))
	return mintKey.PublicKey(), authority
}

// fundBuyer gives the buyer SOL for fees and tokens for the trade, and returns
// the buyer's associated token account.
func fundBuyer(t *testing.T, bank solana.PrivateKey, mint solana.PublicKey, authority solana.PrivateKey, buyer *agent, amount uint64) solana.PublicKey {
	t.Helper()
	c := client(t)
	fundAccount(t, bank, buyer.key.PublicKey(), 1_000_000_000)
	buyerATA, _, err := solana.FindAssociatedTokenAddressWithProgram(buyer.key.PublicKey(), mint, solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	mintTx, err := solana.NewTransaction([]solana.Instruction{
		associatedtokenaccount.NewCreateInstruction(authority.PublicKey(), buyer.key.PublicKey(), mint).Build(),
		token.NewMintToInstruction(amount, mint, buyerATA, authority.PublicKey(), nil).Build(),
	}, solana.Hash{}, solana.TransactionPayer(authority.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	confirm(t, c, send(t, c, mintTx, authority))
	return buyerATA
}

// decodeUnsigned is the buyer's own defence: it deserializes what the service
// handed it and checks the shape before signing anything.
func decodeUnsigned(t *testing.T, encoded string) *solana.Transaction {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("unsignedTx is not base64: %v", err)
	}
	tx, err := solana.TransactionFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// tokenBalance returns the raw base-unit balance. Nodes report the amount either
// as a decimal display string or as raw base units, so both forms are handled
// rather than assuming one and silently scaling an assertion by 10^6.
func tokenBalance(t *testing.T, c *rpc.Client, ata solana.PublicKey) uint64 {
	t.Helper()
	res, err := c.GetTokenAccountBalance(context.Background(), ata, rpc.CommitmentFinalized)
	if err != nil {
		t.Fatal(err)
	}
	raw := res.Value.Amount
	if !strings.Contains(raw, ".") {
		base, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			t.Fatalf("parse raw token balance %q: %v", raw, err)
		}
		return base
	}
	amount, err := money.Parse(raw)
	if err != nil {
		t.Fatalf("parse token balance %q: %v", raw, err)
	}
	base, err := amount.BaseUnits(6)
	if err != nil {
		t.Fatalf("token balance %q: %v", raw, err)
	}
	return base
}

func TestSettlementRoundTripAgainstValidator(t *testing.T) {
	bank := bank(t)
	c := client(t)
	mint, authority := testMint(t, bank)
	policy := fees.Default()
	m := newMarketWithBank(t, mint, policy, bank)
	buyer := newAgent(t, m.server)
	seller := newAgent(t, m.server)
	buyerATA := fundBuyer(t, bank, mint, authority, buyer, 12_500_000)

	tr := m.openOnchainTrade(t, buyer, seller, "12.50")
	feeBefore := feeBalance(t, c, policy.Wallet())

	// §6.2: the service issues an unsigned transaction.
	issued := buyer.buildSettlement(tr.ID)
	if issued.Trade.State != domain.TradeSettlementPending {
		t.Fatalf("state = %s, want settlement_pending", issued.Trade.State)
	}
	if issued.Settlement.FeeLamports != policy.Lamports() || issued.Settlement.FeeWallet != policy.WalletAddress() {
		t.Errorf("fee = %d to %s, want %d to %s", issued.Settlement.FeeLamports,
			issued.Settlement.FeeWallet, policy.Lamports(), policy.WalletAddress())
	}
	if !issued.Settlement.CreatedSellerAta {
		t.Error("the seller's account did not exist, so ATA creation should be included")
	}
	tx := decodeUnsigned(t, issued.Settlement.UnsignedTx)
	// The wire format reserves one signature slot for the buyer; it must be
	// empty, because the service never signs.
	if len(tx.Signatures) != 1 || !tx.Signatures[0].IsZero() {
		t.Errorf("signatures = %v, want a single zero signature the buyer fills", tx.Signatures)
	}
	if len(tx.Message.Instructions) != 4 {
		t.Errorf("instructions = %d, want 4 (ATA, transfer, memo, fee)", len(tx.Message.Instructions))
	}
	if !tx.Message.AccountKeys[0].Equals(buyer.key.PublicKey()) {
		t.Errorf("fee payer = %s, want the buyer", tx.Message.AccountKeys[0])
	}

	// The buyer signs and submits.
	sig := send(t, c, tx, buyer.key)
	confirm(t, c, sig)

	// §6.3: the service verifies what actually landed and issues the tessera.
	result, _, _ := buyer.confirm(tr.ID, sig.String())
	if result.Trade.State != domain.TradeSettled {
		t.Fatalf("state = %s, want settled", result.Trade.State)
	}
	if result.Receipt.JWS == "" {
		t.Fatal("settlement issued no tessera")
	}
	claims, err := m.ledger.Verify(result.Receipt.JWS)
	if err != nil {
		t.Fatalf("tessera does not verify: %v", err)
	}
	if claims.Settlement.Mode != domain.SettlementOnchain {
		t.Errorf("tessera mode = %s, want onchain", claims.Settlement.Mode)
	}
	if claims.Settlement.Signature != sig.String() {
		t.Errorf("tessera signature = %s, want the settlement signature %s", claims.Settlement.Signature, sig)
	}
	if claims.Settlement.LedgerEntryHash == "" {
		t.Error("tessera is not bound to its ledger entry hash")
	}
	sellerATA, _, err := solana.FindAssociatedTokenAddressWithProgram(seller.key.PublicKey(), mint, solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	if got := tokenBalance(t, c, sellerATA); got != 12_500_000 {
		t.Errorf("seller balance = %d, want the full 12500000", got)
	}
	if got := tokenBalance(t, c, buyerATA); got != 0 {
		t.Errorf("buyer balance = %d, want the trade fully paid", got)
	}
	entries, err := m.ledger.Entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].TradeID != tr.ID {
		t.Errorf("ledger entries = %d, want exactly one for %s", len(entries), tr.ID)
	}
	// The fee moved inside the settlement, and the buyer did not overpay.
	if delta := feeBalance(t, c, policy.Wallet()) - feeBefore; delta != policy.Lamports() {
		t.Errorf("fee wallet delta = %d lamports, want exactly %d", delta, policy.Lamports())
	}
}

// TestFeeStrippingEndsDisputed proves the enforcement mechanism: a buyer who
// drops the fee instruction still gets the tokens, but the settlement never
// verifies and the trade goes to disputed with no tessera.
func TestFeeStrippingEndsDisputed(t *testing.T) {
	bank := bank(t)
	mint, authority := testMint(t, bank)
	m := newMarketWithBank(t, mint, fees.Default(), bank)
	buyer := newAgent(t, m.server)
	seller := newAgent(t, m.server)
	fundBuyer(t, bank, mint, authority, buyer, 12_500_000)
	tr := m.openOnchainTrade(t, buyer, seller, "12.50")
	c := client(t)

	issued := buyer.buildSettlement(tr.ID)
	tx := decodeUnsigned(t, issued.Settlement.UnsignedTx)
	// Drop the System Program transfer that pays the service fee.
	tx.Message.Instructions = tx.Message.Instructions[:len(tx.Message.Instructions)-1]
	sig := send(t, c, tx, buyer.key)
	confirm(t, c, sig)

	code := buyer.errorCode(http.MethodPost, "/v1/trades/"+tr.ID+"/confirm",
		map[string]any{"signature": sig.String()}, true, http.StatusConflict)
	if code != "SETTLEMENT_MISMATCH" {
		t.Errorf("code = %s, want SETTLEMENT_MISMATCH", code)
	}
	body := buyer.do(http.MethodGet, "/v1/trades/"+tr.ID, nil, true, http.StatusOK)
	var after domain.Trade
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatal(err)
	}
	if after.State != domain.TradeDisputed {
		t.Errorf("state = %s, want disputed: a stripped fee must never settle", after.State)
	}
	if _, err := m.ledger.Receipt(context.Background(), tr.ID); err == nil {
		t.Error("a disputed trade must never carry a tessera")
	} else if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, ledger.ErrNotSettled) {
		t.Errorf("tessera err = %v, want no receipt for a disputed trade", err)
	}
}

// TestTamperedMemoEndsDisputed proves the memo is verified, so a settlement
// cannot be relabelled as a different trade.
func TestTamperedMemoEndsDisputed(t *testing.T) {
	bank := bank(t)
	mint, authority := testMint(t, bank)
	m := newMarketWithBank(t, mint, fees.Default(), bank)
	buyer := newAgent(t, m.server)
	seller := newAgent(t, m.server)
	fundBuyer(t, bank, mint, authority, buyer, 12_500_000)
	tr := m.openOnchainTrade(t, buyer, seller, "12.50")
	c := client(t)

	issued := buyer.buildSettlement(tr.ID)
	tx := decodeUnsigned(t, issued.Settlement.UnsignedTx)
	// Rewrite the memo in place to name a different trade. The memo program
	// encodes a 4-byte length, the signer count, the signers, then the text.
	forged := "00000000-0000-4000-8000-000000000000"
	memo := []byte{byte(len(forged)), 0, 0, 0, 1}
	memo = append(memo, []byte(forged)...)
	tx.Message.Instructions[len(tx.Message.Instructions)-2].Data = memo
	sig := send(t, c, tx, buyer.key)
	confirm(t, c, sig)

	code := buyer.errorCode(http.MethodPost, "/v1/trades/"+tr.ID+"/confirm",
		map[string]any{"signature": sig.String()}, true, http.StatusConflict)
	if code != "SETTLEMENT_MISMATCH" {
		t.Errorf("code = %s, want SETTLEMENT_MISMATCH", code)
	}
	body := buyer.do(http.MethodGet, "/v1/trades/"+tr.ID, nil, true, http.StatusOK)
	var after domain.Trade
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatal(err)
	}
	if after.State != domain.TradeDisputed {
		t.Errorf("state = %s, want disputed", after.State)
	}
}

// TestSettlementIsBlockedWhileTheTransactionIsLive covers design spec §5: a live
// (unexpired) transaction may already be in flight, so the trade is not
// cancellable until the blockhash lapses.
func TestSettlementIsBlockedWhileTheTransactionIsLive(t *testing.T) {
	bank := bank(t)
	mint, authority := testMint(t, bank)
	m := newMarketWithBank(t, mint, fees.Default(), bank)
	buyer := newAgent(t, m.server)
	seller := newAgent(t, m.server)
	fundBuyer(t, bank, mint, authority, buyer, 12_500_000)
	tr := m.openOnchainTrade(t, buyer, seller, "12.50")
	buyer.buildSettlement(tr.ID)

	code := buyer.errorCode(http.MethodPost, "/v1/trades/"+tr.ID+"/cancel",
		map[string]any{"reason": "changed my mind"}, true, http.StatusConflict)
	if code != "SETTLEMENT_IN_PROGRESS" {
		t.Errorf("code = %s, want SETTLEMENT_IN_PROGRESS", code)
	}
	// The seller may ask for settlement state, but only the buyer may build it.
	if code := seller.errorCode(http.MethodPost, "/v1/trades/"+tr.ID+"/settlement", nil, true, http.StatusForbidden); code != "FORBIDDEN" {
		t.Errorf("seller build code = %s, want FORBIDDEN", code)
	}
}

// TestConfirmOfAnUnseenSignatureIsPendingNotDisputed covers the RPC finality lag
// path from design spec §6.4: a signature the chain has not seen is retryable and
// never an accusation against the counterparty.
func TestConfirmOfAnUnseenSignatureIsPendingNotDisputed(t *testing.T) {
	bank := bank(t)
	mint, _ := testMint(t, bank)
	m := newMarketWithBank(t, mint, fees.Default(), bank)
	buyer := newAgent(t, m.server)
	seller := newAgent(t, m.server)
	tr := m.openOnchainTrade(t, buyer, seller, "12.50")
	// Nothing is submitted in this scenario, so the buyer needs no on-chain funds.
	buyer.buildSettlement(tr.ID)

	// A well-formed signature that was simply never submitted.
	phantom := solana.Signature{}
	if _, err := rand.Read(phantom[:]); err != nil {
		t.Fatal(err)
	}
	code := buyer.errorCode(http.MethodPost, "/v1/trades/"+tr.ID+"/confirm",
		map[string]any{"signature": phantom.String()}, true, http.StatusAccepted)
	if code != "SETTLEMENT_PENDING" {
		t.Errorf("code = %s, want SETTLEMENT_PENDING", code)
	}
	body := buyer.do(http.MethodGet, "/v1/trades/"+tr.ID, nil, true, http.StatusOK)
	var after domain.Trade
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatal(err)
	}
	if after.State != domain.TradeSettlementPending {
		t.Errorf("state = %s, want settlement_pending so the trade is not falsely disputed", after.State)
	}
}
