package settlement

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/preflight"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/gagliardetto/solana-go"
)

var (
	// ErrMintUnverified means the mint's account no longer matches what was
	// governed: it is absent, wrongly owned, or reports a different scale.
	// These are the properties settlement correctness actually depends on, and
	// none of them can change without the token ceasing to be the token priced.
	ErrMintUnverified = errors.New("settlement mint is not the token we priced")
	// ErrMintUnreachable means the mint's status is unknown because the node
	// could not be reached. It is deliberately a different error from
	// ErrMintUnverified: reporting "wrong" for a rate-limited request sends an
	// operator chasing a mint problem that does not exist.
	ErrMintUnreachable = errors.New("settlement mint could not be verified")
	// ErrAuthorityDrifted means a governance pin no longer matches the chain.
	// It is not returned as a failure: a legitimate issuer rotation must not
	// become a settlement outage. The counter and the log are the signal.
	ErrAuthorityDrifted = errors.New("settlement mint authority pin has drifted")
)

// DefaultMintVerificationTTL bounds how long a mint observation is reused.
//
// Thirty seconds is the balance, and it is a balance between two failure modes.
// Long enough that a burst of concurrent builds does not become a burst of RPC
// calls against a rate-limited public endpoint; short enough that a mint which
// changed after boot cannot be used for a settlement request on the strength of
// a boot-time check. It is also far below a blockhash's ~90 second life, so a
// request can never be issued on a stale observation for longer than the
// transaction it carries would remain signable.
const DefaultMintVerificationTTL = 30 * time.Second

// MintReader is the read surface mint verification needs: one account fetch.
type MintReader interface {
	GetAccountInfo(ctx context.Context, addr solana.PublicKey) (preflight.AccountInfo, error)
}

// observation is one verified reading of a mint account.
type observation struct {
	mint preflight.Mint
	// drift is non-empty when a governance pin no longer matches. It does not
	// fail verification; it is counted and logged.
	drift string
	// fault is the empty string when the observation is sound, and otherwise the
	// reason a hard invariant was not met.
	fault string
	at    time.Time
}

// MintVerifier re-checks a governed mint against the chain before a settlement
// request is built from it, so a mint cannot be trusted purely because it was
// right at boot.
//
// Concurrent callers for the same mint collapse onto one RPC read. That matters
// because the callers are buyers arriving at once, and a rate-limited public
// endpoint turns N simultaneous reads into N failures.
type MintVerifier struct {
	reader  MintReader
	cluster cluster.Cluster
	ttl     time.Duration
	now     func() time.Time

	mu       sync.Mutex
	cache    map[string]observation
	inflight map[string]*mintCall

	// driftCounter counts authority drifts observed on use. It is exposed as a
	// number so an operator can alert on it: a rotation that is legitimate once
	// and constant is a different situation from one that keeps moving.
	driftCounter int64
	// onDrift is called once per drifted observation. The library takes a
	// callback rather than a logger so it stays free of a logging dependency
	// and the caller decides what an authority rotation is worth.
	onDrift func(drift string)
}

type mintCall struct {
	done chan struct{}
	obs  observation
	err  error
}

// NewMintVerifier returns a verifier reading from reader.
func NewMintVerifier(reader MintReader, c cluster.Cluster) *MintVerifier {
	return &MintVerifier{
		reader:   reader,
		cluster:  c,
		ttl:      DefaultMintVerificationTTL,
		now:      func() time.Time { return time.Now().UTC() },
		cache:    map[string]observation{},
		inflight: map[string]*mintCall{},
	}
}

// OnDrift registers a callback for each authority-pin drift observed on use.
//
// Settlement does not stop on a drift: the mint is the same mint, and a
// legitimate issuer rotation must not become a marketplace outage. It is a
// governance change that a human still has to look at, so it is reported once
// per observation rather than swallowed.
func (v *MintVerifier) OnDrift(fn func(drift string)) *MintVerifier {
	v.onDrift = fn
	return v
}

// SetTTL overrides the observation window, for tests.
func (v *MintVerifier) SetTTL(ttl time.Duration) *MintVerifier {
	v.ttl = ttl
	return v
}

// SetClock overrides the clock, for tests.
func (v *MintVerifier) SetClock(now func() time.Time) *MintVerifier {
	v.now = now
	return v
}

// AuthorityDrifts is the number of authority-pin drifts observed on use.
func (v *MintVerifier) AuthorityDrifts() int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.driftCounter
}

// Verify reports whether the chain still holds the mint this registry governs.
//
// The three outcomes are kept apart on purpose, because they mean different
// things to different people:
//
//   - a hard invariant failing returns ErrMintUnverified: the token is not the
//     one that was priced, and settlement must stop.
//   - a governance pin drifting returns nil and increments a counter: the token
//     is still the same token, the issuer changed something about it.
//   - the node being unreachable returns ErrMintUnreachable: nothing is known,
//     and the caller should retry rather than conclude.
func (v *MintVerifier) Verify(ctx context.Context, mint tokens.Token) error {
	obs, err := v.observe(ctx, mint)
	if err != nil {
		return err
	}
	if !obs.holds() {
		return fmt.Errorf("%w: %s (%s) %s", ErrMintUnverified, mint.Address, mint.Symbol, obs.fault)
	}
	return nil
}

// observe returns the current reading of a mint, from cache when fresh and from
// a single shared RPC call otherwise.
func (v *MintVerifier) observe(ctx context.Context, mint tokens.Token) (observation, error) {
	key := string(v.cluster) + "/" + mint.Address
	now := v.now().UTC()

	v.mu.Lock()
	if cached, ok := v.cache[key]; ok && now.Sub(cached.at) < v.ttl {
		v.mu.Unlock()
		return cached, nil
	}
	if call, ok := v.inflight[key]; ok {
		// Someone is already reading this mint. Wait for their answer rather
		// than issuing a second identical request.
		v.mu.Unlock()
		select {
		case <-call.done:
			return call.obs, call.err
		case <-ctx.Done():
			return observation{}, ctx.Err()
		}
	}
	call := &mintCall{done: make(chan struct{})}
	v.inflight[key] = call
	v.mu.Unlock()

	obs, readErr := v.fetch(ctx, mint)
	// The read itself runs detached, so a caller that walks away cannot poison
	// the buyers sharing its flight. That makes two separate questions: did the
	// read work, and is this caller still waiting. Collapsing them would either
	// fail innocent waiters with the leader's cancellation or silently answer a
	// buyer that already gave up.
	fetchErr := readErr
	if fetchErr == nil {
		v.mu.Lock()
		delete(v.inflight, key)
		v.cache[key] = obs
		// Counted when the observation is recorded, not when it is read. A
		// counter that incremented per Verify would track request volume, so an
		// operator could not tell a single rotated authority from a busy
		// afternoon, which is the only thing the counter exists to show.
		drifted := obs.drift != ""
		if drifted {
			v.driftCounter++
		}
		v.mu.Unlock()
		if drifted && v.onDrift != nil {
			v.onDrift(obs.drift)
		}
	} else {
		v.mu.Lock()
		delete(v.inflight, key)
		v.mu.Unlock()
	}

	// Waiters are answered with what the read found, because that is what they
	// asked for and the work was already done on their behalf.
	call.obs, call.err = obs, fetchErr
	close(call.done)

	// The leader alone is told about its own context, and only after the shared
	// result has been published.
	if err := ctx.Err(); err != nil {
		return observation{}, err
	}
	return obs, readErr
}

// fetch reads the mint account and compares it against the governed entry.
func (v *MintVerifier) fetch(ctx context.Context, mint tokens.Token) (observation, error) {
	addr, err := solana.PublicKeyFromBase58(mint.Address)
	if err != nil {
		return observation{fault: fmt.Sprintf("is not a Solana address: %v", err)}, nil
	}
	// The shared read outlives any one caller's context. A buyer who gives up
	// must not fail the buyers who joined the same flight, and the call is
	// already in the endpoint's path.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.ttl)
	defer cancel()

	info, err := v.reader.GetAccountInfo(readCtx, addr)
	if err != nil {
		return observation{}, fmt.Errorf("%w: read %s: %v", ErrMintUnreachable, mint.Address, err)
	}
	obs := observation{at: v.now().UTC()}
	if !info.Exists {
		obs.fault = fmt.Sprintf("has no account on %s", v.cluster)
		return obs, nil
	}
	if !info.Owner.Equals(solana.MustPublicKeyFromBase58(tokens.TokenProgram)) {
		obs.fault = fmt.Sprintf("is owned by %s, not %s", info.Owner, tokens.TokenProgram)
		return obs, nil
	}
	decoded, err := preflight.DecodeMint(info.Data)
	if err != nil {
		obs.fault = err.Error()
		return obs, nil
	}
	if !decoded.IsInitialized {
		obs.fault = "is not initialized"
		return obs, nil
	}
	if int(decoded.Decimals) != mint.Decimals {
		// The one that matters most. Decimals is the exponent between the
		// decimal price an agent quoted and the integer base units that move,
		// so a mismatch here is a 10^n error rather than a rounding difference.
		obs.fault = fmt.Sprintf("reports %d decimals, governed set pins %d", decoded.Decimals, mint.Decimals)
		return obs, nil
	}
	obs.mint = decoded
	obs.drift = preflight.AuthorityDrift(mint, decoded)
	return obs, nil
}

// holds reports whether the observation satisfies every hard invariant.
func (o observation) holds() bool { return o.fault == "" }
