package settlement

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/gagliardetto/solana-go"
	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/programs/memo"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
)

const (
	usdcMint  = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	blockhash = "GHtXQBsoZHVnNFa9YevAzFr17DJjgHXk3yczTarb5XDd"
	tradeID   = "8f14e45f-ceea-467a-9575-1f1d5f1a1b2c"
)

var (
	buyerPriv = solana.MustPrivateKeyFromBase58("3iVSPd6VVkr8NwM9vccJF6U9A6pi8Ss1n9hiKbkF3UsbyxCF3TgjreGxrxhMLWzZ16WLAF91JLNsHLy4Lv3KwNEE")
	thiefPriv = solana.MustPrivateKeyFromBase58("4tzF9GT5h3wkdzLejCPJJr2tsnPkivWjFkB1Ckqs6WdmPADhPC2ybvZzE8sCyBFXfuAJjApCQ3BRBZP3RYtCt7VX")
	buyerKey  = buyerPriv.PublicKey()
	sellerKey = solana.MustPrivateKeyFromBase58("R43d6RoGJRKR815gb3XLGCK4av3PS7qCwicmnXid3awYySwUZ6FksYxAuao8N7efejAKhvFxST2BXtfgaW8iNC6").PublicKey()
	thiefKey  = thiefPriv.PublicKey()
)

func testTerms(t *testing.T) Terms {
	t.Helper()
	return Terms{
		TradeID:  tradeID,
		Amount:   12_500_000,
		Decimals: 6,
		Mint:     solana.MustPublicKeyFromBase58(usdcMint),
		Buyer:    buyerKey,
		Seller:   sellerKey,
		Fee:      fees.Default(),
	}
}

func acceptedTrade(t *testing.T) domain.Trade {
	t.Helper()
	return domain.Trade{
		ID:             tradeID,
		BuyerAgentID:   buyerKey.String(),
		SellerAgentID:  sellerKey.String(),
		Amount:         money.MustParse("12.5"),
		Mint:           usdcMint,
		SettlementMode: domain.SettlementOnchain,
		State:          domain.TradeAccepted,
	}
}

type fakeClient struct {
	blockhash     solana.Hash
	lastValid     uint64
	accountExists bool
	accountErr    error
	blockhashErr  error
	fetched       Fetched
	fetchedErr    error
}

func (f *fakeClient) LatestBlockhash(context.Context) (solana.Hash, uint64, error) {
	if f.blockhashErr != nil {
		return solana.Hash{}, 0, f.blockhashErr
	}
	return f.blockhash, f.lastValid, nil
}

func (f *fakeClient) AccountExists(context.Context, solana.PublicKey) (bool, error) {
	return f.accountExists, f.accountErr
}

func (f *fakeClient) Transaction(context.Context, solana.Signature) (Fetched, error) {
	return f.fetched, f.fetchedErr
}

func newFake() *fakeClient {
	return &fakeClient{blockhash: solana.MustHashFromBase58(blockhash), lastValid: 1000}
}

// --- instruction helpers, the way a well-behaved or a tampering client builds ---

func transferInstruction(t Terms) solana.Instruction {
	return transferInstructionWithDecimals(t, t.Decimals)
}

func transferInstructionWithDecimals(t Terms, decimals int) solana.Instruction {
	buyerATA, _ := t.BuyerATA()
	sellerATA, _ := t.SellerATA()
	return token.NewTransferCheckedInstructionBuilder().
		SetAmount(t.Amount).
		SetDecimals(uint8(decimals)).
		SetSourceAccount(buyerATA).
		SetMintAccount(t.Mint).
		SetDestinationAccount(sellerATA).
		SetOwnerAccount(t.Buyer).
		Build()
}

func memoInstruction(t Terms, text string) solana.Instruction {
	return memo.NewMemoInstruction([]byte(text), t.Buyer).Build()
}

func feeInstruction(t Terms) solana.Instruction {
	return system.NewTransferInstruction(t.Fee.Lamports(), t.Buyer, t.Fee.Wallet()).Build()
}

func assembleTx(t *testing.T, instructions []solana.Instruction, payer solana.PublicKey) *solana.Transaction {
	t.Helper()
	tx, err := solana.NewTransaction(instructions, solana.MustHashFromBase58(blockhash), solana.TransactionPayer(payer))
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// assembleCanonical builds the canonical settlement transaction for terms,
// which is how a client acting honestly would reconstruct it.
func assembleCanonical(t *testing.T, terms Terms, sellerATAExists bool) *solana.Transaction {
	t.Helper()
	instructions, err := expectedInstructions(terms, sellerATAExists)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := assemble(instructions, solana.MustHashFromBase58(blockhash), terms.Buyer)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func signAs(t *testing.T, tx *solana.Transaction, keys ...solana.PrivateKey) *solana.Transaction {
	t.Helper()
	if _, err := tx.Sign(func(pub solana.PublicKey) *solana.PrivateKey {
		for i := range keys {
			if keys[i].PublicKey().Equals(pub) {
				return &keys[i]
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return tx
}

// --- terms ---

func TestNewTermsFromAcceptedTrade(t *testing.T) {
	terms, err := NewTerms(acceptedTrade(t), tokens.Default(), fees.Default())
	if err != nil {
		t.Fatal(err)
	}
	if terms.Amount != 12_500_000 {
		t.Errorf("amount = %d, want 12500000 base units", terms.Amount)
	}
	if terms.Decimals != 6 {
		t.Errorf("decimals = %d, want 6", terms.Decimals)
	}
	if !terms.Buyer.Equals(buyerKey) || !terms.Seller.Equals(sellerKey) {
		t.Errorf("parties = %s/%s, want the agent ids used as wallets", terms.Buyer, terms.Seller)
	}
	if string(terms.Memo()) != tradeID {
		t.Errorf("memo = %q, want the trade id", terms.Memo())
	}
}

func TestNewTermsRejectsOffChainAndWrongState(t *testing.T) {
	tr := acceptedTrade(t)
	tr.SettlementMode = domain.SettlementOffchain
	if _, err := NewTerms(tr, tokens.Default(), fees.Default()); !errors.Is(err, ErrTradeNotSettleable) {
		t.Errorf("off-chain trade: err = %v, want ErrTradeNotSettleable", err)
	}
	tr = acceptedTrade(t)
	tr.State = domain.TradeProposed
	if _, err := NewTerms(tr, tokens.Default(), fees.Default()); !errors.Is(err, ErrTradeNotSettleable) {
		t.Errorf("proposed trade: err = %v, want ErrTradeNotSettleable", err)
	}
}

func TestNewTermsRejectsUnregisteredMint(t *testing.T) {
	tr := acceptedTrade(t)
	tr.Mint = thiefKey.String()
	if _, err := NewTerms(tr, tokens.Default(), fees.Default()); !errors.Is(err, tokens.ErrNotRegistered) {
		t.Errorf("err = %v, want ErrNotRegistered", err)
	}
}

func TestNewTermsRejectsDisabledMint(t *testing.T) {
	registry, err := tokens.New([]tokens.Token{{
		Address: usdcMint, Symbol: "USDC", Decimals: 6, Enabled: false,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTerms(acceptedTrade(t), registry, fees.Default()); !errors.Is(err, tokens.ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
}

func TestNewTermsRejectsAmountTooPreciseForMint(t *testing.T) {
	tr := acceptedTrade(t)
	tr.Amount = money.MustParse("12.5000001")
	if _, err := NewTerms(tr, tokens.Default(), fees.Default()); !errors.Is(err, money.ErrTooPrecise) {
		t.Errorf("err = %v, want ErrTooPrecise", err)
	}
}

func TestNewTermsRejectsNonSolanaPartyID(t *testing.T) {
	tr := acceptedTrade(t)
	tr.SellerAgentID = "not-a-solana-address"
	if _, err := NewTerms(tr, tokens.Default(), fees.Default()); err == nil {
		t.Error("expected an error for a non-Solana seller id")
	}
}

// --- build ---

func TestBuildPrependsATAWhenSellerAccountMissing(t *testing.T) {
	build, err := NewBuilder(newFake()).Build(context.Background(), testTerms(t))
	if err != nil {
		t.Fatal(err)
	}
	if !build.Request.CreatedATA {
		t.Error("expected the seller ATA creation to be included")
	}
	got, err := compile(build.Unsigned.Message)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("instructions = %d, want 4 (ATA, transfer, memo, fee)", len(got))
	}
	// Canonical order per the design spec §6.1.
	if !got[0].program.Equals(associatedtokenaccount.ProgramID) {
		t.Errorf("instruction 0 program = %s, want the associated token program", got[0].program)
	}
	if !got[1].program.Equals(token.ProgramID) {
		t.Errorf("instruction 1 program = %s, want the token program", got[1].program)
	}
	if !got[2].program.Equals(memo.ProgramID) {
		t.Errorf("instruction 2 program = %s, want the memo program", got[2].program)
	}
	if !got[3].program.Equals(system.ProgramID) {
		t.Errorf("instruction 3 program = %s, want the system program", got[3].program)
	}
}

func TestBuildOmitsATAWhenSellerAccountExists(t *testing.T) {
	client := newFake()
	client.accountExists = true
	build, err := NewBuilder(client).Build(context.Background(), testTerms(t))
	if err != nil {
		t.Fatal(err)
	}
	if build.Request.CreatedATA {
		t.Error("expected no ATA creation when the seller account exists")
	}
	got, err := compile(build.Unsigned.Message)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("instructions = %d, want 3", len(got))
	}
}

func TestBuildRequestMetadata(t *testing.T) {
	clock := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	build, err := NewBuilder(newFake()).SetClock(func() time.Time { return clock }).
		Build(context.Background(), testTerms(t))
	if err != nil {
		t.Fatal(err)
	}
	req := build.Request
	if req.TradeID != tradeID {
		t.Errorf("tradeId = %s, want %s", req.TradeID, tradeID)
	}
	if req.Blockhash != blockhash {
		t.Errorf("blockhash = %s, want %s", req.Blockhash, blockhash)
	}
	if req.LastValid != 1000 {
		t.Errorf("lastValid = %d, want 1000", req.LastValid)
	}
	if !req.CreatedAt.Equal(clock) {
		t.Errorf("createdAt = %s, want %s", req.CreatedAt, clock)
	}
	if !req.ExpiresAt.Equal(clock.Add(DefaultBlockhashTTL)) {
		t.Errorf("expiresAt = %s, want %s", req.ExpiresAt, clock.Add(DefaultBlockhashTTL))
	}
	if !req.Live(clock.Add(time.Minute)) {
		t.Error("request should be live one minute in")
	}
	if req.Live(clock.Add(2 * DefaultBlockhashTTL)) {
		t.Error("request should not be live after the blockhash window")
	}
}

func TestBuildProducesUnsignedBuyerTransaction(t *testing.T) {
	build, err := NewBuilder(newFake()).Build(context.Background(), testTerms(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(build.Unsigned.Signatures); got != 0 {
		t.Fatalf("signatures = %d, want none on an unsigned transaction", got)
	}
	raw, err := build.Unsigned.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(build.Request.UnsignedTx)
	if err != nil {
		t.Fatalf("unsignedTx is not base64: %v", err)
	}
	if string(raw) != string(decoded) {
		t.Error("unsignedTx does not round-trip to the built transaction")
	}
	// The wire format reserves exactly one signature slot for the buyer to fill.
	if len(raw) == 0 || raw[0] != 1 {
		t.Errorf("wire signature count = %v, want 1 slot for the buyer", raw[:1])
	}
	// Non-custodial: the buyer alone signs and pays.
	if !build.Unsigned.Message.AccountKeys[0].Equals(buyerKey) {
		t.Errorf("fee payer = %s, want the buyer %s", build.Unsigned.Message.AccountKeys[0], buyerKey)
	}
	if build.Unsigned.Message.Header.NumRequiredSignatures != 1 {
		t.Errorf("required signatures = %d, want 1", build.Unsigned.Message.Header.NumRequiredSignatures)
	}
}

func TestBuildPropagatesRPCFailures(t *testing.T) {
	terms := testTerms(t)
	boom := errors.New("rpc down")

	client := newFake()
	client.accountErr = boom
	if _, err := NewBuilder(client).Build(context.Background(), terms); !errors.Is(err, boom) {
		t.Errorf("account error = %v, want it propagated", err)
	}

	client = newFake()
	client.blockhashErr = boom
	if _, err := NewBuilder(client).Build(context.Background(), terms); !errors.Is(err, boom) {
		t.Errorf("blockhash error = %v, want it propagated", err)
	}
}

// --- verify: honest transactions ---

func TestVerifierAcceptsBuiltTransaction(t *testing.T) {
	terms := testTerms(t)
	build, err := NewBuilder(newFake()).Build(context.Background(), terms)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewVerifier().Verify(terms, signAs(t, build.Unsigned, buyerPriv)); err != nil {
		t.Errorf("Verify() = %v, want the built transaction to verify", err)
	}
}

func TestVerifierAcceptsThreeInstructionForm(t *testing.T) {
	terms := testTerms(t)
	client := newFake()
	client.accountExists = true
	build, err := NewBuilder(client).Build(context.Background(), terms)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewVerifier().Verify(terms, signAs(t, build.Unsigned, buyerPriv)); err != nil {
		t.Errorf("Verify() = %v, want the three-instruction form to verify", err)
	}
}

func TestVerifierAcceptsIndependentlyReconstructedTransaction(t *testing.T) {
	terms := testTerms(t)
	tx := assembleCanonical(t, terms, true)
	if err := NewVerifier().Verify(terms, signAs(t, tx, buyerPriv)); err != nil {
		t.Errorf("Verify() = %v, want an honestly reconstructed transaction to verify", err)
	}
}

// --- verify: tampering ---

func TestVerifierRejectsNilTransaction(t *testing.T) {
	if err := NewVerifier().Verify(testTerms(t), nil); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch", err)
	}
}

func TestVerifierRejectsUnsignedTransaction(t *testing.T) {
	terms := testTerms(t)
	build, err := NewBuilder(newFake()).Build(context.Background(), terms)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewVerifier().Verify(terms, build.Unsigned); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch for an unsigned transaction", err)
	}
}

func TestVerifierRejectsFeeStripping(t *testing.T) {
	terms := testTerms(t)
	tx := assembleTx(t, []solana.Instruction{
		transferInstruction(terms),
		memoInstruction(terms, tradeID),
	}, terms.Buyer)
	if err := NewVerifier().Verify(terms, signAs(t, tx, buyerPriv)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch for a stripped fee", err)
	}
}

func TestVerifierRejectsReducedFee(t *testing.T) {
	cheap, err := fees.New(1, fees.DefaultWallet)
	if err != nil {
		t.Fatal(err)
	}
	greedy := testTerms(t)
	greedy.Fee = cheap
	tx := assembleCanonical(t, greedy, true)
	if err := NewVerifier().Verify(testTerms(t), signAs(t, tx, buyerPriv)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch for a reduced fee", err)
	}
}

func TestVerifierRejectsDivertedFee(t *testing.T) {
	diverted, err := fees.New(fees.DefaultLamports, thiefKey.String())
	if err != nil {
		t.Fatal(err)
	}
	greedy := testTerms(t)
	greedy.Fee = diverted
	tx := assembleCanonical(t, greedy, true)
	if err := NewVerifier().Verify(testTerms(t), signAs(t, tx, buyerPriv)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch for a fee paid to another wallet", err)
	}
}

func TestVerifierRejectsWrongAmount(t *testing.T) {
	short := testTerms(t)
	short.Amount = 12_499_999
	tx := assembleCanonical(t, short, true)
	if err := NewVerifier().Verify(testTerms(t), signAs(t, tx, buyerPriv)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch for a short transfer", err)
	}
}

func TestVerifierRejectsWrongDecimals(t *testing.T) {
	terms := testTerms(t)
	tx := assembleTx(t, []solana.Instruction{
		transferInstructionWithDecimals(terms, 9),
		memoInstruction(terms, tradeID),
		feeInstruction(terms),
	}, terms.Buyer)
	if err := NewVerifier().Verify(terms, signAs(t, tx, buyerPriv)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch for wrong decimals", err)
	}
}

func TestVerifierRejectsWrongMemoText(t *testing.T) {
	terms := testTerms(t)
	tx := assembleTx(t, []solana.Instruction{
		transferInstruction(terms),
		memoInstruction(terms, "some-other-trade"),
		feeInstruction(terms),
	}, terms.Buyer)
	if err := NewVerifier().Verify(terms, signAs(t, tx, buyerPriv)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch for a memo naming another trade", err)
	}
}

func TestVerifierRejectsReorderedInstructions(t *testing.T) {
	terms := testTerms(t)
	// Same instructions, but the fee runs before the payment.
	tx := assembleTx(t, []solana.Instruction{
		transferInstruction(terms),
		feeInstruction(terms),
		memoInstruction(terms, tradeID),
	}, terms.Buyer)
	if err := NewVerifier().Verify(terms, signAs(t, tx, buyerPriv)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch for reordered instructions", err)
	}
}

func TestVerifierRejectsExtraInstruction(t *testing.T) {
	terms := testTerms(t)
	tx := assembleTx(t, []solana.Instruction{
		transferInstruction(terms),
		memoInstruction(terms, tradeID),
		feeInstruction(terms),
		memoInstruction(terms, "smuggled"),
	}, terms.Buyer)
	if err := NewVerifier().Verify(terms, signAs(t, tx, buyerPriv)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch for an extra instruction", err)
	}
}

func TestVerifierRejectsTransferToWrongWallet(t *testing.T) {
	terms := testTerms(t)
	thiefATA, err := terms.ata(thiefKey)
	if err != nil {
		t.Fatal(err)
	}
	buyerATA, err := terms.BuyerATA()
	if err != nil {
		t.Fatal(err)
	}
	redirected := token.NewTransferCheckedInstructionBuilder().
		SetAmount(terms.Amount).
		SetDecimals(uint8(terms.Decimals)).
		SetSourceAccount(buyerATA).
		SetMintAccount(terms.Mint).
		SetDestinationAccount(thiefATA).
		SetOwnerAccount(terms.Buyer).
		Build()
	tx := assembleTx(t, []solana.Instruction{
		redirected,
		memoInstruction(terms, tradeID),
		feeInstruction(terms),
	}, terms.Buyer)
	if err := NewVerifier().Verify(terms, signAs(t, tx, buyerPriv)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch for a redirected transfer", err)
	}
}

func TestVerifierRejectsWrongMint(t *testing.T) {
	terms := testTerms(t)
	buyerATA, err := terms.BuyerATA()
	if err != nil {
		t.Fatal(err)
	}
	sellerATA, err := terms.SellerATA()
	if err != nil {
		t.Fatal(err)
	}
	swapped := token.NewTransferCheckedInstructionBuilder().
		SetAmount(terms.Amount).
		SetDecimals(uint8(terms.Decimals)).
		SetSourceAccount(buyerATA).
		SetMintAccount(thiefKey).
		SetDestinationAccount(sellerATA).
		SetOwnerAccount(terms.Buyer).
		Build()
	tx := assembleTx(t, []solana.Instruction{
		swapped,
		memoInstruction(terms, tradeID),
		feeInstruction(terms),
	}, terms.Buyer)
	if err := NewVerifier().Verify(terms, signAs(t, tx, buyerPriv)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch for a different mint", err)
	}
}

func TestVerifierRejectsThirdPartyFeePayer(t *testing.T) {
	terms := testTerms(t)
	instructions, err := expectedInstructions(terms, true)
	if err != nil {
		t.Fatal(err)
	}
	// Even a transaction the thief can fully sign must not settle the trade:
	// the buyer must be the fee payer and the only required signer.
	tx, err := assemble(instructions, solana.MustHashFromBase58(blockhash), thiefKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewVerifier().Verify(terms, signAs(t, tx, thiefPriv, buyerPriv)); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch when a third party pays the fee", err)
	}
}

func TestVerifierRejectsMissingBuyerSignature(t *testing.T) {
	terms := testTerms(t)
	instructions, err := expectedInstructions(terms, true)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := assemble(instructions, solana.MustHashFromBase58(blockhash), terms.Buyer)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewVerifier().Verify(terms, tx); !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch when the buyer did not sign", err)
	}
}

func TestVerifierMismatchNamesOffendingInstruction(t *testing.T) {
	terms := testTerms(t)
	// Instructions 0 and 1 are exactly right; only the fee instruction differs.
	shortFee := system.NewTransferInstruction(499_999, terms.Buyer, terms.Fee.Wallet()).Build()
	tx := assembleTx(t, []solana.Instruction{
		transferInstruction(terms),
		memoInstruction(terms, tradeID),
		shortFee,
	}, terms.Buyer)
	err := NewVerifier().Verify(terms, signAs(t, tx, buyerPriv))
	if err == nil {
		t.Fatal("expected a mismatch")
	}
	if !errors.Is(err, ErrMismatch) {
		t.Errorf("err = %v, want ErrMismatch", err)
	}
	// The report must point at the fee, not at the transfer.
	if !strings.Contains(err.Error(), "instruction 2") {
		t.Errorf("error %q should name the offending instruction index", err)
	}
}
