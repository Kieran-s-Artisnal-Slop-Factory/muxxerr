// Package auth handles credential hashing, passphrase generation and session
// tokens.
//
// Threat model this is built against: someone walks off with the SQLite file.
// Passwords and passphrases are hashed with Argon2id — memory-hard, so GPU
// and ASIC guessing is expensive — with a unique random salt per record, and
// an additional server-wide *pepper* mixed in. The pepper lives outside the
// database (data/pepper.key, or the MUX_PEPPER environment variable), so the
// stolen file alone cannot be attacked offline at all: without the pepper
// every guess is wrong.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PepperEnv is checked before the on-disk key file. Preferring the
// environment lets a deployment keep the pepper out of the data volume
// entirely, which is the point of having one.
const PepperEnv = "MUX_PEPPER"

// pepperBytes is 32 bytes of randomness, plenty for an HMAC-style key.
const pepperBytes = 32

// Pepper is the server-wide secret mixed into every credential hash.
type Pepper []byte

// LoadOrCreatePepper resolves the pepper from MUX_PEPPER, else from
// <dir>/pepper.key, generating and persisting one on first run.
//
// Losing the pepper invalidates every stored password and passphrase — there
// is no recovery, only an admin resetting each account. Back it up with (but
// stored separately from) the database.
func LoadOrCreatePepper(dir string) (Pepper, error) {
	if v := strings.TrimSpace(os.Getenv(PepperEnv)); v != "" {
		p, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("%s must be base64: %w", PepperEnv, err)
		}
		if len(p) < 16 {
			return nil, fmt.Errorf("%s must decode to at least 16 bytes, got %d", PepperEnv, len(p))
		}
		return p, nil
	}

	path := filepath.Join(dir, "pepper.key")
	raw, err := os.ReadFile(path)
	if err == nil {
		p, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if derr != nil {
			return nil, fmt.Errorf("%s is corrupt: %w", path, derr)
		}
		if len(p) < 16 {
			return nil, fmt.Errorf("%s is too short (%d bytes)", path, len(p))
		}
		return p, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	p := make([]byte, pepperBytes)
	if _, err := rand.Read(p); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// 0600: the pepper is as sensitive as the database it protects.
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(p)), 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return p, nil
}
