package mcpserver

// The Server Card is the pre-connection discovery document, published rather than
// reviewed like the registry listing it sits beside. It is validated against the
// checked-in schema here for the same reason the listing is: a mistake in it
// reaches a discovering client, and a hand-checked JSON document is exactly where
// that mistake hides.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/mcp/internal/vtessera"
	"github.com/google/jsonschema-go/jsonschema"
)

const cardSchemaPath = "../../registry/server-card.schema.json"

// cardForTest builds the discovery document the hosted server would serve.
func cardForTest(t *testing.T) ServerCard {
	t.Helper()
	client, err := vtessera.NewClient("http://127.0.0.1:0", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return New(client, "https://vtessera.fly.dev").Card("https://vtessera-mcp.fly.dev/mcp")
}

// The card has to satisfy the extension's schema, or a client that validates it
// before connecting rejects the only advertisement this server publishes.
func TestTheServerCardMatchesThePublishedSchema(t *testing.T) {
	schema := &jsonschema.Schema{}
	if err := json.Unmarshal(readJSON(t, cardSchemaPath), schema); err != nil {
		t.Fatalf("the checked-in card schema is not readable: %v", err)
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{})
	if err != nil {
		t.Fatalf("the checked-in card schema does not resolve: %v", err)
	}
	encoded, err := json.Marshal(cardForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	var draft any
	if err := json.Unmarshal(encoded, &draft); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(draft); err != nil {
		t.Fatalf("the server card does not match the extension schema: %v", err)
	}
}

// The card and the registry listing have to name the same server, because a
// discovering client that reads one and installs the other would be sent to an
// identity that is not there.
func TestTheServerCardNamesTheSameServerAsTheListing(t *testing.T) {
	var listing struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		WebsiteURL string `json:"websiteUrl"`
		Repository struct {
			URL       string `json:"url"`
			Subfolder string `json:"subfolder"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(readJSON(t, listingPath), &listing); err != nil {
		t.Fatal(err)
	}
	card := cardForTest(t)
	if card.Name != listing.Name {
		t.Errorf("card names %q, the listing names %q", card.Name, listing.Name)
	}
	if card.Version != listing.Version {
		t.Errorf("card version %q, the listing version %q", card.Version, listing.Version)
	}
	if card.WebsiteURL != listing.WebsiteURL {
		t.Errorf("card website %q, the listing website %q", card.WebsiteURL, listing.WebsiteURL)
	}
	if card.Repository == nil {
		t.Fatal("the card names no repository, so a reviewer has nowhere to look")
	}
	if card.Repository.URL != listing.Repository.URL {
		t.Errorf("card repository %q, the listing repository %q", card.Repository.URL, listing.Repository.URL)
	}
	if card.Repository.Subfolder != listing.Repository.Subfolder {
		t.Errorf("card repository subfolder %q, the listing subfolder %q", card.Repository.Subfolder, listing.Repository.Subfolder)
	}
}

// The card advertises one streamable-http endpoint, and it must be the endpoint
// the registry listing sends clients to.
func TestTheServerCardAdvertisesTheListedEndpoint(t *testing.T) {
	var listing struct {
		Remotes []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"remotes"`
	}
	if err := json.Unmarshal(readJSON(t, listingPath), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Remotes) == 0 {
		t.Fatal("the listing advertises no remote endpoint to compare against")
	}
	got := cardForTest(t).Remotes
	if len(got) != 1 {
		t.Fatalf("the card advertises %d endpoints, want exactly one", len(got))
	}
	if got[0].Type != listing.Remotes[0].Type || got[0].URL != listing.Remotes[0].URL {
		t.Errorf("card advertises %s %s, the listing advertises %s %s",
			got[0].Type, got[0].URL, listing.Remotes[0].Type, listing.Remotes[0].URL)
	}
}
