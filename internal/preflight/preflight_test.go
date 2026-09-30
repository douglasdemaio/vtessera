package preflight

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
)

const (
	usdc      = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	eurcMint  = "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr"
	feeWallet = "J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh"
)

var (
	tokenProgram = solana.MustPublicKeyFromBase58(tokens.TokenProgram)
	mintAuth     = solana.MustPublicKeyFromBase58("GrNg1XM2ctzeE2mXxXCfhcTUbejM8Z4z4wNVTy2FjMEz")
	freezeAuth   = solana.MustPublicKeyFromBase58("CJtyoKSLrktozQzjERTiK3btQtiTK3nN4QrqGHLidyCT")
)

// fakeRPC is a node that answers from a table, so every preflight outcome can be
// provoked exactly rather than by hoping a real node misbehaves.
type fakeRPC struct {
	genesis    string
	genesisErr error
	accounts   map[string]AccountInfo
	accountErr error
	balance    uint64
	balanceErr error
	rent       uint64
	rentErr    error

	genesisCalls int
	accountCalls int
	balanceCalls int
}

func (f *fakeRPC) GetGenesisHash(context.Context) (string, error) {
	f.genesisCalls++
	return f.genesis, f.genesisErr
}

func (f *fakeRPC) GetAccountInfo(_ context.Context, addr solana.PublicKey) (AccountInfo, error) {
	f.accountCalls++
	if f.accountErr != nil {
		return AccountInfo{}, f.accountErr
	}
	return f.accounts[addr.String()], nil
}

func (f *fakeRPC) GetBalance(context.Context, solana.PublicKey) (uint64, error) {
	f.balanceCalls++
	return f.balance, f.balanceErr
}

func (f *fakeRPC) GetMinimumBalanceForRentExemption(context.Context, uint64) (uint64, error) {
	if f.rentErr != nil {
		return 0, f.rentErr
	}
	return f.rent, nil
}

// mintData encodes a mint account the way the chain would, so the decoder is
// exercised against real layout rather than a hand-built byte slice.
func mintData(t *testing.T, decimals uint8, initialized bool, mintAuth, freezeAuth *solana.PublicKey) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := bin.NewBinEncoder(&buf).Encode(&token.Mint{
		MintAuthority:   mintAuth,
		Supply:          0,
		Decimals:        decimals,
		IsInitialized:   initialized,
		FreezeAuthority: freezeAuth,
	}); err != nil {
		t.Fatalf("encode mint: %v", err)
	}
	return buf.Bytes()
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

// healthy is a node that answers correctly for every governed mint on
// mainnet-beta, with a fee wallet comfortably rent-exempt.
func healthy(t *testing.T) *fakeRPC {
	t.Helper()
	return &fakeRPC{
		genesis: cluster.MainnetBeta.GenesisHash(),
		accounts: map[string]AccountInfo{
			usdc: {
				Owner:  tokenProgram,
				Data:   mintData(t, 6, true, &mintAuth, &freezeAuth),
				Exists: true,
			},
		},
		balance: 39_724_828,
		rent:    650_240,
	}
}

func deps(rpc *fakeRPC, mints ...tokens.Token) Deps {
	policy, err := fees.New(1000, feeWallet)
	if err != nil {
		panic(err)
	}
	return Deps{
		Cluster:   cluster.MainnetBeta,
		Endpoint:  "https://api.mainnet-beta.solana.com",
		RPC:       rpc,
		Mints:     mints,
		FeePolicy: policy,
	}
}

func TestPreflightPassesOnAHealthyNode(t *testing.T) {
	report, err := Check(context.Background(), deps(healthy(t), governedUSDC()))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.GenesisHash != cluster.MainnetBeta.GenesisHash() {
		t.Errorf("genesis = %q", report.GenesisHash)
	}
	if len(report.Mints) != 1 || !report.Mints[0].Verified || !report.Mints[0].AuthoritiesMatch {
		t.Errorf("mints = %+v, want one verified with matching authorities", report.Mints)
	}
	if report.RentMinimum != 650_240 {
		t.Errorf("rent = %d, want the queried 650240", report.RentMinimum)
	}
	if report.FeeBalance != 39_724_828 {
		t.Errorf("fee balance = %d", report.FeeBalance)
	}
}

func TestPreflightRefusesAnUnreachableChain(t *testing.T) {
	// A node that cannot be reached is never a mint fault. Conflating the two
	// would point an operator at the token registry when the fault is the network.
	rpc := healthy(t)
	rpc.genesisErr = errors.New("dial tcp: connection refused")
	_, err := Check(context.Background(), deps(rpc, governedUSDC()))
	if !errors.Is(err, ErrChainUnreachable) {
		t.Fatalf("err = %v, want ErrChainUnreachable", err)
	}
	// The message has to name the network, not a subsystem. An operator reading
	// "insufficient funds" at boot would go looking at the fee wallet while the
	// actual fault is that nothing can be reached.
	if errors.Is(err, ErrFeeWalletUnderfunded) || errors.Is(err, ErrFeeWalletMissing) {
		t.Errorf("an unreachable node was reported as a fee-wallet fault: %v", err)
	}
	if errors.Is(err, ErrMintMissing) || errors.Is(err, ErrMintOwner) ||
		errors.Is(err, ErrMintDecimals) || errors.Is(err, ErrMintUninitialized) {
		t.Errorf("an unreachable node was reported as a mint fault: %v", err)
	}
}

func TestPreflightRefusesTheWrongChain(t *testing.T) {
	// A devnet endpoint under a mainnet-beta declaration. The mint exists, so
	// only the genesis hash catches this, which is why it is checked first.
	rpc := healthy(t)
	rpc.genesis = cluster.Devnet.GenesisHash()
	_, err := Check(context.Background(), deps(rpc, governedUSDC()))
	if !errors.Is(err, ErrClusterIdentity) {
		t.Errorf("err = %v, want ErrClusterIdentity", err)
	}
	if rpc.accountCalls != 0 {
		t.Error("mint accounts were read before the cluster was identified")
	}
}

func TestPreflightRefusesAMissingMint(t *testing.T) {
	rpc := healthy(t)
	// Absent from the table entirely: Exists false, no transport error. The
	// distinction is the point — a missing account is an answer about the chain,
	// not a failed read of it.
	delete(rpc.accounts, usdc)
	_, err := Check(context.Background(), deps(rpc, governedUSDC()))
	if !errors.Is(err, ErrMintMissing) {
		t.Errorf("err = %v, want ErrMintMissing", err)
	}
}

func TestPreflightRefusesAMintOwnedByTheWrongProgram(t *testing.T) {
	rpc := healthy(t)
	// A Token-2022 mint, or any account that is not the classic token program's.
	other := solana.MustPublicKeyFromBase58("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")
	rpc.accounts[usdc] = AccountInfo{
		Owner:  other,
		Data:   mintData(t, 6, true, &mintAuth, &freezeAuth),
		Exists: true,
	}
	_, err := Check(context.Background(), deps(rpc, governedUSDC()))
	if !errors.Is(err, ErrMintOwner) {
		t.Errorf("err = %v, want ErrMintOwner", err)
	}
}

func TestPreflightRefusesWrongDecimals(t *testing.T) {
	rpc := healthy(t)
	rpc.accounts[usdc] = AccountInfo{
		Owner:  tokenProgram,
		Data:   mintData(t, 9, true, &mintAuth, &freezeAuth),
		Exists: true,
	}
	_, err := Check(context.Background(), deps(rpc, governedUSDC()))
	if !errors.Is(err, ErrMintDecimals) {
		t.Errorf("err = %v, want ErrMintDecimals", err)
	}
}

func TestPreflightRefusesAnUninitializedMint(t *testing.T) {
	rpc := healthy(t)
	rpc.accounts[usdc] = AccountInfo{
		Owner:  tokenProgram,
		Data:   mintData(t, 6, false, &mintAuth, &freezeAuth),
		Exists: true,
	}
	_, err := Check(context.Background(), deps(rpc, governedUSDC()))
	if !errors.Is(err, ErrMintUninitialized) {
		t.Errorf("err = %v, want ErrMintUninitialized", err)
	}
}

// TestPreflightRefusesAuthorityDrift is the boot-time half of the asymmetry:
// drift stops the service here, even though the token is still the same token.
func TestPreflightRefusesAuthorityDrift(t *testing.T) {
	rpc := healthy(t)
	rotated := solana.MustPublicKeyFromBase58("DuYQKfdunafuUqS7h6gugadrAiprxwtwBZFZLAMk4A99")
	rpc.accounts[usdc] = AccountInfo{
		Owner:  tokenProgram,
		Data:   mintData(t, 6, true, &rotated, &freezeAuth),
		Exists: true,
	}
	report, err := Check(context.Background(), deps(rpc, governedUSDC()))
	if !errors.Is(err, ErrAuthorityDrift) {
		t.Fatalf("err = %v, want ErrAuthorityDrift", err)
	}
	if len(report.Mints) != 1 || report.Mints[0].AuthoritiesMatch {
		t.Errorf("report = %+v, want the drift recorded", report.Mints)
	}
	if report.Mints[0].Drift == "" {
		t.Error("drift should name what changed")
	}
	// The hard invariants did hold, so the report says so: an operator needs to
	// know the difference between "this token is broken" and "the issuer rotated".
	if !report.Mints[0].Verified {
		t.Error("mint should report as verified; only the pin drifted")
	}
}

func TestPreflightRefusesANewlyAppearingFreezeAuthority(t *testing.T) {
	rpc := healthy(t)
	// The pin says no freeze authority. The account now has one. A counterparty
	// who cannot move funds in a frozen account has a problem an ordinary mint
	// rotation does not create, so this is not a bookkeeping change.
	noFreeze := governedUSDC()
	noFreeze.FreezeAuthority = ""
	rpc.accounts[usdc] = AccountInfo{
		Owner:  tokenProgram,
		Data:   mintData(t, 6, true, &mintAuth, &freezeAuth),
		Exists: true,
	}
	if _, err := Check(context.Background(), deps(rpc, noFreeze)); !errors.Is(err, ErrAuthorityDrift) {
		t.Errorf("err = %v, want ErrAuthorityDrift when a freeze authority appears", err)
	}
}

func TestPreflightRefusesAnUnderfundedFeeWallet(t *testing.T) {
	rpc := healthy(t)
	rpc.balance = 1
	_, err := Check(context.Background(), deps(rpc, governedUSDC()))
	if !errors.Is(err, ErrFeeWalletUnderfunded) {
		t.Errorf("err = %v, want ErrFeeWalletUnderfunded", err)
	}
}

func TestPreflightUsesTheQueriedRentMinimum(t *testing.T) {
	// A validator charges more rent than mainnet. Using a hardcoded threshold
	// would refuse a correctly funded localnet wallet, or accept an unfunded
	// mainnet one, depending on which number was written down.
	rpc := healthy(t)
	rpc.rent = 890_880
	rpc.balance = 890_880
	if _, err := Check(context.Background(), deps(rpc, governedUSDC())); err != nil {
		t.Errorf("exactly rent-exempt should pass: %v", err)
	}
	rpc.balance = 890_879
	if _, err := Check(context.Background(), deps(rpc, governedUSDC())); !errors.Is(err, ErrFeeWalletUnderfunded) {
		t.Errorf("err = %v, want one lamport short to fail", err)
	}
}

func TestPreflightRefusesANonLoopbackLocalnetEndpoint(t *testing.T) {
	// A disposable chain has no genesis pin, so the host is the only thing
	// binding it to this machine. Getting this wrong points settlement at a
	// remote validator that will happily confirm anything.
	rpc := &fakeRPC{
		genesis:  "whatever",
		accounts: map[string]AccountInfo{},
		balance:  1_000_000_000,
		rent:     890_880,
	}
	d := deps(rpc)
	d.Cluster = cluster.Localnet
	d.Endpoint = "http://203.0.113.9:8899"
	d.Mints = nil
	_, err := Check(context.Background(), d)
	if !errors.Is(err, ErrEndpointNotAllowed) {
		t.Errorf("err = %v, want ErrEndpointNotAllowed", err)
	}
	if rpc.genesisCalls != 0 {
		t.Error("a request was sent to a disallowed endpoint")
	}
}

func TestPreflightAllowsALoopbackLocalnetEndpoint(t *testing.T) {
	rpc := &fakeRPC{
		genesis:  "whatever",
		accounts: map[string]AccountInfo{},
		balance:  1_000_000_000,
		rent:     890_880,
	}
	d := deps(rpc)
	d.Cluster = cluster.Localnet
	d.Endpoint = "http://127.0.0.1:8899"
	d.Mints = nil
	if _, err := Check(context.Background(), d); err != nil {
		t.Errorf("Check: %v", err)
	}
}

func TestPreflightGovernsNoMintsOnAnEmptyLocalnet(t *testing.T) {
	// The localnet registry is empty by design, and an empty set must pass
	// rather than tripping over a loop it never entered.
	rpc := &fakeRPC{
		genesis:  "whatever",
		accounts: map[string]AccountInfo{},
		balance:  1_000_000_000,
		rent:     890_880,
	}
	d := deps(rpc)
	d.Cluster = cluster.Localnet
	d.Endpoint = "http://localhost:8899"
	if _, err := Check(context.Background(), d); err != nil {
		t.Errorf("Check: %v", err)
	}
}

func TestDecodeMintRefusesAnEmptyAccount(t *testing.T) {
	// Reading a missing account as "zero decimals" is how a 10^6 error gets in,
	// so an empty account must be an error rather than a zero-valued mint.
	if _, err := DecodeMint(nil); err == nil {
		t.Error("DecodeMint(nil) should fail")
	}
}

func TestEURCIsTheSameAddressOnMainnetAndDevnet(t *testing.T) {
	// Worth pinning in a test: the shared address is surprising enough that
	// someone will eventually "fix" one of the two entries, and a devnet EURC
	// that does not exist settles nothing.
	mainnet, err := tokens.ForCluster(cluster.MainnetBeta)
	if err != nil {
		t.Fatal(err)
	}
	devnet, err := tokens.ForCluster(cluster.Devnet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mainnet.Enabled(eurcMint); err != nil {
		t.Errorf("mainnet EURC: %v", err)
	}
	if _, err := devnet.Enabled(eurcMint); err != nil {
		t.Errorf("devnet EURC: %v", err)
	}
}
