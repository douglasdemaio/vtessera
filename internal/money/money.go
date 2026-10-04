package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	MaxFractionDigits = 9
	MaxIntegerDigits  = 19
)

var (
	ErrOverflow        = errors.New("amount does not fit in base units")
	ErrTooPrecise      = errors.New("amount has more fraction digits than the mint supports")
	ErrNegative        = errors.New("amount must not be negative")
	ErrNonCanonical    = errors.New("amount is not in canonical form")
	ErrNotAStringValue = errors.New("amount must be a JSON string")
	ErrBadScale        = errors.New("decimals is not a representable scale")
)

type Amount struct {
	value string
}

func Parse(s string) (Amount, error) {
	if s == "" {
		return Amount{}, fmt.Errorf("%w: empty", ErrNonCanonical)
	}
	if strings.HasPrefix(s, "-") {
		return Amount{}, ErrNegative
	}
	if strings.HasPrefix(s, "+") || strings.ContainsAny(s, "eE") {
		return Amount{}, fmt.Errorf("%w: %q", ErrNonCanonical, s)
	}
	integer, fraction, hasFraction := strings.Cut(s, ".")
	if integer == "" || len(integer) > MaxIntegerDigits {
		return Amount{}, fmt.Errorf("%w: %q", ErrNonCanonical, s)
	}
	if len(integer) > 1 && integer[0] == '0' {
		return Amount{}, fmt.Errorf("%w: %q", ErrNonCanonical, s)
	}
	if !allDigits(integer) {
		return Amount{}, fmt.Errorf("%w: %q", ErrNonCanonical, s)
	}
	if hasFraction {
		if len(fraction) == 0 || len(fraction) > MaxFractionDigits {
			return Amount{}, fmt.Errorf("%w: %q", ErrNonCanonical, s)
		}
		if !allDigits(fraction) {
			return Amount{}, fmt.Errorf("%w: %q", ErrNonCanonical, s)
		}
	}
	return Amount{value: s}, nil
}

func MustParse(s string) Amount {
	a, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return a
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (a Amount) String() string {
	return a.value
}

func (a Amount) IsZero() bool {
	return strings.Trim(a.value, "0.") == ""
}

func (a Amount) Cmp(b Amount) int {
	ai, af, _ := strings.Cut(a.value, ".")
	bi, bf, _ := strings.Cut(b.value, ".")
	scale := max(len(af), len(bf))
	width := max(len(ai), len(bi))
	at := align(ai, af, width, scale)
	bt := align(bi, bf, width, scale)
	switch {
	case at > bt:
		return 1
	case at < bt:
		return -1
	default:
		return 0
	}
}

func align(integer, fraction string, width, scale int) string {
	var b strings.Builder
	for i := len(integer); i < width; i++ {
		b.WriteByte('0')
	}
	b.WriteString(integer)
	b.WriteString(fraction)
	for i := len(fraction); i < scale; i++ {
		b.WriteByte('0')
	}
	return b.String()
}

func (a Amount) BaseUnits(decimals int) (uint64, error) {
	if decimals < 0 || decimals > MaxFractionDigits {
		return 0, fmt.Errorf("%w: %d", ErrNonCanonical, decimals)
	}
	integer, fraction, _ := strings.Cut(a.value, ".")
	if len(fraction) > decimals {
		return 0, fmt.Errorf("%w: %d fraction digits given, mint has %d", ErrTooPrecise, len(fraction), decimals)
	}
	digits := integer + fraction + strings.Repeat("0", decimals-len(fraction))
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return 0, nil
	}
	units, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrOverflow, err)
	}
	return units, nil
}

// NewFromBaseUnits builds an Amount from a count of base units. It is the exact
// inverse of BaseUnits, so parsing a rendered amount and converting it back
// returns the same units. It exists because an aggregate computed over many
// amounts has to be reported in the same decimal form the amounts arrived in, and
// rendering that by dividing a float would reintroduce the imprecision this type
// exists to remove.
//
// It reports an error rather than an unusable amount for a scale or a magnitude
// it cannot represent. The callers all pass a compile-time scale, so that error
// is unreachable in practice; a money type that answers a bad question with an
// empty string is worse than one that says it cannot answer.
func NewFromBaseUnits(units uint64, decimals int) (Amount, error) {
	if decimals <= 0 || decimals > MaxFractionDigits {
		return Amount{}, fmt.Errorf("%w: %d", ErrBadScale, decimals)
	}
	digits := strconv.FormatUint(units, 10)
	// Unreachable with today's constants, and kept anyway: a uint64 is at most
	// twenty digits and the smallest acceptable scale allows exactly twenty, so
	// the guard only fires if one of those constants moves. It costs one
	// comparison and it is what keeps such a change from quietly producing an
	// amount this package would then refuse to parse.
	if len(digits) > MaxIntegerDigits+decimals {
		return Amount{}, fmt.Errorf("%w: %d base units at %d decimals", ErrOverflow, units, decimals)
	}
	if len(digits) <= decimals {
		digits = strings.Repeat("0", decimals-len(digits)+1) + digits
	}
	return Amount{value: digits[:len(digits)-decimals] + "." + digits[len(digits)-decimals:]}, nil
}

func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.value)
}

func (a *Amount) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("%w: %v", ErrNotAStringValue, err)
	}
	parsed, err := Parse(s)
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}
