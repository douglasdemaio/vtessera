package cluster

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAcceptsTheSupportedClusters(t *testing.T) {
	for name, want := range map[string]Cluster{
		"mainnet-beta": MainnetBeta,
		"devnet":       Devnet,
		"localnet":     Localnet,
		// A value typed into an environment file is rarely a bare token, and
		// trailing whitespace is a formatting mistake rather than an operator
		// trying to name a different chain.
		"  DEVNET  ": Devnet,
	} {
		got, err := Parse(name)
		if err != nil {
			t.Errorf("Parse(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("Parse(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestParseRefusesTestnetSeparately matters because "testnet" and "devnet" differ
// by one character in a string an operator types. Reporting it as an unknown
// cluster sends them off to check spellings; reporting it as unsupported tells
// them the service will not do it.
func TestParseRefusesTestnetSeparately(t *testing.T) {
	_, err := Parse("testnet")
	if !errors.Is(err, ErrClusterUnsupported) {
		t.Errorf("err = %v, want ErrClusterUnsupported", err)
	}
	if errors.Is(err, ErrUnknownCluster) {
		t.Error("testnet must not be reported as an unknown cluster")
	}
	if !strings.Contains(err.Error(), "testnet") {
		t.Errorf("error should name the cluster: %v", err)
	}
}

func TestParseRefusesAnEmptyValue(t *testing.T) {
	// An unset VTESSERA_CLUSTER is a configuration error, not a default. Picking
	// one silently is how a deployment ends up settling on the wrong chain.
	_, err := Parse("")
	if !errors.Is(err, ErrUnknownCluster) {
		t.Errorf("err = %v, want ErrUnknownCluster", err)
	}
	if !strings.Contains(err.Error(), "mainnet-beta") {
		t.Errorf("error should list the supported clusters: %v", err)
	}
}

func TestVerifyAcceptsThePinnedGenesis(t *testing.T) {
	for _, c := range []Cluster{MainnetBeta, Devnet} {
		if err := c.Verify(c.GenesisHash()); err != nil {
			t.Errorf("%s.Verify(pin): %v", c, err)
		}
	}
}

func TestVerifyRefusesAnotherClustersGenesis(t *testing.T) {
	// The devnet endpoint under a mainnet-beta declaration. Everything else in
	// the service would look healthy: mints exist, accounts are readable, the
	// fee wallet is funded. Only the genesis hash says no.
	err := MainnetBeta.Verify(Devnet.GenesisHash())
	if !errors.Is(err, ErrClusterMismatch) {
		t.Errorf("err = %v, want ErrClusterMismatch", err)
	}
	if !strings.Contains(err.Error(), MainnetBeta.GenesisHash()) {
		t.Errorf("error should name the expected pin: %v", err)
	}
}

// TestLocalnetAcceptsAnyGenesis is the deliberate exception: a disposable
// validator's hash is meaningless, so there is nothing to compare and the host
// rule carries the check instead.
func TestLocalnetAcceptsAnyGenesis(t *testing.T) {
	if err := Localnet.Verify("whatever-the-validator-happens-to-report"); err != nil {
		t.Errorf("Localnet.Verify: %v", err)
	}
	if Localnet.GenesisHash() != "" {
		t.Error("localnet must not carry a genesis pin")
	}
}

// TestGenesisPinsAreDistinct guards against a copy-paste that gives two clusters
// the same identity, which would make the cross-cluster check above pass for the
// wrong reason.
func TestGenesisPinsAreDistinct(t *testing.T) {
	seen := map[string]Cluster{}
	for c, pin := range genesisPins {
		if len(pin) != 44 {
			t.Errorf("%s pin is %d characters, want a 44-character base58 hash", c, len(pin))
		}
		if other, dup := seen[pin]; dup {
			t.Errorf("%s and %s share the genesis pin %s", c, other, pin)
		}
		seen[pin] = c
	}
}

func TestOnlyMainnetBetaIsProduction(t *testing.T) {
	if !MainnetBeta.IsProduction() {
		t.Error("mainnet-beta must be production")
	}
	for _, c := range []Cluster{Devnet, Localnet, Testnet} {
		if c.IsProduction() {
			t.Errorf("%s must not be production", c)
		}
	}
}

func TestOnlyLocalnetTakesOperatorMints(t *testing.T) {
	// On a public cluster an extra mint would mean this project governing a token
	// it does not, and preflight would be asked to bless it.
	if !Localnet.AllowsExtraMints() {
		t.Error("localnet must accept operator mints")
	}
	for _, c := range []Cluster{MainnetBeta, Devnet, Testnet} {
		if c.AllowsExtraMints() {
			t.Errorf("%s must not accept operator mints", c)
		}
	}
}

func TestLocalnetEndpointMustBeLoopback(t *testing.T) {
	for _, endpoint := range []string{
		"http://127.0.0.1:8899",
		"http://127.0.0.1:1",
		"https://localhost:8899",
		"http://[::1]:8899",
	} {
		if err := Localnet.CheckEndpoint(endpoint, nil); err != nil {
			t.Errorf("CheckEndpoint(%q): %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"http://203.0.113.9:8899",
		"http://192.168.1.10:8899",
		"https://api.mainnet-beta.solana.com",
		"ftp://127.0.0.1",
		"not a url",
		"",
	} {
		if err := Localnet.CheckEndpoint(endpoint, nil); !errors.Is(err, ErrEndpointNotAllowed) {
			t.Errorf("CheckEndpoint(%q) err = %v, want ErrEndpointNotAllowed", endpoint, err)
		}
	}
}

func TestLocalnetAcceptsAnAllowListedHost(t *testing.T) {
	// Docker networks are the ordinary reason a validator is not on loopback:
	// the service runs in a sibling container. The operator names the host, and
	// that is an explicit, auditable decision rather than a wildcard.
	const host = "validator.internal"
	err := Localnet.CheckEndpoint("http://"+host+":8899", []string{"validator.internal"})
	if err != nil {
		t.Errorf("CheckEndpoint: %v", err)
	}
	// The exemption is for the named host only.
	if err := Localnet.CheckEndpoint("http://"+host+":8899", []string{"other.internal"}); !errors.Is(err, ErrEndpointNotAllowed) {
		t.Errorf("err = %v, want ErrEndpointNotAllowed", err)
	}
}

func TestPublicClustersImposeNoHostRule(t *testing.T) {
	// A self-operated node is a configuration choice, not a code change, so
	// mainnet-beta and devnet accept any host that reports the right genesis.
	for _, endpoint := range []string{
		"https://api.mainnet-beta.solana.com",
		"http://10.0.0.4:8899",
		"not even a url",
	} {
		for _, c := range []Cluster{MainnetBeta, Devnet} {
			if err := c.CheckEndpoint(endpoint, nil); err != nil {
				t.Errorf("%s.CheckEndpoint(%q): %v", c, endpoint, err)
			}
		}
	}
}
