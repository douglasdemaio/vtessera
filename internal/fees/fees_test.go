package fees

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gagliardetto/solana-go"
)

func TestDefaultMatchesSpec(t *testing.T) {
	p := Default()
	if p.Lamports() != 1_000 {
		t.Errorf("lamports = %d, want 1000", p.Lamports())
	}
	if p.WalletAddress() != DefaultWallet {
		t.Errorf("wallet = %s, want %s", p.WalletAddress(), DefaultWallet)
	}
	if got, want := p.Wallet(), solana.MustPublicKeyFromBase58(DefaultWallet); !got.Equals(want) {
		t.Errorf("wallet key = %s, want %s", got, want)
	}
}

func TestNewRejectsZeroLamports(t *testing.T) {
	if _, err := New(0, DefaultWallet); !errors.Is(err, ErrInvalidLamports) {
		t.Errorf("err = %v, want ErrInvalidLamports", err)
	}
}

func TestNewRejectsBadWallet(t *testing.T) {
	for _, wallet := range []string{"", "not-base58", "1111", "J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakx!"} {
		if _, err := New(1, wallet); !errors.Is(err, ErrInvalidWallet) {
			t.Errorf("wallet %q: err = %v, want ErrInvalidWallet", wallet, err)
		}
	}
}

func TestNewAcceptsOverride(t *testing.T) {
	p, err := New(1, "11111111111111111111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if p.Lamports() != 1 {
		t.Errorf("lamports = %d, want 1", p.Lamports())
	}
}

func TestMarshalJSON(t *testing.T) {
	raw, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["lamports"] != float64(1_000) {
		t.Errorf("lamports = %v, want 1000", got["lamports"])
	}
	if got["wallet"] != DefaultWallet {
		t.Errorf("wallet = %v, want %s", got["wallet"], DefaultWallet)
	}
	if _, ok := got["description"].(string); !ok {
		t.Errorf("description missing from %s", raw)
	}
}
