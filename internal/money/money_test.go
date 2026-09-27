package money

import (
	"encoding/json"
	"errors"
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
