package ledger

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/douglasdemaio/vtessera/internal/attest"
	"github.com/mr-tron/base58"
)

const chainDomain = "vtessera/ledger/v1"

// keyringVersion is the on-disk version of the keyring. It is written so a future
// change to the file can be told apart from this one rather than guessed at.
const keyringVersion = 1

// Signer is the marketplace's signing identity: exactly one private key that
// signs everything, plus the public keys of keys that have been retired and are
// kept only so signatures they made still verify.
//
// Retired entries are public keys and nothing else. A retired private key would
// be a secret kept past its rotation for no reason, so rotation overwrites it and
// the file never holds more than one private key.
type Signer struct {
	current       ed25519.PrivateKey
	currentBase58 string
	retiredBase58 []string
	retiredPublic []ed25519.PublicKey
}

// keyringFile is the on-disk shape. Current is the base64 private key, the same
// encoding the legacy single-key file used, so migrating is a change of wrapper
// rather than of secret.
type keyringFile struct {
	Version int      `json:"version"`
	Current string   `json:"current"`
	Retired []string `json:"retired,omitempty"`
}

// NewSigner wraps an Ed25519 private key with no retired keys.
func NewSigner(private ed25519.PrivateKey) (*Signer, error) {
	return NewSignerWithRetired(private, nil)
}

// NewSignerWithRetired wraps a private key and the public keys it succeeds.
//
// Retired keys are validated here rather than trusted: a malformed one would
// otherwise be accepted at boot and only surface as a signature that mysteriously
// fails to verify against a key the service claims to trust.
func NewSignerWithRetired(private ed25519.PrivateKey, retired []string) (*Signer, error) {
	if len(private) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing key must be %d bytes, got %d", ed25519.PrivateKeySize, len(private))
	}
	current := private.Public().(ed25519.PublicKey)
	currentBase58 := base58.Encode(current)
	s := &Signer{current: private, currentBase58: currentBase58}
	seen := map[string]bool{currentBase58: true}
	for _, id := range retired {
		if id == "" {
			return nil, fmt.Errorf("retired signing key id is empty")
		}
		if seen[id] {
			return nil, fmt.Errorf("signing key %s is listed twice, or is the current key", id)
		}
		raw, err := base58.Decode(id)
		if err != nil {
			return nil, fmt.Errorf("retired signing key %s is not base58: %w", id, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("retired signing key %s is %d bytes, want %d", id, len(raw), ed25519.PublicKeySize)
		}
		seen[id] = true
		s.retiredBase58 = append(s.retiredBase58, id)
		s.retiredPublic = append(s.retiredPublic, ed25519.PublicKey(raw))
	}
	return s, nil
}

// GenerateSigner makes a fresh key with no retired keys.
func GenerateSigner() (*Signer, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	return NewSigner(private)
}

// LoadOrCreateSigner reads the keyring at path, creating one on first boot.
//
// A file that does not begin with a JSON object is read as the legacy single-key
// file: a base64 private key, treated as the current key with nothing retired.
// That is what every deployment written before rotation has, and reading it here
// is what makes upgrading a deploy rather than a migration.
func LoadOrCreateSigner(path string) (*Signer, bool, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		signer, err := parseSignerFile(raw)
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
	if err := writeKeyring(path, signer); err != nil {
		return nil, false, err
	}
	return signer, true, nil
}

// RotateSigner retires the current key and generates a new one, in place. It
// returns the new key's base58 public key.
//
// The old private key is not written anywhere: the file is replaced atomically
// with one whose retired list contains the old *public* key, and the old secret
// exists only in this call's memory. It is an offline action because the running
// service holds the old key in memory until restarted, and rotating a key it
// keeps signing with would be worse than not rotating.
func RotateSigner(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read signing key %s: %w", path, err)
	}
	current, err := parseSignerFile(raw)
	if err != nil {
		return "", fmt.Errorf("load signing key %s: %w", path, err)
	}
	next, err := GenerateSigner()
	if err != nil {
		return "", err
	}
	next, err = NewSignerWithRetired(next.current, current.VerificationKeyIDs())
	if err != nil {
		return "", err
	}
	if err := writeKeyring(path, next); err != nil {
		return "", err
	}
	return next.currentBase58, nil
}

// parseSignerFile reads either keyring shape. A leading brace means the JSON
// keyring; anything else is the legacy base64 secret.
func parseSignerFile(raw []byte) (*Signer, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		private, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(raw)))
		if err != nil {
			return nil, fmt.Errorf("decode signing key: %w", err)
		}
		return NewSigner(private)
	}
	var file keyringFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode keyring: %w", err)
	}
	if file.Current == "" {
		return nil, fmt.Errorf("keyring has no current key")
	}
	private, err := base64.StdEncoding.DecodeString(file.Current)
	if err != nil {
		return nil, fmt.Errorf("decode current signing key: %w", err)
	}
	return NewSignerWithRetired(private, file.Retired)
}

// writeKeyring replaces the file atomically: a temp file in the same directory,
// then rename. A half-written keyring is a deployment that cannot start, and the
// secret is small enough that crash-safety here is cheap.
func writeKeyring(path string, s *Signer) error {
	file := keyringFile{
		Version: keyringVersion,
		Current: base64.StdEncoding.EncodeToString(s.current),
		Retired: append([]string(nil), s.retiredBase58...),
	}
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode keyring: %w", err)
	}
	encoded = append(encoded, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".signer.key.*")
	if err != nil {
		return fmt.Errorf("create keyring temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set keyring permissions: %w", err)
	}
	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		return fmt.Errorf("write keyring: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync keyring: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close keyring: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace keyring: %w", err)
	}
	if handle, err := os.Open(dir); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}

// CurrentPrivate returns the private key that signs. It is a method rather than a
// field read so the signing secret stays inside this package's control.
func (s *Signer) CurrentPrivate() ed25519.PrivateKey {
	return s.current
}

func (s *Signer) PublicKey() ed25519.PublicKey {
	return s.current.Public().(ed25519.PublicKey)
}

func (s *Signer) PublicKeyBase58() string {
	return s.currentBase58
}

// VerificationKeyIDs is every key a verifier may accept, the current one first.
func (s *Signer) VerificationKeyIDs() []string {
	out := make([]string, 0, 1+len(s.retiredBase58))
	out = append(out, s.currentBase58)
	return append(out, s.retiredBase58...)
}

// RetiredKeyIDs is every key that is verify-only, oldest first.
func (s *Signer) RetiredKeyIDs() []string {
	return append([]string(nil), s.retiredBase58...)
}

// PublicKeys is every trusted public key, the current one first.
func (s *Signer) PublicKeys() []ed25519.PublicKey {
	out := make([]ed25519.PublicKey, 0, 1+len(s.retiredPublic))
	out = append(out, s.PublicKey())
	return append(out, s.retiredPublic...)
}

// PublicKeyForID resolves a key id to a trusted public key.
func (s *Signer) PublicKeyForID(id string) (ed25519.PublicKey, bool) {
	if id == s.currentBase58 {
		return s.PublicKey(), true
	}
	for i, retired := range s.retiredBase58 {
		if id == retired {
			return s.retiredPublic[i], true
		}
	}
	return nil, false
}

// KeyID is the base58 name a public key is known by, the same string a signature's
// KeyID carries and /healthz publishes.
func (s *Signer) KeyID(key ed25519.PublicKey) string {
	return base58.Encode(key)
}

func EntryHash(seq int64, prevHash, payloadHash string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s", chainDomain, seq, prevHash, payloadHash)))
	return hex.EncodeToString(sum[:])
}

func PayloadHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// AttestationSigner returns this ledger's key as an attest.SigningKey.
//
// It is the same key that issues receipts, deliberately: a marketplace with one
// signing identity has one thing to protect and one thing to rotate, and the
// alternative — a second key for listings — would add an identity without
// removing the one that matters. The verification key in /healthz is therefore
// the key a reader needs to check a card this marketplace published, which is
// also the key a buyer needs to check a receipt. Blasting that radius is a real
// cost and is recorded in the threat model rather than hidden here.
func (s *Signer) AttestationSigner() (*attest.SigningKey, error) {
	return attest.NewSigningKey(s.current)
}
