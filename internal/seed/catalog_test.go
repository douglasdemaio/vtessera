package seed

import "testing"

// A seeded listing for code:explain would shadow the standing seller
// (vtessera-seller), which advertises the same capability with the only endpoint
// a buyer can actually finish against. The built-in catalog must not list it.
func TestDefaultCatalogLeavesCodeExplainToTheStandingSeller(t *testing.T) {
	for _, l := range DefaultCatalog() {
		for _, c := range l.Capabilities {
			if c == "code:explain" {
				t.Fatalf("default catalog lists %q, which the standing seller publishes", c)
			}
		}
	}
}
