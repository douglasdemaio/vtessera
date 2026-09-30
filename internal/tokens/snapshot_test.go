package tokens_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/tokens"
)

// snapshot mirrors internal/tokens/testdata/governed-mints.json. The fields kept
// here are the ones a transcription error would corrupt; the comment block in
// the file is documentation, not data under test.
type snapshot struct {
	TokenProgram string `json:"tokenProgram"`
	Clusters     map[string]struct {
		GenesisHash string `json:"genesisHash"`
		FeeWallet   struct {
			Address            string `json:"address"`
			RentMinimumLamport uint64 `json:"rentMinimumLamports"`
		} `json:"feeWallet"`
		Mints []struct {
			Address         string `json:"address"`
			Symbol          string `json:"symbol"`
			Decimals        int    `json:"decimals"`
			MintAuthority   string `json:"mintAuthority"`
			FreezeAuthority string `json:"freezeAuthority"`
		} `json:"mints"`
	} `json:"clusters"`
}

func loadSnapshot(t *testing.T) snapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/governed-mints.json")
	if err != nil {
		t.Fatal(err)
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

// The shipped governed table is asserted against a recorded snapshot, so a
// changed base58 constant is a reviewable diff rather than a quiet correction.
//
// This is the direct regression guard for the Phase 2 defect: a lookalike EURC
// address that does not exist on chain passed every unit test, because a unit
// test only checks the code against itself. An external record is the only
// thing that can disagree.
func TestTheShippedTableMatchesTheRecordedSnapshot(t *testing.T) {
	snap := loadSnapshot(t)

	if snap.TokenProgram != tokens.TokenProgram {
		t.Errorf("token program = %s, snapshot says %s", tokens.TokenProgram, snap.TokenProgram)
	}

	for _, c := range []cluster.Cluster{cluster.MainnetBeta, cluster.Devnet} {
		t.Run(string(c), func(t *testing.T) {
			recorded, ok := snap.Clusters[string(c)]
			if !ok {
				t.Fatalf("the snapshot has no entry for %s", c)
			}
			if recorded.GenesisHash != c.GenesisHash() {
				t.Errorf("genesis = %s, snapshot says %s", c.GenesisHash(), recorded.GenesisHash)
			}
			if recorded.FeeWallet.Address != fees.DefaultWallet {
				t.Errorf("fee wallet = %s, snapshot says %s", fees.DefaultWallet, recorded.FeeWallet.Address)
			}

			registry, err := tokens.ForCluster(c)
			if err != nil {
				t.Fatal(err)
			}
			gov := registry.List()
			if len(gov) != len(recorded.Mints) {
				t.Fatalf("governed mints = %d, snapshot records %d", len(gov), len(recorded.Mints))
			}
			// Matched by address, not by position. The order the registry returns
			// is a display choice and the snapshot was written by hand; a
			// difference in ordering is not a governance change and should not
			// read as one.
			byAddress := make(map[string]tokens.Token, len(gov))
			for _, tok := range gov {
				byAddress[tok.Address] = tok
			}
			for _, want := range recorded.Mints {
				got, ok := byAddress[want.Address]
				if !ok {
					t.Errorf("%s is not governed on %s, the snapshot records it", want.Address, c)
					continue
				}
				if got.Symbol != want.Symbol {
					t.Errorf("mint %s symbol = %s, snapshot says %s", got.Address, got.Symbol, want.Symbol)
				}
				if got.Decimals != want.Decimals {
					t.Errorf("mint %s decimals = %d, snapshot says %d", got.Address, got.Decimals, want.Decimals)
				}
				if got.MintAuthority != want.MintAuthority {
					t.Errorf("mint %s mint authority = %s, snapshot says %s",
						got.Address, got.MintAuthority, want.MintAuthority)
				}
				if got.FreezeAuthority != want.FreezeAuthority {
					t.Errorf("mint %s freeze authority = %s, snapshot says %s",
						got.Address, got.FreezeAuthority, want.FreezeAuthority)
				}
			}
		})
	}
}

// The lookalike address from the Phase 2 defect is recorded in the design spec
// as the documented failure and must appear nowhere a program can read.
func TestTheLookalikeEURCAddressAppearsInNoGovernedSet(t *testing.T) {
	const lookalike = "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXcyK85CNzz7iwQc"
	for _, c := range []cluster.Cluster{cluster.MainnetBeta, cluster.Devnet, cluster.Localnet} {
		registry, err := tokens.ForCluster(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range registry.List() {
			if tok.Address == lookalike {
				t.Errorf("%s governs the non-existent lookalike EURC address", c)
			}
		}
	}
}
