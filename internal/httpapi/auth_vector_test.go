package httpapi_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/auth"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/mr-tron/base58"
)

// The published handshake vector at docs/test-vectors/handshake.json promises
// that re-signing its documented bytes reproduces its documented signature and
// that the server accepts that signature. These tests hold the promise to the
// code, so the sample cannot drift away from real behaviour unnoticed.
type handshakeVector struct {
	PrivateKeySeedHex string `json:"privateKeySeedHex"`
	PublicKeyHex      string `json:"publicKeyHex"`
	Algorithm         string `json:"algorithm"`
	Warning           string `json:"warning"`
	AgentID           string `json:"agentId"`
	ChallengeID       string `json:"challengeId"`
	Nonce             string `json:"nonce"`
	Message           string `json:"message"`
	MessageSHA256     string `json:"messageSha256"`
	Signature         string `json:"signature"`
}

func loadHandshakeVector(t *testing.T) handshakeVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "test-vectors", "handshake.json"))
	if err != nil {
		t.Fatalf("read published handshake vector: %v", err)
	}
	var vector handshakeVector
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("parse published handshake vector: %v", err)
	}
	return vector
}

// vectorChallenge installs the vector's fixed challenge ID and nonce as server
// state, with an expiry chosen at test time: the expiry is never part of the
// signed bytes, so it is the one field the vector deliberately does not pin.
func vectorChallenge(vector handshakeVector) domain.Challenge {
	return domain.Challenge{
		ID:        vector.ChallengeID,
		AgentID:   vector.AgentID,
		Nonce:     vector.Nonce,
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}
}

func postJSON(t *testing.T, url string, payload any) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode, mustRead(t, response.Body)
}

func TestPublishedHandshakeVector(t *testing.T) {
	vector := loadHandshakeVector(t)

	t.Run("the published key is marked as a throwaway", func(t *testing.T) {
		// The private half ships in the file, so a sample without this warning
		// hands impersonation to whoever reads it.
		if vector.Warning == "" {
			t.Fatal("the vector carries no warning that its key is public and must never be used for a real agent")
		}
		if vector.Algorithm != "Ed25519" {
			t.Fatalf("algorithm = %q, want Ed25519", vector.Algorithm)
		}
	})

	t.Run("the agent ID is the public key in base58", func(t *testing.T) {
		publicKey, err := hex.DecodeString(vector.PublicKeyHex)
		if err != nil {
			t.Fatalf("publicKeyHex: %v", err)
		}
		if got := base58.Encode(publicKey); got != vector.AgentID {
			t.Fatalf("agentId = %q, want base58 of the public key %q", got, vector.AgentID)
		}
		if _, err := domain.ParsePublicKey(vector.AgentID); err != nil {
			t.Fatalf("agentId is not a usable public key: %v", err)
		}
	})

	t.Run("the signed bytes are exactly what the server derives", func(t *testing.T) {
		want := auth.Message(vector.ChallengeID, vector.AgentID, vector.Nonce)
		if string(want) != vector.Message {
			t.Fatalf("message drifted from auth.Message:\n got %q\nwant %q", vector.Message, want)
		}
		sum := sha256.Sum256(want)
		if got := hex.EncodeToString(sum[:]); got != vector.MessageSHA256 {
			t.Fatalf("messageSha256 = %q, want %q", got, vector.MessageSHA256)
		}
	})

	t.Run("the signature reproduces from the published seed", func(t *testing.T) {
		seed, err := hex.DecodeString(vector.PrivateKeySeedHex)
		if err != nil {
			t.Fatalf("privateKeySeedHex: %v", err)
		}
		key := ed25519.NewKeyFromSeed(seed)
		publicKey, err := hex.DecodeString(vector.PublicKeyHex)
		if err != nil {
			t.Fatalf("publicKeyHex: %v", err)
		}
		if !bytes.Equal(key.Public().(ed25519.PublicKey), publicKey) {
			t.Fatal("the seed and the public key are not a pair")
		}
		signature, err := base64.StdEncoding.DecodeString(vector.Signature)
		if err != nil {
			t.Fatalf("signature: %v", err)
		}
		// Ed25519 is deterministic: an implementation that signs the documented
		// bytes with the documented seed must produce this exact signature, which
		// is what lets a developer check their code with no server in the loop.
		if got := ed25519.Sign(key, []byte(vector.Message)); !bytes.Equal(got, signature) {
			t.Fatal("signing the vector's message with the vector's seed does not reproduce the published signature")
		}
		if !ed25519.Verify(ed25519.PublicKey(publicKey), []byte(vector.Message), signature) {
			t.Fatal("the published signature does not verify under the published public key")
		}
	})

	t.Run("the server accepts the vector's signature", func(t *testing.T) {
		challenge := vectorChallenge(vector)
		server, _ := buildServer(t, serverBuild{challenge: &challenge})
		status, raw := postJSON(t, server.URL+"/v1/auth/verify", map[string]any{
			"challengeId": vector.ChallengeID,
			"signature":   vector.Signature,
		})
		if status != http.StatusOK {
			t.Fatalf("verify answered %d: %s", status, raw)
		}
		var session struct {
			AgentID string `json:"agentId"`
			Token   string `json:"token"`
		}
		if err := json.Unmarshal(raw, &session); err != nil {
			t.Fatal(err)
		}
		if session.AgentID != vector.AgentID {
			t.Fatalf("session agentId = %q, want the vector's %q", session.AgentID, vector.AgentID)
		}
		// The token is real: a protected route authenticates it (404, no such
		// trade) rather than rejecting it (401).
		request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/tesseras/"+vector.AgentID, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("authorization", "Bearer "+session.Token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized {
			t.Fatalf("the token the vector's signature earned was refused: %d", response.StatusCode)
		}
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("a protected route answered %d for the vector's token, want 404 (authenticated, no such trade)", response.StatusCode)
		}
	})

	t.Run("a tampered signature is refused", func(t *testing.T) {
		challenge := vectorChallenge(vector)
		server, _ := buildServer(t, serverBuild{challenge: &challenge})
		signature, err := base64.StdEncoding.DecodeString(vector.Signature)
		if err != nil {
			t.Fatal(err)
		}
		signature[0] ^= 0x01
		status, raw := postJSON(t, server.URL+"/v1/auth/verify", map[string]any{
			"challengeId": vector.ChallengeID,
			"signature":   base64.StdEncoding.EncodeToString(signature),
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("a tampered signature answered %d, want 401: %s", status, raw)
		}
	})
}
