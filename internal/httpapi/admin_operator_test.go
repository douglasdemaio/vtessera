package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/httpapi"
)

const (
	aliceOperatorToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bobOperatorToken   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// operatorServer is a marketplace with two named operators, so a test can tell
// a credential apart from the name it carries.
func operatorServer(t *testing.T) (*httptest.Server, *agentClient) {
	t.Helper()
	server, _ := buildServer(t, serverBuild{adminOperators: []httpapi.Operator{
		{Name: "alice", Token: []byte(aliceOperatorToken)},
		{Name: "bob", Token: []byte(bobOperatorToken)},
	}})
	return server, newAgent(t, server)
}

// adminDoHeader is adminDo with one extra request header, for the test that a
// caller-supplied X-Operator is not what the audit row records.
func adminDoHeader(t *testing.T, server *httptest.Server, method, path, token, header, value string, payload map[string]any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if header != "" {
		req.Header.Set(header, value)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestAnOperatorActionIsAttributedToTheCredentialName(t *testing.T) {
	server, seller := operatorServer(t)
	seller.publishOffer("10.00", usdc)

	status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", aliceOperatorToken,
		map[string]any{"reason": "probe left behind"})
	if status != http.StatusOK {
		t.Fatalf("retire = %d %s, want 200", status, body)
	}
	status, body = adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/restore", bobOperatorToken, nil)
	if status != http.StatusOK {
		t.Fatalf("restore = %d %s, want 200", status, body)
	}

	// Two operators, one row: the withdrawal names the credential that made it,
	// and the restore names a different one. That is the point of naming them.
	status, body = adminDo(t, server, http.MethodGet,
		"/v1/admin/agents/"+seller.id+"/retirement", aliceOperatorToken, nil)
	if status != http.StatusOK {
		t.Fatalf("read retirement = %d %s, want 200", status, body)
	}
	var retirement domain.Retirement
	decodeInto(t, body, &retirement)
	if retirement.Actor != "alice" {
		t.Errorf("actor = %q, want the credential's name alice", retirement.Actor)
	}
	if retirement.RestoredBy != "bob" {
		t.Errorf("restoredBy = %q, want the credential's name bob", retirement.RestoredBy)
	}
}

func TestTheXOperatorHeaderCannotForgeTheAuditName(t *testing.T) {
	server, seller := operatorServer(t)

	// The header used to be believed. It is now ignored, so a holder of alice's
	// token cannot record a withdrawal as somebody else.
	status, body := adminDoHeader(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", aliceOperatorToken, "X-Operator", "mallory",
		map[string]any{"reason": "withdrawn"})
	if status != http.StatusOK {
		t.Fatalf("retire = %d %s, want 200", status, body)
	}
	status, body = adminDo(t, server, http.MethodGet,
		"/v1/admin/agents/"+seller.id+"/retirement", aliceOperatorToken, nil)
	if status != http.StatusOK {
		t.Fatalf("read retirement = %d %s, want 200", status, body)
	}
	var retirement domain.Retirement
	decodeInto(t, body, &retirement)
	if retirement.Actor != "alice" {
		t.Errorf("actor = %q, want alice, not the caller-typed header", retirement.Actor)
	}
}

func TestEveryNamedOperatorCredentialIsAccepted(t *testing.T) {
	// Two credentials live at once, which is what makes rotation possible: the
	// new one works before the old one is removed.
	server, seller := operatorServer(t)
	seller.publishOffer("10.00", usdc)
	if status, body := adminDo(t, server, http.MethodPost,
		"/v1/admin/agents/"+seller.id+"/retire", aliceOperatorToken,
		map[string]any{"reason": "withdrawn"}); status != http.StatusOK {
		t.Fatalf("retire with alice = %d %s, want 200", status, body)
	}

	if status, body := adminDo(t, server, http.MethodGet,
		"/v1/admin/agents/"+seller.id+"/retirement", aliceOperatorToken, nil); status != http.StatusOK {
		t.Errorf("alice's credential = %d %s, want 200", status, body)
	}
	if status, body := adminDo(t, server, http.MethodGet,
		"/v1/admin/agents/"+seller.id+"/retirement", bobOperatorToken, nil); status != http.StatusOK {
		t.Errorf("bob's credential = %d %s, want 200", status, body)
	}
}

func TestACredentialMatchingNoConfiguredOperatorIsRefused(t *testing.T) {
	server, seller := operatorServer(t)
	status, _ := adminDo(t, server, http.MethodGet,
		"/v1/admin/agents/"+seller.id+"/retirement", "not-a-configured-token", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("unknown credential = %d, want 401", status)
	}
}
