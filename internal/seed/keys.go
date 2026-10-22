package seed

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// keyringVersion is the on-disk version of the seed keyring. It is written so a
// future change to the file shape can refuse an old one rather than guess at it.
const keyringVersion = 1

// keyringFile is the on-disk shape. One file holds one identity per seeded
// agent, so a restart reuses the same agent ID instead of minting a new agent
// and orphaning every offer the old one published.
type keyringFile struct {
	Version int               `json:"version"`
	Keys    map[string]string `json:"keys"`
}

// Keyring owns the Ed25519 identities behind the seeded listings. A seeded agent
// holds no funds and shares no key with the marketplace, so losing this file is
// not losing value; it is losing the identity, and the honest recovery is to
// publish under a new one.
type Keyring struct {
	path string
	mu   sync.Mutex
	keys map[string]ed25519.PrivateKey
}

// OpenKeyring reads the keyring at path, or returns an empty one when the file
// does not exist yet. A malformed file is an error rather than an empty keyring:
// silently replacing it would republish every listing under new identities.
func OpenKeyring(path string) (*Keyring, error) {
	k := &Keyring{path: path, keys: map[string]ed25519.PrivateKey{}}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return k, nil
		}
		return nil, fmt.Errorf("read seed keyring %s: %w", path, err)
	}
	var file keyringFile
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("decode seed keyring %s: %w", path, err)
	}
	if file.Version != keyringVersion {
		return nil, fmt.Errorf("seed keyring %s has version %d, want %d", path, file.Version, keyringVersion)
	}
	for name, encoded := range file.Keys {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("seed keyring %s: key %q is not base64: %w", path, name, err)
		}
		if len(raw) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("seed keyring %s: key %q is %d bytes, want %d", path, name, len(raw), ed25519.PrivateKeySize)
		}
		k.keys[name] = ed25519.PrivateKey(raw)
	}
	return k, nil
}

// Key returns the identity for a listing, generating and persisting one the
// first time it is asked for.
func (k *Keyring) Key(name string) (ed25519.PrivateKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key, ok := k.keys[name]; ok {
		return key, nil
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate identity for %q: %w", name, err)
	}
	k.keys[name] = key
	if err := k.save(); err != nil {
		delete(k.keys, name)
		return nil, err
	}
	return key, nil
}

// save replaces the file atomically: a temp file in the same directory, then
// rename. It is written on first use of a new identity, so an interrupted write
// leaves either the old keyring or none, never a partial one.
func (k *Keyring) save() error {
	file := keyringFile{Version: keyringVersion, Keys: map[string]string{}}
	for name, key := range k.keys {
		file.Keys[name] = base64.StdEncoding.EncodeToString(key)
	}
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode seed keyring: %w", err)
	}
	encoded = append(encoded, '\n')
	dir := filepath.Dir(k.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create seed keyring directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".seed-keyring.*")
	if err != nil {
		return fmt.Errorf("create seed keyring temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set seed keyring permissions: %w", err)
	}
	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		return fmt.Errorf("write seed keyring: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync seed keyring: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close seed keyring: %w", err)
	}
	if err := os.Rename(tmpName, k.path); err != nil {
		return fmt.Errorf("replace seed keyring: %w", err)
	}
	return nil
}
