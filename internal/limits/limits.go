// Package limits holds the spending caps that bound what one agent can commit
// through the marketplace, and the prices those caps are denominated in.
//
// The caps are in USD and the trades are in tokens, so something has to convert
// between them. Nothing in this service has ever known a price, and adding a
// price feed would mean trusting a new external party with the number that
// decides whether a trade is allowed. So each supported mint carries a rate that
// an operator declares and can audit, and a mint with no declared rate cannot be
// traded at all.
//
// That refusal is the whole point. A cap that quietly does not apply to a mint it
// has no price for is not a cap: an agent would pick the unpriced currency and
// spend without limit. So the rule here is that an unknown mint is refused, at
// the moment the seller chooses a currency, rather than at the moment a buyer
// tries to spend.
package limits

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strings"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
)

// USDScale is the number of decimal places USD amounts are held at internally.
// Six places is the same scale as the stablecoins this marketplace prices, so a
// par rate converts without rounding at all.
const USDScale = 6

var (
	// ErrNoRate means the mint has no declared USD rate. It is returned instead
	// of a zero rate, because a zero rate would make every trade free and a
	// missing rate must never be read as one.
	ErrNoRate = errors.New("no USD rate is declared for this mint")
	// ErrRateInvalid means a declared rate cannot be used: it must be a
	// positive decimal that fits the internal scale.
	ErrRateInvalid = errors.New("USD rate must be a positive decimal amount")
	// ErrCapInvalid means a cap cannot be used: it must be a positive decimal.
	ErrCapInvalid = errors.New("spending cap must be a positive decimal amount")
	// ErrCapNotPositive means a cap was raised to zero, which would stop the
	// agent trading rather than bound it.
	ErrCapNotPositive = errors.New("spending cap must be greater than zero")
	// ErrAboveCeiling means an agent asked for more than this deployment allows
	// any agent to have. The ceiling is the operator's, not the agent's.
	ErrAboveCeiling = errors.New("requested cap is above this deployment's ceiling")
)

// rate is what one whole token of a mint is worth in USD.
type rate struct {
	decimals uint8
	microUSD uint64 // USD per whole token, scaled by 10^USDScale
}

// Rate is a declared price for one mint.
type Rate struct {
	Decimals uint8
	USD      money.Amount
}

// Rates maps a mint address to its declared price. It is the auditable answer to
// "what is a USDC worth in this service", and the only place a price enters the
// system.
type Rates map[string]Rate

// governedPar lists the stablecoins this marketplace prices at par, on the
// clusters where they are governed. These are the same addresses the governed
// mint table pins, which is verified against both public clusters by
// `make preflight-live`; they are not derived from anything here.
//
// A stablecoin trading below its peg makes these caps wrong in the conservative
// direction for a USD-denominated limit only if it depegs upward, which is the
// direction that matters: a token worth more than a dollar spends more real
// value than the cap counted. An operator who needs that bounded should declare
// a rate above par for that mint.
var governedPar = map[string]Rate{
	// USDC on mainnet-beta.
	"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": {Decimals: 6, USD: money.MustParse("1.00")},
	// USDC on devnet. A different address from mainnet's USDC.
	"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU": {Decimals: 6, USD: money.MustParse("1.00")},
	// EURC, which is the same address on mainnet-beta and devnet.
	"HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr": {Decimals: 6, USD: money.MustParse("1.00")},
}

// DefaultRates returns the built-in par rates for the governed stablecoins.
func DefaultRates() Rates {
	out := make(Rates, len(governedPar))
	for mint, r := range governedPar {
		out[mint] = r
	}
	return out
}

// Add declares a rate, overriding any built-in one for the same mint.
func (r Rates) Add(mint string, rate Rate) error {
	if _, err := ParsePublicKeyish(mint); err != nil {
		return fmt.Errorf("rate mint %q: %w", mint, err)
	}
	if rate.Decimals > 9 {
		return fmt.Errorf("rate mint %q: %d decimals, the most this service supports is 9", mint, rate.Decimals)
	}
	if _, err := toMicro(rate.USD); err != nil {
		return fmt.Errorf("rate mint %q: %w", mint, err)
	}
	r[mint] = rate
	return nil
}

// ParsePublicKeyish rejects a rate for anything that is not a mint address, so a
// typo in configuration is a startup error rather than a rate that silently never
// matches a real mint. The length is not enough to tell: base58 addresses of 32
// bytes run from 32 to 44 characters, so a dropped character still lands inside
// the range. The address is decoded instead, by the same validator the rest of
// the service uses for mints.
func ParsePublicKeyish(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", errors.New("mint address is empty")
	}
	if err := domain.ValidateMint(s); err != nil {
		return "", fmt.Errorf("%q is not a base58 mint address: %w", s, err)
	}
	return s, nil
}

// Rates returns the price table, for a caller that has to audit what is priced.
func (p Policy) Rates() Rates { return p.rates }

// Lookup returns the declared rate for a mint.
func (r Rates) Lookup(mint string) (Rate, bool) {
	rate, ok := r[mint]
	return rate, ok
}

// Missing lists the governed mints that carry no declared rate. A non-empty
// result means this deployment would refuse to trade in those currencies, which
// is reported at boot rather than discovered by an agent mid-trade.
func (r Rates) Missing(governed []string) []string {
	var out []string
	for _, mint := range governed {
		if _, ok := r[mint]; !ok {
			out = append(out, mint)
		}
	}
	sort.Strings(out)
	return out
}

// String renders the table for a log line or an operator-facing report.
func (r Rates) String() string {
	mints := make([]string, 0, len(r))
	for mint := range r {
		mints = append(mints, mint)
	}
	sort.Strings(mints)
	parts := make([]string, 0, len(mints))
	for _, mint := range mints {
		parts = append(parts, fmt.Sprintf("%s=%s (%ddp)", mint, r[mint].USD, r[mint].Decimals))
	}
	return strings.Join(parts, ", ")
}

// Policy is the set of caps in force for one agent on one deployment.
//
// PerTrade and PerDay are the defaults an agent gets without asking. The
// ceilings are the most this deployment lets any agent hold, which is what makes
// an agent's own opt-in bounded: raising a cap is something an agent may do, but
// only up to a number the operator chose.
type Policy struct {
	PerTrade          money.Amount
	PerDay            money.Amount
	Window            time.Duration
	CeilingPerTrade   money.Amount
	CeilingPerDay     money.Amount
	rates             Rates
	perAgentOverrides func(ctx context.Context, agentID string) (perTrade, perDay money.Amount, ok bool)
}

// DefaultPolicy is the cap set a deployment starts with: ten dollars a trade and
// ten a day, over a rolling window, with the ceilings left unset so an agent
// cannot raise anything until an operator says how far it may go.
//
// The two are equal on purpose. It means one agent can commit one maximum-size
// trade per rolling day and no second one, which is a tighter daily exposure than
// a per-trade cap below a larger daily cap would give.
func DefaultPolicy(rates Rates) Policy {
	return Policy{
		PerTrade: money.MustParse("10.00"),
		PerDay:   money.MustParse("10.00"),
		Window:   24 * time.Hour,
		rates:    rates,
	}
}

// WithRates sets the price table the caps are evaluated against.
func (p Policy) WithRates(rates Rates) Policy {
	p.rates = rates
	return p
}

// WithCeilings sets the most any single agent may raise its caps to.
func (p Policy) WithCeilings(perTrade, perDay money.Amount) Policy {
	p.CeilingPerTrade, p.CeilingPerDay = perTrade, perDay
	return p
}

// WithWindow sets the rolling window the daily cap is measured over.
func (p Policy) WithWindow(d time.Duration) Policy {
	p.Window = d
	return p
}

// WithAgentLimits supplies the per-agent opt-in. It is a function because the
// values live in the store, and the policy itself is built before the store is
// consulted on any given request.
func (p Policy) WithAgentLimits(f func(ctx context.Context, agentID string) (perTrade, perDay money.Amount, ok bool)) Policy {
	p.perAgentOverrides = f
	return p
}

// Validate rejects a policy that cannot be enforced, so a typo is a startup
// error rather than a cap that silently measures nothing.
func (p Policy) Validate() error {
	if p.PerTrade.IsZero() {
		return fmt.Errorf("per-trade cap: %w", ErrCapNotPositive)
	}
	if p.PerDay.IsZero() {
		return fmt.Errorf("daily cap: %w", ErrCapNotPositive)
	}
	if _, err := toMicro(p.PerTrade); err != nil {
		return fmt.Errorf("per-trade cap: %w", err)
	}
	if _, err := toMicro(p.PerDay); err != nil {
		return fmt.Errorf("daily cap: %w", err)
	}
	if p.Window <= 0 {
		return fmt.Errorf("daily window must be positive, got %s", p.Window)
	}
	// Each ceiling is checked against the cap it bounds and nothing else. A
	// per-trade ceiling below the daily cap is ordinary: an operator may let an
	// agent raise a single trade from five to ten while leaving the daily limit at
	// twenty, and refusing that at boot would be pedantry in the wrong direction.
	for _, bound := range []struct {
		name    string
		value   money.Amount
		bounded money.Amount
	}{
		{"ceiling per trade", p.CeilingPerTrade, p.PerTrade},
		{"ceiling per day", p.CeilingPerDay, p.PerDay},
	} {
		if bound.value.IsZero() {
			continue
		}
		if _, err := toMicro(bound.value); err != nil {
			return fmt.Errorf("%s: %w", bound.name, err)
		}
		if bound.value.Cmp(bound.bounded) < 0 {
			return fmt.Errorf("%s %s is below the %s cap it is meant to bound", bound.name, bound.value, bound.bounded)
		}
	}
	return nil
}

// Limits is the effective cap pair for one agent after any opt-in.
type Limits struct {
	PerTrade money.Amount
	PerDay   money.Amount
	// Raised reports that this agent opted in above the deployment default, so
	// the response can say so rather than leaving an agent to guess why its cap
	// differs from the advertised one.
	Raised bool
}

// Priced reports whether a mint can be priced in USD on this deployment. It
// satisfies the registry's pricer question, so a currency with no rate cannot be
// offered.
func (p Policy) Priced(mint string) bool {
	_, ok := p.rates.Lookup(mint)
	return ok
}

// Effective resolves the caps for an agent. An override that is not stored, or
// not parseable, falls back to the deployment default rather than to something
// more permissive.
func (p Policy) Effective(ctx context.Context, agentID string) Limits {
	out := Limits{PerTrade: p.PerTrade, PerDay: p.PerDay}
	if p.perAgentOverrides == nil {
		return out
	}
	perTrade, perDay, ok := p.perAgentOverrides(ctx, agentID)
	if !ok {
		return out
	}
	if !perTrade.IsZero() && perTrade.Cmp(p.PerTrade) > 0 {
		out.PerTrade, out.Raised = perTrade, true
	}
	if !perDay.IsZero() && perDay.Cmp(p.PerDay) > 0 {
		out.PerDay, out.Raised = perDay, true
	}
	return out
}

// Check validates a requested raise against the ceilings. It is separate from
// Effective because raising a cap is a decision the agent asks for and the
// deployment refuses or allows; reading a cap never fails.
func (p Policy) Check(perTrade, perDay money.Amount) error {
	if perTrade.IsZero() {
		return fmt.Errorf("per-trade cap: %w", ErrCapNotPositive)
	}
	if perDay.IsZero() {
		return fmt.Errorf("daily cap: %w", ErrCapNotPositive)
	}
	if _, err := toMicro(perTrade); err != nil {
		return fmt.Errorf("per-trade cap: %w", err)
	}
	if _, err := toMicro(perDay); err != nil {
		return fmt.Errorf("daily cap: %w", err)
	}
	if !p.CeilingPerTrade.IsZero() && perTrade.Cmp(p.CeilingPerTrade) > 0 {
		return fmt.Errorf("per-trade cap %s: %w of %s", perTrade, ErrAboveCeiling, p.CeilingPerTrade)
	}
	if !p.CeilingPerDay.IsZero() && perDay.Cmp(p.CeilingPerDay) > 0 {
		return fmt.Errorf("daily cap %s: %w of %s", perDay, ErrAboveCeiling, p.CeilingPerDay)
	}
	return nil
}

// USDValue converts a token amount into USD, scaled by 10^USDScale.
//
// The division truncates, which rounds a spend down by less than one millionth
// of a dollar. For a cap that is the safe direction: the counted value is never
// higher than the value actually at stake.
func (p Policy) USDValue(amount money.Amount, mint string) (uint64, error) {
	rate, ok := p.rates.Lookup(mint)
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrNoRate, mint)
	}
	units, err := amount.BaseUnits(int(rate.Decimals))
	if err != nil {
		return 0, fmt.Errorf("amount %s for %s: %w", amount, mint, err)
	}
	perToken, err := toMicro(rate.USD)
	if err != nil {
		return 0, fmt.Errorf("rate for %s: %w", mint, err)
	}
	if perToken == 0 {
		return 0, fmt.Errorf("%w: %s is priced at zero", ErrRateInvalid, mint)
	}
	return scaleToUSD(units, perToken), nil
}

// usdScaleFactor is 10^USDScale, the divisor that turns a micro-USD quantity into
// a stored USD quantity. It is derived rather than written as a literal so that
// changing USDScale cannot leave the two disagreeing.
var usdScaleFactor = uint64(math.Pow10(USDScale))

// scaleToUSD computes units * microUSDPerToken / 10^USDScale in 128 bits, so a
// large token amount times a large rate cannot wrap. uint64 alone is not enough:
// the largest representable amount times a par rate overflows.
func scaleToUSD(units, microUSDPerToken uint64) uint64 {
	hi, lo := bits.Mul64(units, microUSDPerToken)
	// A quotient that does not fit in 64 bits cannot be divided by a 64-bit
	// divisor, and must not wrap either. Saturating is the conservative answer:
	// an absurd declared rate has to make a trade look expensive, and wrapping
	// would make it look free.
	if hi >= usdScaleFactor {
		return math.MaxUint64
	}
	q, _ := bits.Div64(hi, lo, usdScaleFactor)
	return q
}

// toMicro converts a USD decimal amount to the internal integer scale.
func toMicro(a money.Amount) (uint64, error) {
	if a.IsZero() {
		return 0, ErrCapInvalid
	}
	micro, err := a.BaseUnits(USDScale)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %v", ErrCapInvalid, a, err)
	}
	return micro, nil
}

// CapMicro converts a cap to the internal integer scale, for callers that
// compare against a summed USD value rather than parsing an amount.
func CapMicro(c money.Amount) (uint64, error) {
	return toMicro(c)
}

// FormatUSD renders an internal USD value as a decimal string.
//
// A micro-USD count at USDScale always renders, so the error is impossible here;
// it is folded into the zero rather than carried, because this is used in refusal
// messages and a message must never become the thing that fails.
func FormatUSD(micro uint64) string {
	amount, err := money.NewFromBaseUnits(micro, USDScale)
	if err != nil {
		return "an unrepresentable amount"
	}
	return amount.String()
}
