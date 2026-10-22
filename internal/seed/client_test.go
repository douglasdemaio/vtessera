package seed

import "testing"

// refusal is what turns a 4xx/5xx body into the error every Client call
// returns. A marketplace refusal always carries a code ("MINT_UNPRICED" and
// so on); a body from something that is not this service's API - a proxy's
// error page, say - never does, and the raw body is the only description
// left to show.
func TestRefusalParsesTheMarketplaceErrorAndCode(t *testing.T) {
	err := refusal(409, []byte(`{"error":"mint has no declared rate","code":"MINT_UNPRICED"}`))
	refused, ok := err.(*Refused)
	if !ok {
		t.Fatalf("refusal returned %T, want *Refused", err)
	}
	if refused.Status != 409 || refused.Code != "MINT_UNPRICED" || refused.Message != "mint has no declared rate" {
		t.Errorf("refusal = %+v, want {409 MINT_UNPRICED mint has no declared rate}", refused)
	}
}

func TestRefusalFallsBackToTheRawBodyWhenItIsNotJSON(t *testing.T) {
	err := refusal(502, []byte("  <html>bad gateway</html>  "))
	refused := err.(*Refused)
	if refused.Code != "" {
		t.Errorf("code = %q, want empty for a body this service never wrote", refused.Code)
	}
	if refused.Message != "<html>bad gateway</html>" {
		t.Errorf("message = %q, want the raw body trimmed", refused.Message)
	}
}

// Error's two forms exist so a reader can tell the marketplace's own refusal
// (named by its code) from an upstream failure the marketplace never
// produced (no code, so the message stands on its own).
func TestRefusedErrorNamesTheCodeWhenPresent(t *testing.T) {
	err := &Refused{Status: 404, Code: "AGENT_NOT_FOUND", Message: "no such agent"}
	want := "marketplace refused (AGENT_NOT_FOUND): no such agent"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestRefusedErrorFallsBackToStatusWithoutACode(t *testing.T) {
	err := &Refused{Status: 503, Message: "service unavailable"}
	want := "marketplace refused with 503: service unavailable"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
