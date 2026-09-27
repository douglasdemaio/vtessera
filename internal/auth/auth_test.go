package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/store"
	"github.com/mr-tron/base58"
)

func newService(t *testing.T) (*Service, ed25519.PrivateKey, string) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "vtessera.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(st, secret)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return svc, priv, base58.Encode(pub)
}

func sign(t *testing.T, priv ed25519.PrivateKey, challenge domain.Challenge) string {
	t.Helper()
	sig := ed25519.Sign(priv, Message(challenge.ID, challenge.AgentID, challenge.Nonce))
	return base64.StdEncoding.EncodeToString(sig)
}

func TestChallengeResponseIssuesSession(t *testing.T) {
	ctx := context.Background()
	svc, priv, agentID := newService(t)

	challenge, err := svc.IssueChallenge(ctx, agentID)
	if err != nil {
		t.Fatalf("issue challenge: %v", err)
	}
	if challenge.AgentID != agentID || challenge.Nonce == "" {
		t.Errorf("challenge = %+v, want agent %s with a nonce", challenge, agentID)
	}
	if !challenge.ExpiresAt.After(time.Now()) {
		t.Error("challenge must not be expired on issue")
	}

	session, err := svc.Redeem(ctx, challenge.ID, sign(t, priv, challenge))
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if session.AgentID != agentID {
		t.Errorf("session agent = %s, want %s", session.AgentID, agentID)
	}
	got, err := svc.Authenticate("Bearer " + session.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got != agentID {
		t.Errorf("authenticated agent = %s, want %s", got, agentID)
	}
}

func TestChallengeIsSingleUse(t *testing.T) {
	ctx := context.Background()
	svc, priv, agentID := newService(t)
	challenge, err := svc.IssueChallenge(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	signature := sign(t, priv, challenge)
	if _, err := svc.Redeem(ctx, challenge.ID, signature); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Redeem(ctx, challenge.ID, signature); !errors.Is(err, domain.ErrStale) {
		t.Errorf("replay = %v, want ErrStale", err)
	}
}

func TestRedeemRejectsWrongSigner(t *testing.T) {
	ctx := context.Background()
	svc, _, agentID := newService(t)
	challenge, err := svc.IssueChallenge(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Redeem(ctx, challenge.ID, sign(t, otherPriv, challenge)); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("foreign key = %v, want ErrInvalidSignature", err)
	}
}

func TestRedeemRejectsGarbageSignature(t *testing.T) {
	ctx := context.Background()
	svc, _, agentID := newService(t)
	challenge, err := svc.IssueChallenge(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Redeem(ctx, challenge.ID, "!!!not base64!!!"); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("garbage signature = %v, want ErrInvalidSignature", err)
	}
}

func TestIssueChallengeRejectsBadPublicKey(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newService(t)
	for _, bad := range []string{"", "not-base58-!!", base58.Encode(make([]byte, 31))} {
		if _, err := svc.IssueChallenge(ctx, bad); err == nil {
			t.Errorf("IssueChallenge(%q) = nil error, want rejection", bad)
		}
	}
}

func TestSessionRejectsForeignAndExpiredTokens(t *testing.T) {
	ctx := context.Background()
	svc, priv, agentID := newService(t)
	challenge, err := svc.IssueChallenge(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := svc.Redeem(ctx, challenge.ID, sign(t, priv, challenge))
	if err != nil {
		t.Fatal(err)
	}

	other, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	otherSvc, err := New(nil, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherSvc.Authenticate("Bearer " + session.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("foreign secret = %v, want ErrSessionInvalid", err)
	}
	for _, header := range []string{"", "Bearer", "Bearer ", "Basic " + session.Token, session.Token} {
		if _, err := svc.Authenticate(header); !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("header %q = %v, want ErrSessionInvalid", header, err)
		}
	}
}

func TestSessionExpires(t *testing.T) {
	ctx := context.Background()
	svc, priv, agentID := newService(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	challenge, err := svc.IssueChallenge(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := svc.Redeem(ctx, challenge.ID, sign(t, priv, challenge))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate("Bearer " + session.Token); err != nil {
		t.Fatalf("fresh session must authenticate: %v", err)
	}
	svc.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := svc.Authenticate("Bearer " + session.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("expired session = %v, want ErrSessionInvalid", err)
	}
}

func TestNewRejectsShortSecret(t *testing.T) {
	if _, err := New(nil, []byte("too short")); !errors.Is(err, ErrSecretTooShort) {
		t.Errorf("short secret = %v, want ErrSecretTooShort", err)
	}
}

func TestChallengeExpiry(t *testing.T) {
	ctx := context.Background()
	svc, priv, agentID := newService(t)
	svc.now = func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }
	challenge, err := svc.IssueChallenge(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	signature := sign(t, priv, challenge)
	svc.now = func() time.Time { return challenge.ExpiresAt.Add(time.Second) }
	if _, err := svc.Redeem(ctx, challenge.ID, signature); !errors.Is(err, domain.ErrStale) {
		t.Errorf("expired challenge = %v, want ErrStale", err)
	}
}
