package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. 64 MiB / 3 passes / 4 lanes is the OWASP-recommended
// second option and takes roughly 50-100 ms on a modern core — slow enough to
// make guessing expensive, fast enough that a login does not feel stalled.
// The values are recorded in every hash string, so raising them later still
// verifies old hashes; they are re-hashed on next successful login.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	saltLen             = 16
)

// MinPasswordLength is deliberately the only password rule. Composition rules
// ("one uppercase, one symbol") push people towards predictable mutations;
// length is what actually costs an attacker.
const MinPasswordLength = 10

var (
	ErrMismatch    = errors.New("credentials do not match")
	ErrHashInvalid = errors.New("stored hash is malformed")
)

// peppered pre-mixes the secret with the server pepper via HMAC before Argon2
// sees it. Doing it this way (rather than concatenation) keeps the pepper a
// fixed-size key and sidesteps length-extension and ambiguity concerns, and
// it means an attacker holding only the database cannot even begin.
func peppered(pepper Pepper, secret string) []byte {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(secret))
	return mac.Sum(nil)
}

// Hash produces a PHC-format Argon2id string:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<b64 salt>$<b64 hash>
//
// Storing the parameters inline is what makes them changeable later without
// stranding existing accounts.
func Hash(pepper Pepper, secret string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey(peppered(pepper, secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify checks secret against an encoded hash in constant time. It returns
// ErrMismatch for a wrong secret and ErrHashInvalid for a corrupt record —
// callers must report both to the user identically.
func Verify(pepper Pepper, encoded, secret string) error {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return err
	}
	got := argon2.IDKey(peppered(pepper, secret), salt, params.time, params.memory, params.threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash reports whether a stored hash used weaker parameters than the
// current ones, so a successful login can quietly upgrade it.
func NeedsRehash(encoded string) bool {
	p, _, _, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return p.memory < argonMemory || p.time < argonTime || p.threads < argonThreads
}

type argonParams struct {
	memory, time uint32
	threads      uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	var p argonParams
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, ErrHashInvalid
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, ErrHashInvalid
	}
	var mem, t uint64
	var threads uint64
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return p, nil, nil, ErrHashInvalid
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return p, nil, nil, ErrHashInvalid
		}
		switch k {
		case "m":
			mem = n
		case "t":
			t = n
		case "p":
			threads = n
		default:
			return p, nil, nil, ErrHashInvalid
		}
	}
	if mem == 0 || t == 0 || threads == 0 || threads > 255 {
		return p, nil, nil, ErrHashInvalid
	}
	p = argonParams{memory: uint32(mem), time: uint32(t), threads: uint8(threads)}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return p, nil, nil, ErrHashInvalid
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return p, nil, nil, ErrHashInvalid
	}
	return p, salt, key, nil
}

// dummyHash is verified against when a login names a user that does not
// exist, so a missing account costs the same wall-clock time as a wrong
// password and cannot be distinguished by timing. It is built lazily, under a
// mutex: two simultaneous logins for unknown users are entirely ordinary, and
// an unsynchronised package-level string would be a genuine data race. The
// mutex is not sync.Once because a failed first build must be retried rather
// than latched — a permanently empty dummyHash would silently reintroduce the
// timing signal this exists to remove.
var (
	dummyMu   sync.Mutex
	dummyHash string
)

// FakeVerify burns the same work as a real verification. It exists purely to
// flatten the timing signal on unknown usernames.
func FakeVerify(pepper Pepper, secret string) {
	dummyMu.Lock()
	if dummyHash == "" {
		h, err := Hash(pepper, "this password is never valid")
		if err != nil {
			dummyMu.Unlock()
			return
		}
		dummyHash = h
	}
	h := dummyHash
	dummyMu.Unlock()
	// Verify outside the lock: the Argon2 work is the point, and serialising
	// it would turn the timing defence into a throughput bottleneck.
	_ = Verify(pepper, h, secret)
}

// ValidatePassword enforces the one rule worth enforcing.
func ValidatePassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	// Argon2 is fine with long inputs, but an unbounded password is a cheap
	// way to make the server do arbitrary work.
	if len(pw) > 1024 {
		return errors.New("password must be at most 1024 bytes")
	}
	return nil
}
