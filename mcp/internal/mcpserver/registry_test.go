package mcpserver

// The registry listing is validated here rather than by eye, because a listing is
// published rather than reviewed: a mistake in it reaches clients as an install
// that fails, and a hand-checked JSON file is exactly where that mistake hides.
//
// The published schema is checked in beside the listing so this test needs no
// network. It was fetched from the $schema URL the listing itself declares.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	listingPath = "../../registry/server.json"
	schemaPath  = "../../registry/server.schema.json"
)

func readJSON(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The draft has to satisfy the registry's own schema, or the registry rejects it.
func TestTheListingMatchesThePublishedSchema(t *testing.T) {
	schema := &jsonschema.Schema{}
	if err := json.Unmarshal(readJSON(t, schemaPath), schema); err != nil {
		t.Fatalf("the checked-in schema is not readable: %v", err)
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{})
	if err != nil {
		t.Fatalf("the checked-in schema does not resolve: %v", err)
	}
	var draft any
	if err := json.Unmarshal(readJSON(t, listingPath), &draft); err != nil {
		t.Fatalf("the listing is not valid JSON: %v", err)
	}
	if err := resolved.Validate(draft); err != nil {
		t.Fatalf("the listing does not match the registry schema: %v", err)
	}
}

// The listing and the binary have to agree on a version, because the version a
// client is told at initialize comes from the code and the version an installer
// reads comes from this file.
func TestTheListingNamesTheVersionTheBinaryReports(t *testing.T) {
	var draft struct {
		Version  string `json:"version"`
		Packages []struct {
			RegistryType string `json:"registryType"`
			Identifier   string `json:"identifier"`
			Version      string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(readJSON(t, listingPath), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.Version != Version {
		t.Errorf("listing version %q, binary reports %q", draft.Version, Version)
	}
	for i, p := range draft.Packages {
		// The registry rejects a `version` field on an OCI package: the tag in
		// the identifier is the version, and a separate field is an error, not a
		// duplicate. So for OCI the claim to check is the tag.
		reported := p.Version
		if p.RegistryType == "oci" {
			reported = p.Identifier[strings.LastIndex(p.Identifier, ":")+1:]
		}
		if reported != Version {
			t.Errorf("packages[%d] version %q, binary reports %q", i, reported, Version)
		}
	}
}

// The registry rejects a version range, and a range here would read as "whatever
// is newest", which is not a thing a client can install against a marketplace
// whose endpoints have changed underneath it.
func TestTheListingPinsExactVersions(t *testing.T) {
	raw := string(readJSON(t, listingPath))
	for _, forbidden := range []string{"latest", "^", "~", ">=", "1.x", "*"} {
		if strings.Contains(raw, `"version": "`+forbidden) {
			t.Errorf("the listing pins %q, which the registry rejects", forbidden)
		}
	}
}

// A draft that claims to be ready while still carrying placeholders would be
// submitted and fail to install. The two are checked against each other so the
// flag cannot lie.
func TestADraftCannotClaimToBeReadyWhileItCarriesPlaceholders(t *testing.T) {
	raw := string(readJSON(t, listingPath))
	var draft struct {
		Meta struct {
			Draft struct {
				ReadyToSubmit bool `json:"readyToSubmit"`
			} `json:"readyToSubmit"`
		} `json:"vtessera/draft"`
	}
	if err := json.Unmarshal(readJSON(t, listingPath), &draft); err != nil {
		t.Fatal(err)
	}
	placeholders := strings.Count(raw, "TODO(")
	if draft.Meta.Draft.ReadyToSubmit && placeholders > 0 {
		t.Fatalf("the listing is marked ready to submit and still carries %d placeholders", placeholders)
	}
	t.Logf("%d placeholder(s) outstanding; readyToSubmit=%t", placeholders, draft.Meta.Draft.ReadyToSubmit)
}

// Every tool a client can call is discoverable from the listing's description, so
// this file has to say what the server is for rather than only that it exists.
func TestTheListingDescribesWhatTheServerDoes(t *testing.T) {
	var draft struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(readJSON(t, listingPath), &draft); err != nil {
		t.Fatal(err)
	}
	if len(draft.Description) < 40 {
		t.Errorf("description is %d characters, which says nothing useful: %q", len(draft.Description), draft.Description)
	}
	for _, capability := range []string{"offers", "cards", "route", "attest"} {
		if !strings.Contains(draft.Description, capability) {
			t.Errorf("the description does not mention %q: %s", capability, draft.Description)
		}
	}
	if len(draft.Description) > 100 {
		t.Errorf("description is %d characters, over the registry's 100 limit", len(draft.Description))
	}
}

// The listing must not describe a server that can move money. It cannot, because
// this server has no session, and a listing claiming otherwise would be the worst
// kind of wrong.
func TestTheListingDoesNotClaimToTrade(t *testing.T) {
	raw := strings.ToLower(string(readJSON(t, listingPath)))
	for _, claim := range []string{"buy", "sell", "transfer funds", "trade on behalf", "execute trades"} {
		if strings.Contains(raw, claim) {
			t.Errorf("the listing claims the server can %q, which this read-only server cannot", claim)
		}
	}
}

// The tools are read-only, and every one of them is annotated as such so a client
// may call it without asking a person.
func TestEveryToolIsMarkedReadOnly(t *testing.T) {
	s := newTestServer(t, healthOnly())
	tools := s.listTools()
	if len(tools) == 0 {
		t.Fatal("no tools are advertised")
	}
	for _, tool := range tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
	}
	fmt.Fprintf(os.Stderr, "%d tools, all read-only\n", len(tools))
}
