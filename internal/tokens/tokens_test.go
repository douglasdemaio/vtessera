package tokens

import (
	"errors"
	"sync"
	"testing"
)

const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
const eurch = "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr"

func TestDefaultSeedsLaunchMints(t *testing.T) {
	reg := Default()
	for _, mint := range []string{usdc, eurch} {
		entry, err := reg.Enabled(mint)
		if err != nil {
			t.Fatalf("Enabled(%s): %v", mint, err)
		}
		if entry.Decimals != 6 {
			t.Errorf("%s decimals = %d, want 6", entry.Symbol, entry.Decimals)
		}
		if !entry.MintInfo().Known {
			t.Errorf("%s should project as a known mint", entry.Symbol)
		}
	}
	if got := len(reg.List()); got != 2 {
		t.Errorf("List() = %d entries, want 2", got)
	}
}

func TestListIsSortedAndCopied(t *testing.T) {
	reg := Default()
	list := reg.List()
	if list[0].Symbol != "EURC" || list[1].Symbol != "USDC" {
		t.Errorf("List() not sorted by symbol: %s, %s", list[0].Symbol, list[1].Symbol)
	}
	list[0].Symbol = "MUTATED"
	if again := reg.List(); again[0].Symbol != "EURC" {
		t.Error("List() returned a slice aliasing internal state")
	}
}

func TestUnknownMintIsNotRegistered(t *testing.T) {
	_, err := Default().Enabled("11111111111111111111111111111111")
	if !errors.Is(err, ErrNotRegistered) {
		t.Errorf("err = %v, want ErrNotRegistered", err)
	}
}

func TestDisabledMintIsRejected(t *testing.T) {
	reg, err := New([]Token{{Address: usdc, Symbol: "USDC", Decimals: 6, Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Enabled(usdc); !errors.Is(err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
	// Still discoverable, just not settleable.
	if _, ok := reg.Lookup(usdc); !ok {
		t.Error("disabled mint should still be discoverable via Lookup")
	}
}

func TestNewRejectsInvalidEntries(t *testing.T) {
	cases := map[string]Token{
		"bad address":       {Address: "nope", Symbol: "X", Decimals: 6},
		"no symbol":         {Address: usdc, Decimals: 6},
		"neg decimals":      {Address: usdc, Symbol: "USDC", Decimals: -1},
		"too many decimals": {Address: usdc, Symbol: "USDC", Decimals: 10},
	}
	for name, entry := range cases {
		if _, err := New([]Token{entry}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestNewRejectsDuplicateMints(t *testing.T) {
	_, err := New([]Token{
		{Address: usdc, Symbol: "USDC", Decimals: 6, Enabled: true},
		{Address: usdc, Symbol: "USD-C", Decimals: 6, Enabled: true},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid for duplicate mint", err)
	}
}

func TestRegistryIsConcurrencySafe(t *testing.T) {
	reg := Default()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				reg.Enabled(usdc)
				reg.Lookup(eurch)
				reg.List()
			}
		}()
	}
	wg.Wait()
}
