package tokens

import (
	"errors"
	"sync"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/cluster"
)

const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
const eurch = "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr"

// devnet USDC is a different account from mainnet USDC. Circle deploys a
// separate mint per cluster, and treating the two as one address is the mistake
// that would let a mainnet-priced offer settle against a devnet account.
const devnetUSDC = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"

func TestForClusterSeedsLaunchMints(t *testing.T) {
	// EURC is deliberately the same address on both clusters, so this table also
	// pins the fact that a shared address does not imply a shared mint account.
	for _, c := range []cluster.Cluster{cluster.MainnetBeta, cluster.Devnet} {
		reg, err := ForCluster(c)
		if err != nil {
			t.Fatalf("ForCluster(%s): %v", c, err)
		}
		mints := []string{usdc, eurch}
		if c == cluster.Devnet {
			mints = []string{devnetUSDC, eurch}
		}
		for _, mint := range mints {
			entry, err := reg.Enabled(mint)
			if err != nil {
				t.Errorf("%s Enabled(%s): %v", c, mint, err)
				continue
			}
			if entry.Decimals != 6 {
				t.Errorf("%s %s decimals = %d, want 6", c, entry.Symbol, entry.Decimals)
			}
			if !entry.MintInfo().Known {
				t.Errorf("%s %s should project as a known mint", c, entry.Symbol)
			}
			if entry.Cluster != string(c) {
				t.Errorf("%s entry cluster = %q, want %q", c, entry.Cluster, c)
			}
		}
		if got := len(reg.List()); got != 2 {
			t.Errorf("%s List() = %d entries, want 2", c, got)
		}
	}
}

func TestUSDCIsADistinctAccountPerCluster(t *testing.T) {
	// A registry that resolved devnet USDC to the mainnet address would produce
	// a transaction moving an account the seller never offered, so the two must
	// not be conflated. Re-derive these from getAccountInfo rather than trusting
	// that the addresses look related.
	mainnet, err := ForCluster(cluster.MainnetBeta)
	if err != nil {
		t.Fatal(err)
	}
	devnet, err := ForCluster(cluster.Devnet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mainnet.Enabled(devnetUSDC); !errors.Is(err, ErrUngoverned) {
		t.Errorf("mainnet should not govern devnet USDC: %v", err)
	}
	if _, err := devnet.Enabled(usdc); !errors.Is(err, ErrUngoverned) {
		t.Errorf("devnet should not govern mainnet USDC: %v", err)
	}
}

func TestForClusterPinsBothAuthorities(t *testing.T) {
	reg, err := ForCluster(cluster.Devnet)
	if err != nil {
		t.Fatal(err)
	}
	// A freeze authority is a fact about the account, not an opinion. Leaving it
	// blank asserts that the issuer cannot freeze, which is stronger than
	// "unverified" and is the kind of wrong that only shows up as a surprise.
	for _, mint := range reg.List() {
		if mint.MintAuthority == "" {
			t.Errorf("%s has no mint authority pinned", mint.Symbol)
		}
		if mint.FreezeAuthority == "" {
			t.Errorf("%s has no freeze authority pinned; devnet Circle mints are freezable", mint.Symbol)
		}
	}
}

func TestLocalnetGovernsNothingUntilTold(t *testing.T) {
	reg, err := ForCluster(cluster.Localnet)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reg.List()); got != 0 {
		t.Errorf("List() = %d entries, want 0: a validator has no token to ship with", got)
	}
	if _, err := reg.Enabled(usdc); !errors.Is(err, ErrUngoverned) {
		t.Errorf("err = %v, want ErrUngoverned on an empty cluster", err)
	}
}

func TestLocalnetAcceptsOperatorMints(t *testing.T) {
	reg, err := ForCluster(cluster.Localnet, Token{
		Address: usdc, Symbol: "USDC", Decimals: 6, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Enabled(usdc); err != nil {
		t.Errorf("Enabled: %v", err)
	}
}

func TestPublicClusterRefusesOperatorMints(t *testing.T) {
	// Allowing an extra mint on mainnet-beta would let a typo or a
	// misconfiguration settle a trade in a token this project does not govern,
	// and the preflight would be asked to bless it. The fix is to use localnet.
	_, err := ForCluster(cluster.MainnetBeta, Token{
		Address: usdc, Symbol: "USDC", Decimals: 6, Enabled: true,
	})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestTestnetIsRecognisedAndRefused(t *testing.T) {
	// testnet differs from devnet by one character in a string an operator types.
	// It must be refused as an unsupported cluster rather than reported as an
	// unknown one, because "not supported" tells the operator what to do next and
	// "unknown" invites them to keep guessing at spellings.
	_, err := cluster.Parse("testnet")
	if !errors.Is(err, cluster.ErrClusterUnsupported) {
		t.Errorf("Parse(testnet) err = %v, want ErrClusterUnsupported", err)
	}
	if _, err := New(cluster.Testnet, nil); !errors.Is(err, cluster.ErrClusterUnsupported) {
		t.Errorf("New(testnet) err = %v, want ErrClusterUnsupported", err)
	}
}

func TestMintGovernedElsewhereIsUngovernedHere(t *testing.T) {
	// The distinction matters operationally: ErrUngoverned means "this is a real
	// token, wrong chain", which is a configuration answer, while
	// ErrNotRegistered means nobody has ever heard of this address.
	empty, err := New(cluster.Localnet, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Enabled(usdc); !errors.Is(err, ErrUngoverned) {
		t.Errorf("err = %v, want ErrUngoverned", err)
	}
	if _, err := empty.Enabled("11111111111111111111111111111111"); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("err = %v, want ErrNotRegistered for an address governed nowhere", err)
	}
}

func TestListIsSortedAndCopied(t *testing.T) {
	reg, err := ForCluster(cluster.MainnetBeta)
	if err != nil {
		t.Fatal(err)
	}
	list := reg.List()
	if list[0].Symbol != "EURC" || list[1].Symbol != "USDC" {
		t.Errorf("List() not sorted by symbol: %s, %s", list[0].Symbol, list[1].Symbol)
	}
	list[0].Symbol = "MUTATED"
	if again := reg.List(); again[0].Symbol != "EURC" {
		t.Error("List() returned a slice aliasing internal state")
	}
}

func TestDisabledMintIsRejected(t *testing.T) {
	reg, err := New(cluster.Localnet, []Token{{Address: usdc, Symbol: "USDC", Decimals: 6, Enabled: false}})
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
		if _, err := New(cluster.Localnet, []Token{entry}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestNewRejectsDuplicateMints(t *testing.T) {
	_, err := New(cluster.Localnet, []Token{
		{Address: usdc, Symbol: "USDC", Decimals: 6, Enabled: true},
		{Address: usdc, Symbol: "USD-C", Decimals: 6, Enabled: true},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid for duplicate mint", err)
	}
}

func TestRegistryIsConcurrencySafe(t *testing.T) {
	reg, err := ForCluster(cluster.MainnetBeta)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				reg.Enabled(usdc)
				reg.Lookup(eurch)
				reg.List()
				reg.Cluster()
			}
		}()
	}
	wg.Wait()
}
