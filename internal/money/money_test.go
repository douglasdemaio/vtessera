package money

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestParseAccepts(t *testing.T) {
	for _, s := range []string{"0", "1", "0.5", "25", "1234567890.123456789", "0.000000001"} {
		if _, err := Parse(s); err != nil {
			t.Errorf("Parse(%q) = %v, want ok", s, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]error{
		"":                     ErrNonCanonical,
		"-1":                   ErrNegative,
		"+1":                   ErrNonCanonical,
		"1e3":                  ErrNonCanonical,
		"1.5e3":                ErrNonCanonical,
		".5":                   ErrNonCanonical,
		"1.":                   ErrNonCanonical,
		"007":                  ErrNonCanonical,
		"1,5":                  ErrNonCanonical,
		"1 000":                ErrNonCanonical,
		"1.1234567890":         ErrNonCanonical,
		"12345678901234567890": ErrNonCanonical,
	}
	for s, want := range cases {
		if _, err := Parse(s); !errors.Is(err, want) {
			t.Errorf("Parse(%q) = %v, want %v", s, err, want)
		}
	}
}

func TestBaseUnits(t *testing.T) {
	cases := []struct {
		amount   string
		decimals int
		want     uint64
		wantErr  error
	}{
		{"1", 6, 1000000, nil},
		{"0.5", 6, 500000, nil},
		{"25", 0, 25, nil},
		{"0.000000001", 9, 1, nil},
		{"0", 6, 0, nil},
		{"1.5", 0, 0, ErrTooPrecise},
		{"1.5", 2, 150, nil},
		{"18446744073709.551615", 6, 18446744073709551615, nil},
		{"18446744073709.551616", 6, 0, ErrOverflow},
	}
	for _, c := range cases {
		got, err := MustParse(c.amount).BaseUnits(c.decimals)
		if c.wantErr != nil {
			if !errors.Is(err, c.wantErr) {
				t.Errorf("%s.BaseUnits(%d) err = %v, want %v", c.amount, c.decimals, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s.BaseUnits(%d) unexpected err %v", c.amount, c.decimals, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s.BaseUnits(%d) = %d, want %d", c.amount, c.decimals, got, c.want)
		}
	}
}

func TestCmp(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1", "1", 0},
		{"1.0", "1", 0},
		{"1.10", "1.1", 0},
		{"1", "2", -1},
		{"2", "1", 1},
		{"10", "9.999999999", 1},
		{"0.1", "0.100000001", -1},
		{"12345.6", "12345.6", 0},
	}
	for _, c := range cases {
		if got := MustParse(c.a).Cmp(MustParse(c.b)); got != c.want {
			t.Errorf("%s.Cmp(%s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsZero(t *testing.T) {
	for _, s := range []string{"0", "0.0", "0.000"} {
		if !MustParse(s).IsZero() {
			t.Errorf("IsZero(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"0.0001", "1"} {
		if MustParse(s).IsZero() {
			t.Errorf("IsZero(%q) = true, want false", s)
		}
	}
}

func TestJSONRoundTripIsString(t *testing.T) {
	b, err := json.Marshal(MustParse("12.5"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"12.5"` {
		t.Errorf("marshal = %s, want %q", b, `"12.5"`)
	}
	var back Amount
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.String() != "12.5" {
		t.Errorf("round trip = %q, want %q", back, "12.5")
	}
}

func TestJSONRejectsNumber(t *testing.T) {
	var a Amount
	if err := json.Unmarshal([]byte(`12.5`), &a); !errors.Is(err, ErrNotAStringValue) {
		t.Errorf("unmarshal number err = %v, want %v", err, ErrNotAStringValue)
	}
}

func TestJSONRejectsNonCanonicalString(t *testing.T) {
	var a Amount
	if err := json.Unmarshal([]byte(`"-1"`), &a); !errors.Is(err, ErrNegative) {
		t.Errorf("unmarshal %q err = %v, want %v", "-1", err, ErrNegative)
	}
}

func TestMustParsePanicsOnInvalidAmount(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustParse did not panic on invalid amount")
		}
	}()
	MustParse("-1")
}

func TestBaseUnitsRejectsOutOfRangeDecimals(t *testing.T) {
	for _, decimals := range []int{-1, MaxFractionDigits + 1} {
		if _, err := MustParse("1").BaseUnits(decimals); !errors.Is(err, ErrNonCanonical) {
			t.Errorf("BaseUnits(%d) err = %v, want %v", decimals, err, ErrNonCanonical)
		}
	}
}

func TestNewFromBaseUnitsRoundTripsThroughBaseUnits(t *testing.T) {
	for _, tc := range []struct {
		units    uint64
		decimals int
		want     string
	}{
		{0, 6, "0.000000"},
		{1, 6, "0.000001"},
		{1000000, 6, "1.000000"},
		{4500000000, 6, "4500.000000"},
		{999, 2, "9.99"},
	} {
		amount, err := NewFromBaseUnits(tc.units, tc.decimals)
		if err != nil {
			t.Fatalf("NewFromBaseUnits(%d, %d): %v", tc.units, tc.decimals, err)
		}
		if amount.String() != tc.want {
			t.Errorf("NewFromBaseUnits(%d, %d) = %s, want %s", tc.units, tc.decimals, amount, tc.want)
		}
		back, err := amount.BaseUnits(tc.decimals)
		if err != nil {
			t.Fatalf("BaseUnits(%d) on %s: %v", tc.decimals, amount, err)
		}
		if back != tc.units {
			t.Errorf("%s read back as %d units, want %d", amount, back, tc.units)
		}
	}
}

func TestNewFromBaseUnitsRefusesWhatItCannotRepresent(t *testing.T) {
	// An unrepresentable scale or magnitude must be an error rather than an
	// Amount that reads as empty: this type exists so a figure cannot quietly
	// stop being the figure it was.
	if _, err := NewFromBaseUnits(1, MaxFractionDigits+1); !errors.Is(err, ErrBadScale) {
		t.Errorf("NewFromBaseUnits with too many decimals err = %v, want %v", err, ErrBadScale)
	}
	if _, err := NewFromBaseUnits(1, 0); !errors.Is(err, ErrBadScale) {
		t.Errorf("NewFromBaseUnits with no decimals err = %v, want %v", err, ErrBadScale)
	}

	// The largest count there is still renders, at every acceptable scale, and
	// comes back to the same units. This is the boundary the overflow guard above
	// exists to defend, and the reason the package cannot be given a scale that
	// makes a uint64 unrepresentable without this failing.
	largest := uint64(math.MaxUint64)
	for decimals := 1; decimals <= MaxFractionDigits; decimals++ {
		amount, err := NewFromBaseUnits(largest, decimals)
		if err != nil {
			t.Fatalf("NewFromBaseUnits(MaxUint64, %d): %v", decimals, err)
		}
		back, err := amount.BaseUnits(decimals)
		if err != nil {
			t.Fatalf("BaseUnits(%d) on %s: %v", decimals, amount, err)
		}
		if back != largest {
			t.Errorf("MaxUint64 at %d decimals came back as %d", decimals, back)
		}
		if _, err := Parse(amount.String()); err != nil {
			t.Errorf("%s is not canonical: %v", amount, err)
		}
	}
}
