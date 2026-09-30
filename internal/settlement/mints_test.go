package settlement_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/preflight"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
)

const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

var (
	tokenProgram = solana.MustPublicKeyFromBase58(tokens.TokenProgram)
	mintAuth     = solana.MustPublicKeyFromBase58("GrNg1XM2ctzeE2mXxXCfhcTUbejM8Z4z4wNVTy2FjMEz")
	freezeAuth   = solana.MustPublicKeyFromBase58("CJtyoKSLrktozQzjERTiK3btQtiTK3nN4QrqGHLidyCT")
)

func mintData(t *testing.T, decimals uint8, initialized bool, mintAuth, freezeAuth *solana.PublicKey) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := bin.NewBinEncoder(&buf).Encode(&token.Mint{
		MintAuthority:   mintAuth,
		Decimals:        decimals,
		IsInitialized:   initialized,
		FreezeAuthority: freezeAuth,
	}); err != nil {
		t.Fatalf("encode mint: %v", err)
	}
	return buf.Bytes()
}

// countingReader records how many times the mint account was actually read, and
// can be made to fail or to block so the cache and the flight are observable.
type countingReader struct {
	info    preflight.AccountInfo
	err     error
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *countingReader) GetAccountInfo(context.Context, solana.PublicKey) (preflight.AccountInfo, error) {
	r.calls.Add(1)
	if r.entered != nil {
		r.once.Do(func() { close(r.entered) })
	}
	if r.release != nil {
		<-r.release
	}
	return r.info, r.err
}

func healthyMint(t *testing.T) preflight.AccountInfo {
	t.Helper()
	return preflight.AccountInfo{
		Owner:  tokenProgram,
		Data:   mintData(t, 6, true, &mintAuth, &freezeAuth),
		Exists: true,
	}
}

func governedUSDC() tokens.Token {
	return tokens.Token{
		Address:         usdc,
		Symbol:          "USDC",
		Decimals:        6,
		Enabled:         true,
		Cluster:         string(cluster.MainnetBeta),
		MintAuthority:   mintAuth.String(),
		FreezeAuthority: freezeAuth.String(),
	}
}

func TestVerifyAcceptsTheGovernedMint(t *testing.T) {
	v := settlement.NewMintVerifier(&countingReader{info: healthyMint(t)}, cluster.MainnetBeta)
	if err := v.Verify(context.Background(), governedUSDC()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyCachesWithinTheTTL(t *testing.T) {
	reader := &countingReader{info: healthyMint(t)}
	v := settlement.NewMintVerifier(reader, cluster.MainnetBeta)
	for i := 0; i < 5; i++ {
		if err := v.Verify(context.Background(), governedUSDC()); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	}
	// Five settlements inside the window must not become five reads. A burst of
	// buyers is exactly when a rate-limited public endpoint starts failing.
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("mint account read %d times, want 1", got)
	}
}

func TestVerifyReadsAgainAfterTheTTL(t *testing.T) {
	reader := &countingReader{info: healthyMint(t)}
	now := time.Now()
	v := settlement.NewMintVerifier(reader, cluster.MainnetBeta).
		SetClock(func() time.Time { return now }).
		SetTTL(30 * time.Second)
	if err := v.Verify(context.Background(), governedUSDC()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(29 * time.Second)
	if err := v.Verify(context.Background(), governedUSDC()); err != nil {
		t.Fatal(err)
	}
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("read %d times before the TTL expired, want 1", got)
	}
	now = now.Add(2 * time.Second)
	if err := v.Verify(context.Background(), governedUSDC()); err != nil {
		t.Fatal(err)
	}
	if got := reader.calls.Load(); got != 2 {
		t.Errorf("read %d times after the TTL expired, want 2", got)
	}
}

// TestVerifyCollapsesConcurrentReads is the rate-limit defence. Without it, N
// buyers arriving at once produce N simultaneous getAccountInfo calls, and a
// public endpoint answers most of them with 429.
func TestVerifyCollapsesConcurrentReads(t *testing.T) {
	reader := &countingReader{
		info:    healthyMint(t),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	v := settlement.NewMintVerifier(reader, cluster.MainnetBeta)

	const buyers = 16
	var wg sync.WaitGroup
	errs := make([]error, buyers)
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = v.Verify(context.Background(), governedUSDC())
		}(i)
	}
	<-reader.entered
	close(reader.release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("buyer %d: %v", i, err)
		}
	}
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("mint account read %d times for %d concurrent buyers, want 1", got, buyers)
	}
}

func TestVerifyReportsAMissingMintAsUnverified(t *testing.T) {
	v := settlement.NewMintVerifier(&countingReader{info: preflight.AccountInfo{}}, cluster.MainnetBeta)
	err := v.Verify(context.Background(), governedUSDC())
	if !errors.Is(err, settlement.ErrMintUnverified) {
		t.Errorf("err = %v, want ErrMintUnverified", err)
	}
}

func TestVerifyReportsWrongDecimalsAsUnverified(t *testing.T) {
	// The one that matters most: decimals is the exponent between the decimal
	// price an agent quoted and the base units that move, so this is a 10^n
	// error rather than a rounding difference.
	info := preflight.AccountInfo{
		Owner:  tokenProgram,
		Data:   mintData(t, 9, true, &mintAuth, &freezeAuth),
		Exists: true,
	}
	v := settlement.NewMintVerifier(&countingReader{info: info}, cluster.MainnetBeta)
	err := v.Verify(context.Background(), governedUSDC())
	if !errors.Is(err, settlement.ErrMintUnverified) {
		t.Errorf("err = %v, want ErrMintUnverified", err)
	}
}

func TestVerifyReportsTheWrongProgramAsUnverified(t *testing.T) {
	info := healthyMint(t)
	info.Owner = solana.MustPublicKeyFromBase58("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")
	v := settlement.NewMintVerifier(&countingReader{info: info}, cluster.MainnetBeta)
	if err := v.Verify(context.Background(), governedUSDC()); !errors.Is(err, settlement.ErrMintUnverified) {
		t.Errorf("err = %v, want ErrMintUnverified", err)
	}
}

// TestVerifyReportsAnUnreachableChainSeparately is the distinction that keeps an
// operator from chasing a mint problem during a network outage.
func TestVerifyReportsAnUnreachableChainSeparately(t *testing.T) {
	reader := &countingReader{err: errors.New("429 Too Many Requests")}
	v := settlement.NewMintVerifier(reader, cluster.MainnetBeta)
	err := v.Verify(context.Background(), governedUSDC())
	if !errors.Is(err, settlement.ErrMintUnreachable) {
		t.Errorf("err = %v, want ErrMintUnreachable", err)
	}
	if errors.Is(err, settlement.ErrMintUnverified) {
		t.Error("an unreachable chain must not be reported as a mint fault")
	}
}

func TestVerifyDoesNotCacheAFailure(t *testing.T) {
	// Caching "could not check" for the TTL would turn a ten-second rate limit
	// into thirty seconds of refusal, and a transient fault into a real outage.
	reader := &countingReader{err: errors.New("503 Service Unavailable")}
	v := settlement.NewMintVerifier(reader, cluster.MainnetBeta)
	if err := v.Verify(context.Background(), governedUSDC()); !errors.Is(err, settlement.ErrMintUnreachable) {
		t.Fatalf("err = %v, want ErrMintUnreachable", err)
	}
	if err := v.Verify(context.Background(), governedUSDC()); !errors.Is(err, settlement.ErrMintUnreachable) {
		t.Fatalf("err = %v, want ErrMintUnreachable", err)
	}
	if got := reader.calls.Load(); got != 2 {
		t.Errorf("read %d times, want 2: a failure must not be cached", got)
	}
}

// TestVerifyToleratesAuthorityDriftOnUse is the on-use half of the asymmetry.
// A legitimate issuer rotation must not become a settlement outage.
func TestVerifyToleratesAuthorityDriftOnUse(t *testing.T) {
	rotated := solana.MustPublicKeyFromBase58("DuYQKfdunafuUqS7h6gugadrAiprxwtwBZFZLAMk4A99")
	info := preflight.AccountInfo{
		Owner:  tokenProgram,
		Data:   mintData(t, 6, true, &rotated, &freezeAuth),
		Exists: true,
	}
	v := settlement.NewMintVerifier(&countingReader{info: info}, cluster.MainnetBeta)
	if err := v.Verify(context.Background(), governedUSDC()); err != nil {
		t.Errorf("a rotated mint authority must still settle: %v", err)
	}
	if got := v.AuthorityDrifts(); got != 1 {
		t.Errorf("AuthorityDrifts() = %d, want 1", got)
	}
}

func TestVerifyDoesNotCountDriftOnEveryCachedRead(t *testing.T) {
	rotated := solana.MustPublicKeyFromBase58("DuYQKfdunafuUqS7h6gugadrAiprxwtwBZFZLAMk4A99")
	info := preflight.AccountInfo{
		Owner:  tokenProgram,
		Data:   mintData(t, 6, true, &rotated, &freezeAuth),
		Exists: true,
	}
	v := settlement.NewMintVerifier(&countingReader{info: info}, cluster.MainnetBeta)
	for i := 0; i < 3; i++ {
		if err := v.Verify(context.Background(), governedUSDC()); err != nil {
			t.Fatal(err)
		}
	}
	// One drift observed means one observation, so the count is a rate an
	// operator can alert on rather than a number that tracks request volume.
	if got := v.AuthorityDrifts(); got != 1 {
		t.Errorf("AuthorityDrifts() = %d, want 1 for three reads of one observation", got)
	}
}

func TestVerifyKeepsClustersApart(t *testing.T) {
	// Same address, two clusters, one cache. Sharing the key would let a devnet
	// observation satisfy a mainnet request.
	mainnetReader := &countingReader{info: healthyMint(t)}
	v := settlement.NewMintVerifier(mainnetReader, cluster.MainnetBeta)
	if err := v.Verify(context.Background(), governedUSDC()); err != nil {
		t.Fatal(err)
	}
	devnetReader := &countingReader{info: preflight.AccountInfo{}}
	v2 := settlement.NewMintVerifier(devnetReader, cluster.Devnet)
	mint := governedUSDC()
	mint.Cluster = string(cluster.Devnet)
	if err := v2.Verify(context.Background(), mint); !errors.Is(err, settlement.ErrMintUnverified) {
		t.Errorf("err = %v, want ErrMintUnverified", err)
	}
	if devnetReader.calls.Load() == 0 {
		t.Error("devnet verification reused the mainnet observation")
	}
}

func TestVerifySurvivesACancelledCaller(t *testing.T) {
	// The shared read outlives the caller that started it, so one buyer giving
	// up must not fail the buyers who joined the same flight.
	reader := &countingReader{
		info:    healthyMint(t),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	v := settlement.NewMintVerifier(reader, cluster.MainnetBeta)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- v.Verify(ctx, governedUSDC()) }()
	<-reader.entered
	cancel()
	close(reader.release)

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("the cancelled caller should see its own cancellation, got %v", err)
	}
	// The read completed anyway, so a later caller is served from the observation.
	if err := v.Verify(context.Background(), governedUSDC()); err != nil {
		t.Errorf("Verify after a cancelled flight: %v", err)
	}
}

func TestACancelledLeaderDoesNotFailTheBuyerWaitingBehindIt(t *testing.T) {
	// The follower joined the flight while the leader's read was still in the
	// node, then the leader gave up. The follower did not cancel anything, so
	// handing it the leader's context error would report a cancellation the
	// buyer never asked for, and would make a healthy mint look unverified.
	reader := &countingReader{
		info:    healthyMint(t),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	v := settlement.NewMintVerifier(reader, cluster.MainnetBeta)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() { leaderDone <- v.Verify(leaderCtx, governedUSDC()) }()
	<-reader.entered

	// Join the same flight before the leader abandons it.
	followerDone := make(chan error, 1)
	go func() { followerDone <- v.Verify(context.Background(), governedUSDC()) }()

	cancelLeader()
	close(reader.release)

	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Errorf("the leader should see its own cancellation, got %v", err)
	}
	if err := <-followerDone; err != nil {
		t.Errorf("the follower was never cancelled and must not be failed: %v", err)
	}
	// One read served both callers.
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("mint reads = %d, want 1 shared read for two callers", got)
	}
}

func TestAFailedReadIsNotCachedAsAnObservation(t *testing.T) {
	// An unreachable node says nothing about the mint. Caching that answer would
	// keep refusing a perfectly good mint until the TTL expired, which turns a
	// momentary outage into settlement downtime.
	reader := &mutableReader{info: healthyMint(t), err: errors.New("dial tcp: i/o timeout")}
	v := settlement.NewMintVerifier(reader, cluster.MainnetBeta)
	if err := v.Verify(context.Background(), governedUSDC()); !errors.Is(err, settlement.ErrMintUnreachable) {
		t.Fatalf("err = %v, want ErrMintUnreachable", err)
	}
	reader.set(nil)
	if err := v.Verify(context.Background(), governedUSDC()); err != nil {
		t.Errorf("Verify once the node answers again: %v", err)
	}
}

// mutableReader answers with a fixed reading, or an error that a test can clear
// between verifications.
type mutableReader struct {
	mu   sync.Mutex
	info preflight.AccountInfo
	err  error
}

func (r *mutableReader) set(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *mutableReader) GetAccountInfo(context.Context, solana.PublicKey) (preflight.AccountInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.info, r.err
}

func TestAnAuthorityDriftIsReportedToTheOperatorEvenThoughSettlementContinues(t *testing.T) {
	// A rotation must not halt the marketplace, but it must not be silent either:
	// the pin no longer describes the token the marketplace is pricing, and only
	// a human can decide whether the new authority is the issuer.
	rotated := solana.MustPublicKeyFromBase58("DuYQKfdunafuUqS7h6gugadrAiprxwtwBZFZLAMk4A99")
	info := preflight.AccountInfo{
		Owner:  tokenProgram,
		Data:   mintData(t, 6, true, &rotated, &freezeAuth),
		Exists: true,
	}
	var reported []string
	v := settlement.NewMintVerifier(&countingReader{info: info}, cluster.MainnetBeta).
		OnDrift(func(drift string) { reported = append(reported, drift) })

	if err := v.Verify(context.Background(), governedUSDC()); err != nil {
		t.Fatalf("verification must still succeed through a drift, got %v", err)
	}
	if len(reported) != 1 {
		t.Fatalf("drift reported %d times, want 1: %v", len(reported), reported)
	}
	if reported[0] == "" {
		t.Error("the report must name what drifted, not just that something did")
	}
	// Repeated reads of the same observation are not repeated events.
	for i := 0; i < 3; i++ {
		if err := v.Verify(context.Background(), governedUSDC()); err != nil {
			t.Fatal(err)
		}
	}
	if len(reported) != 1 {
		t.Errorf("drift reported %d times, want 1: one observation is one event", len(reported))
	}
}
