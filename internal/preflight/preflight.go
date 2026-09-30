// Package preflight is the fail-closed gate between a configured endpoint and a
// serving process. Nothing is cached at startup: a mint that is wrong must never
// be able to look right because it was right a moment ago.
//
// The ordering of the checks is load-bearing. Cluster identity comes first
// because every later check reads an account and draws a conclusion from
// whatever that account says, and on the wrong chain those conclusions are
// about a chain nobody is watching.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/gagliardetto/solana-go"
)

// Each check fails with a typed error naming the specific check, because
// "preflight failed" sends an operator looking in the wrong place and
// "genesis hash mismatch" does not.
var (
	// ErrClusterIdentity means the endpoint is not the declared cluster.
	ErrClusterIdentity = errors.New("cluster identity check failed")
	// ErrMintMissing means a governed mint has no account on this chain.
	ErrMintMissing = errors.New("governed mint is absent")
	// ErrMintOwner means a governed mint is not owned by the token program.
	ErrMintOwner = errors.New("governed mint has the wrong owner")
	// ErrMintDecimals means a governed mint's scale differs from the pin.
	ErrMintDecimals = errors.New("governed mint has the wrong decimals")
	// ErrMintUninitialized means a governed mint account exists but is not
	// initialized, so it holds no usable state.
	ErrMintUninitialized = errors.New("governed mint is not initialized")
	// ErrAuthorityDrift means a governance pin no longer matches the chain. It is
	// fatal at startup and a warning on use.
	ErrAuthorityDrift = errors.New("governed mint authority pin does not match")
	// ErrEndpointNotAllowed means the endpoint is not somewhere this cluster may
	// be reached, which for localnet means not a loopback address.
	ErrEndpointNotAllowed = errors.New("endpoint is not allowed for this cluster")
	// ErrFeeWalletMissing means the fee wallet has no account.
	ErrFeeWalletMissing = errors.New("fee wallet is absent")
	// ErrFeeWalletUnderfunded means the fee wallet cannot exist as a
	// rent-exempt account at its current balance.
	ErrFeeWalletUnderfunded = errors.New("fee wallet is below the rent-exempt minimum")
	// ErrChainUnreachable means a check could not complete because the node
	// could not be reached. It is never a mint fault: reporting "wrong" for a
	// rate-limited request sends an operator chasing a problem that does not
	// exist. It was previously called ErrChainUnreachable, which described a
	// fee-wallet condition and nothing else — the name pointed an operator at
	// the one subsystem the fault was never in.
	ErrChainUnreachable = errors.New("the chain could not be consulted")
)

// TokenProgram is the SPL Token program every governed mint must be owned by.
const TokenProgram = tokens.TokenProgram

// Deps is everything Check reads. Mints is the active cluster's governed set,
// already scoped, so preflight never decides what is governed — it only reports
// whether the chain agrees with what the registry says.
type Deps struct {
	Cluster cluster.Cluster
	// Endpoint is the configured RPC URL, checked against the cluster's host
	// rules before any request is made.
	Endpoint   string
	RPC        RPC
	Mints      []tokens.Token
	FeePolicy  fees.Policy
	AllowHosts []string
}

// Report is what Check returns when the endpoint is fit to settle against. It
// is logged as a single structured line and asserted on by tests, so it records
// the facts that were verified rather than only the absence of errors.
type Report struct {
	Cluster     cluster.Cluster
	GenesisHash string
	Endpoint    string
	Mints       []MintCheck
	FeeWallet   string
	FeeBalance  uint64
	RentMinimum uint64
}

// MintCheck is one mint's verdict. The distinction between a hard invariant and
// a governance pin is the reason this is a struct and not a bool: a hard
// invariant that fails stops settlement everywhere, while an authority that
// drifts only stops it at startup, where a human can decide whether to re-pin.
type MintCheck struct {
	Address  string
	Symbol   string
	Decimals int
	// Verified is true when the account exists, is owned by the token program,
	// is initialized, and reports the pinned decimals.
	Verified bool
	// AuthoritiesMatch is false when a governance pin drifted. Startup treats
	// this as fatal; settlement-time verification treats it as a warning,
	// because a legitimate issuer rotation must not become an outage.
	AuthoritiesMatch bool
	// Drift names what changed, for the log line and the error message.
	Drift string
}

// Check runs every preflight check in order and returns the first failure. A
// returned error is the reason not to serve, so it names the check.
func Check(ctx context.Context, d Deps) (Report, error) {
	report := Report{
		Cluster:  d.Cluster,
		Endpoint: d.Endpoint,
		Mints:    make([]MintCheck, 0, len(d.Mints)),
	}

	// 0. The endpoint host, before a single request goes out. On localnet this
	// is the only thing binding a chain with no genesis pin to this machine, so
	// it belongs before the identity check rather than beside it.
	if err := d.Cluster.CheckEndpoint(d.Endpoint, d.AllowHosts); err != nil {
		return report, fmt.Errorf("%w: %v", ErrEndpointNotAllowed, err)
	}

	// 1. Cluster identity. Everything below reads an account and concludes
	// something from it, so this has to come first: on the wrong chain, a mint
	// that exists is a mint on the wrong chain.
	genesis, err := d.RPC.GetGenesisHash(ctx)
	if err != nil {
		return report, fmt.Errorf("%w: read genesis hash: %v", ErrChainUnreachable, err)
	}
	report.GenesisHash = genesis
	if err := d.Cluster.Verify(genesis); err != nil {
		return report, fmt.Errorf("%w: %v", ErrClusterIdentity, err)
	}

	// 2. Governed mints, split into hard invariants and governance pins.
	//
	// Drift is fatal here and only a warning on use. The asymmetry is
	// deliberate: at boot there is a human who can decide whether a rotated
	// authority is legitimate and re-pin it, and a service that started anyway
	// would be settling from pins nobody has looked at. Once running, stopping
	// every settlement over an issuer rotation would be a worse failure than
	// continuing to settle the same token.
	var drift []string
	for _, mint := range d.Mints {
		check, err := d.checkMint(ctx, mint)
		report.Mints = append(report.Mints, check)
		if err != nil {
			return report, err
		}
		if check.Drift != "" {
			drift = append(drift, check.Symbol+" ("+check.Address+"): "+check.Drift)
		}
	}
	if len(drift) > 0 {
		return report, fmt.Errorf("%w: %s; re-pin or correct the registry before serving",
			ErrAuthorityDrift, strings.Join(drift, "; "))
	}

	// 3. The fee wallet must be able to exist. The threshold is queried, never
	// hardcoded: the same query returns 650,240 on mainnet and 890,880 on a
	// local validator, so a literal would be wrong in one of those places.
	report.RentMinimum, err = d.RPC.GetMinimumBalanceForRentExemption(ctx, 0)
	if err != nil {
		return report, fmt.Errorf("%w: query rent-exempt minimum: %v", ErrChainUnreachable, err)
	}
	feeWallet, err := solana.PublicKeyFromBase58(d.FeePolicy.WalletAddress())
	if err != nil {
		return report, fmt.Errorf("%w: fee wallet %q: %v", ErrChainUnreachable, d.FeePolicy.WalletAddress(), err)
	}
	report.FeeWallet = feeWallet.String()
	report.FeeBalance, err = d.RPC.GetBalance(ctx, feeWallet)
	if err != nil {
		return report, fmt.Errorf("%w: read fee wallet balance: %v", ErrChainUnreachable, err)
	}
	if report.FeeBalance < report.RentMinimum {
		return report, fmt.Errorf("%w: %s holds %d lamports, needs %d to exist rent-exempt",
			ErrFeeWalletUnderfunded, feeWallet, report.FeeBalance, report.RentMinimum)
	}
	return report, nil
}

// checkMint verifies one governed mint. Hard invariants are fatal here and
// fatal on use; authority drift is fatal here and only a warning on use, so the
// caller learns which kind it hit from the returned error.
func (d Deps) checkMint(ctx context.Context, mint tokens.Token) (MintCheck, error) {
	check := MintCheck{Address: mint.Address, Symbol: mint.Symbol, Decimals: mint.Decimals}
	addr, err := solana.PublicKeyFromBase58(mint.Address)
	if err != nil {
		// A malformed entry is a programming fault in the registry, not
		// something the node can report on.
		return check, fmt.Errorf("%w: %s is not a Solana address: %v", ErrMintMissing, mint.Address, err)
	}
	info, err := d.RPC.GetAccountInfo(ctx, addr)
	if err != nil {
		return check, fmt.Errorf("%w: read %s: %v", ErrChainUnreachable, mint.Address, err)
	}
	if !info.Exists {
		return check, fmt.Errorf("%w: %s (%s) has no account on %s",
			ErrMintMissing, mint.Address, mint.Symbol, d.Cluster)
	}
	if !info.Owner.Equals(mustTokenProgram()) {
		return check, fmt.Errorf("%w: %s is owned by %s, want %s",
			ErrMintOwner, mint.Address, info.Owner, TokenProgram)
	}
	decoded, err := DecodeMint(info.Data)
	if err != nil {
		return check, fmt.Errorf("%w: %s: %v", ErrMintMissing, mint.Address, err)
	}
	if !decoded.IsInitialized {
		return check, fmt.Errorf("%w: %s (%s)", ErrMintUninitialized, mint.Address, mint.Symbol)
	}
	if int(decoded.Decimals) != mint.Decimals {
		// This is the one that matters most: decimals is the exponent between
		// the decimal price an agent quoted and the integer base units that
		// move. A mismatch here is a 10^n error, not a rounding difference.
		return check, fmt.Errorf("%w: %s (%s) reports %d decimals, pinned to %d",
			ErrMintDecimals, mint.Address, mint.Symbol, decoded.Decimals, mint.Decimals)
	}

	// Hard invariants hold. From here the remaining checks are governance pins,
	// which are recorded rather than returned as an error by this function so
	// the caller decides the severity.
	check.Verified = true
	check.AuthoritiesMatch = true
	if drift := AuthorityDrift(mint, decoded); drift != "" {
		check.AuthoritiesMatch = false
		check.Drift = drift
	}
	return check, nil
}

// AuthorityDrift names the first governance pin that differs from the chain, or
// "" when both match. It is exported because startup and settlement-time
// verification ask the same question and must not be able to disagree about the
// answer.
//
// A freeze authority appearing where there was none is a real signal rather than
// a bookkeeping change: a counterparty who cannot move funds in a frozen account
// has a problem a mint-authority rotation does not create.
func AuthorityDrift(mint tokens.Token, decoded Mint) string {
	if !sameAuthority(mint.MintAuthorityKey(), decoded.MintAuthority) {
		return fmt.Sprintf("mint authority is %s, pinned to %s", describe(decoded.MintAuthority), describe(mint.MintAuthorityKey()))
	}
	if !sameAuthority(mint.FreezeAuthorityKey(), decoded.FreezeAuthority) {
		return fmt.Sprintf("freeze authority is %s, pinned to %s", describe(decoded.FreezeAuthority), describe(mint.FreezeAuthorityKey()))
	}
	return ""
}

func sameAuthority(pinned, observed *solana.PublicKey) bool {
	switch {
	case pinned == nil && observed == nil:
		return true
	case pinned == nil || observed == nil:
		return false
	default:
		return pinned.Equals(*observed)
	}
}

func describe(key *solana.PublicKey) string {
	if key == nil {
		return "none"
	}
	return key.String()
}

func mustTokenProgram() solana.PublicKey { return solana.MustPublicKeyFromBase58(TokenProgram) }
