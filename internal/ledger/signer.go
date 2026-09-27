package ledger

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mr-tron/base58"
)

const chainDomain = "vtessera/ledger/v1"

type Signer struct {
	private ed25519.PrivateKey
}

func NewSigner(private ed25519.PrivateKey) (*Signer, error) {
	if len(private) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing key must be %d bytes, got %d", ed25519.PrivateKeySize, len(private))
	}
	return &Signer{private: private}, nil
}

func GenerateSigner() (*Signer, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	return NewSigner(private)
}

func LoadOrCreateSigner(path string) (*Signer, bool, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		private, decodeErr := base64.StdEncoding.DecodeString(string(raw))
		if decodeErr != nil {
			return nil, false, fmt.Errorf("decode signing key %s: %w", path, decodeErr)
		}
		signer, err := NewSigner(private)
		if err != nil {
			return nil, false, fmt.Errorf("load signing key %s: %w", path, err)
		}
		return signer, false, nil
	case !os.IsNotExist(err):
		return nil, false, fmt.Errorf("read signing key %s: %w", path, err)
	}
	signer, err := GenerateSigner()
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create key directory: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(signer.private)
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, false, fmt.Errorf("write signing key %s: %w", path, err)
	}
	return signer, true, nil
}

func (s *Signer) PublicKey() ed25519.PublicKey {
	return s.private.Public().(ed25519.PublicKey)
}

func (s *Signer) PublicKeyBase58() string {
	return base58.Encode(s.PublicKey())
}

func EntryHash(seq int64, prevHash, payloadHash string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s", chainDomain, seq, prevHash, payloadHash)))
	return hex.EncodeToString(sum[:])
}

func PayloadHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
