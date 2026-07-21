// Tests for the credential layer. The bias of these tests is towards the
// properties that would be silently, catastrophically wrong rather than
// loudly broken: that the pepper actually participates in the hash, that a
// corrupt stored hash is reported as corrupt rather than as a mismatch (a
// caller that conflated the two would happily let a truncated record through
// some future "if err == ErrMismatch" branch), and that the wordlist is still
// the exact size the passphrase entropy claim depends on.
//
// Argon2 at 64 MiB costs 50-100 ms per call, so hashes are computed sparingly
// and shared where a test only needs "some valid hash". Anything that wants
// many iterations exercises the cheap surface (decodeHash, NormalisePassphrase,
// NewSessionToken) instead.
package auth

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testPepper is a fixed value so failures are reproducible; the real one is
// random, but nothing here depends on that.
var testPepper = Pepper([]byte("0123456789abcdef0123456789abcdef"))

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := Hash(testPepper, "correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := Verify(testPepper, h, "correct horse battery staple"); err != nil {
		t.Fatalf("Verify with the right secret: %v", err)
	}
	if err := Verify(testPepper, h, "correct horse battery stapl"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("Verify with a wrong secret = %v, want ErrMismatch", err)
	}
	// An empty secret must not be a wildcard.
	if err := Verify(testPepper, h, ""); !errors.Is(err, ErrMismatch) {
		t.Fatalf("Verify with an empty secret = %v, want ErrMismatch", err)
	}
}

// TestPepperIsLoadBearing is the whole justification for having a pepper: a
// database stolen without it is useless, because every guess verifies against
// nothing. If this ever passes with the wrong pepper, the pepper has stopped
// reaching the KDF and offline attack becomes possible again.
func TestPepperIsLoadBearing(t *testing.T) {
	a := Pepper([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	b := Pepper([]byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))

	h, err := Hash(a, "shared secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := Verify(a, h, "shared secret"); err != nil {
		t.Fatalf("Verify under the minting pepper: %v", err)
	}
	if err := Verify(b, h, "shared secret"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("Verify under a different pepper = %v, want ErrMismatch", err)
	}
}

// Equal hashes for equal secrets would let an attacker with the database spot
// shared passwords across accounts and attack them once for many wins.
func TestHashUsesUniqueSalts(t *testing.T) {
	h1, err := Hash(testPepper, "same secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	h2, err := Hash(testPepper, "same secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h1 == h2 {
		t.Fatal("two hashes of the same secret are identical: salt is not random")
	}
	// Both must still verify — a unique salt is only useful if it round trips.
	if err := Verify(testPepper, h2, "same secret"); err != nil {
		t.Fatalf("Verify second hash: %v", err)
	}
}

// A malformed stored hash must be ErrHashInvalid, never ErrMismatch and never
// a panic. Callers show the user the same message either way, but they log
// them differently: a corrupt record is an operations problem, a mismatch is a
// user problem, and a slice bounds panic in decodeHash would be a denial of
// service reachable by anyone who can write a row.
func TestVerifyRejectsMalformedHashes(t *testing.T) {
	// One real hash to mutate, so the fields we are not testing stay valid.
	good, err := Hash(testPepper, "secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	parts := strings.Split(good, "$") // ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, key]
	if len(parts) != 6 {
		t.Fatalf("Hash produced %d fields, want 6: %q", len(parts), good)
	}
	rebuild := func(mutate func(p []string)) string {
		cp := append([]string(nil), parts...)
		mutate(cp)
		return strings.Join(cp, "$")
	}

	cases := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"not a hash at all", "hunter2"},
		{"too few fields", "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA"},
		{"too many fields", good + "$extra"},
		{"leading field not empty", strings.TrimPrefix(good, "$")},
		{"wrong algorithm", rebuild(func(p []string) { p[1] = "argon2i" })},
		{"bcrypt-shaped", "$2a$10$abcdefghijklmnopqrstuv"},
		{"unparseable version", rebuild(func(p []string) { p[2] = "v=nineteen" })},
		{"wrong version", rebuild(func(p []string) { p[2] = "v=16" })},
		{"missing version key", rebuild(func(p []string) { p[2] = "19" })},
		{"unknown parameter key", rebuild(func(p []string) { p[3] = "m=65536,t=3,p=4,k=1" })},
		{"parameter without value", rebuild(func(p []string) { p[3] = "m=65536,t,p=4" })},
		{"non-numeric parameter", rebuild(func(p []string) { p[3] = "m=lots,t=3,p=4" })},
		{"negative parameter", rebuild(func(p []string) { p[3] = "m=-1,t=3,p=4" })},
		{"zero memory", rebuild(func(p []string) { p[3] = "m=0,t=3,p=4" })},
		{"zero time", rebuild(func(p []string) { p[3] = "m=65536,t=0,p=4" })},
		{"zero threads", rebuild(func(p []string) { p[3] = "m=65536,t=3,p=0" })},
		{"threads beyond a uint8", rebuild(func(p []string) { p[3] = "m=65536,t=3,p=256" })},
		{"missing time parameter", rebuild(func(p []string) { p[3] = "m=65536,p=4" })},
		{"empty parameter section", rebuild(func(p []string) { p[3] = "" })},
		{"non-base64 salt", rebuild(func(p []string) { p[4] = "not base64!!" })},
		{"empty salt", rebuild(func(p []string) { p[4] = "" })},
		{"non-base64 key", rebuild(func(p []string) { p[5] = "***" })},
		{"empty key", rebuild(func(p []string) { p[5] = "" })},
		{"padded base64 salt", rebuild(func(p []string) { p[4] = base64.StdEncoding.EncodeToString([]byte("sixteen byte slt")) + "==" })},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(testPepper, tc.encoded, "secret")
			if !errors.Is(err, ErrHashInvalid) {
				t.Fatalf("Verify(%q) = %v, want ErrHashInvalid", tc.encoded, err)
			}
		})
	}
}

func TestNeedsRehash(t *testing.T) {
	current, err := Hash(testPepper, "secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if NeedsRehash(current) {
		t.Fatal("a freshly minted hash wants rehashing")
	}

	// Only the parameter fields matter here, so these are hand-built rather
	// than computed — no Argon2 work, and the weakness is explicit.
	salt := base64.RawStdEncoding.EncodeToString([]byte("sixteen byte slt"))
	key := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	with := func(params string) string {
		return "$argon2id$v=19$" + params + "$" + salt + "$" + key
	}

	weaker := []struct {
		name, params string
	}{
		{"less memory", "m=16384,t=3,p=4"},
		{"fewer passes", "m=65536,t=1,p=4"},
		{"fewer lanes", "m=65536,t=3,p=1"},
		{"weak in every dimension", "m=4096,t=1,p=1"},
	}
	for _, w := range weaker {
		if !NeedsRehash(with(w.params)) {
			t.Errorf("NeedsRehash(%s) = false, want true", w.name)
		}
	}

	// Stronger-than-current must not trigger a downgrade.
	if NeedsRehash(with("m=131072,t=4,p=8")) {
		t.Error("NeedsRehash on stronger parameters = true, want false (would downgrade)")
	}
	// Garbage is unusable, so the only safe answer is "replace it".
	for _, bad := range []string{"", "garbage", "$argon2id$v=19$m=65536,t=3,p=4"} {
		if !NeedsRehash(bad) {
			t.Errorf("NeedsRehash(%q) = false, want true", bad)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		ok   bool
	}{
		{"one under the minimum", strings.Repeat("a", MinPasswordLength-1), false},
		{"exactly the minimum", strings.Repeat("a", MinPasswordLength), true},
		{"comfortably long", strings.Repeat("a", 40), true},
		{"empty", "", false},
		// Runes, not bytes: an emoji password is short in characters but long
		// in bytes, and counting bytes would wave through a 3-character one.
		{"ten emoji", strings.Repeat("\U0001F510", MinPasswordLength), true},
		{"nine emoji", strings.Repeat("\U0001F510", MinPasswordLength-1), false},
		{"ten multi-byte letters", strings.Repeat("é", MinPasswordLength), true},
		{"1024 bytes", strings.Repeat("a", 1024), true},
		{"1025 bytes", strings.Repeat("a", 1025), false},
		// Long in bytes but not in runes: still rejected, because the cost to
		// the server is measured in bytes.
		{"300 emoji is 1200 bytes", strings.Repeat("\U0001F510", 300), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.pw)
			if tc.ok && err != nil {
				t.Fatalf("ValidatePassword(%d runes/%d bytes) = %v, want nil",
					len([]rune(tc.pw)), len(tc.pw), err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidatePassword(%d runes/%d bytes) = nil, want an error",
					len([]rune(tc.pw)), len(tc.pw))
			}
		})
	}
}

// The 256 is not decoration. It is the reason a word carries exactly 8 bits
// and the reason the documented "6 words = 48 bits" claim holds. Adding a word
// someone liked would quietly weaken the stated entropy and reintroduce modulo
// bias for any future byte-indexed implementation.
func TestWordlistIsExactly256(t *testing.T) {
	if len(wordlist) != 256 {
		t.Fatalf("len(wordlist) = %d, want exactly 256", len(wordlist))
	}
}

func TestWordlistIsUnique(t *testing.T) {
	seen := make(map[string]int, len(wordlist))
	for i, w := range wordlist {
		if first, dup := seen[w]; dup {
			t.Errorf("wordlist[%d] = %q duplicates wordlist[%d]; entropy is lower than advertised", i, w, first)
			continue
		}
		seen[w] = i
	}
}

// Anything outside a-z survives a round trip through NormalisePassphrase only
// by accident: digits pass, separators collapse, everything else is dropped.
// A word containing a dropped character could never be retyped successfully.
func TestWordlistIsPlainLowercase(t *testing.T) {
	for i, w := range wordlist {
		if w == "" {
			t.Errorf("wordlist[%d] is empty", i)
			continue
		}
		for _, r := range w {
			if r < 'a' || r > 'z' {
				t.Errorf("wordlist[%d] = %q contains %q, want only a-z", i, w, r)
				break
			}
		}
		if NormalisePassphrase(w) != w {
			t.Errorf("wordlist[%d] = %q does not survive NormalisePassphrase (got %q)", i, w, NormalisePassphrase(w))
		}
	}
}

func TestGeneratePassphrase(t *testing.T) {
	inList := make(map[string]bool, len(wordlist))
	for _, w := range wordlist {
		inList[w] = true
	}

	first, err := GeneratePassphrase()
	if err != nil {
		t.Fatalf("GeneratePassphrase: %v", err)
	}
	words := strings.Split(first, passphraseSeparator)
	if len(words) != PassphraseWords {
		t.Fatalf("GeneratePassphrase produced %d words (%q), want %d", len(words), first, PassphraseWords)
	}
	for _, w := range words {
		if !inList[w] {
			t.Errorf("generated word %q is not in the wordlist", w)
		}
	}

	// Not a randomness test — just a guard against a stuck generator, which is
	// the failure mode that would hand every user the same recovery phrase.
	// 100 draws from 256^6 colliding entirely is not a thing that happens.
	distinct := make(map[string]bool)
	for i := 0; i < 100; i++ {
		p, err := GeneratePassphrase()
		if err != nil {
			t.Fatalf("GeneratePassphrase #%d: %v", i, err)
		}
		distinct[p] = true
	}
	if len(distinct) < 90 {
		t.Fatalf("100 generated passphrases yielded only %d distinct values", len(distinct))
	}
}

// NormalisePassphrase is the contract with the user's fingers: they will retype
// the phrase from a screenshot, a sticky note or a password manager, with
// whatever separators and capitalisation they feel like. Every form here must
// hash to the same thing, and — because the normalised form is what was
// hashed at sign-up — this function must never change behaviour for input it
// already accepted.
func TestNormalisePassphraseCanonicalises(t *testing.T) {
	const want = "meadow-cobalt-jigsaw-hornet-fresco-lantern"
	variants := []struct{ name, in string }{
		{"canonical", "meadow-cobalt-jigsaw-hornet-fresco-lantern"},
		{"spaces", "meadow cobalt jigsaw hornet fresco lantern"},
		{"underscores", "meadow_cobalt_jigsaw_hornet_fresco_lantern"},
		{"upper case", "MEADOW-COBALT-JIGSAW-HORNET-FRESCO-LANTERN"},
		{"title case with spaces", "Meadow Cobalt Jigsaw Hornet Fresco Lantern"},
		{"mixed separators", "meadow_cobalt jigsaw-hornet  fresco\tlantern"},
		{"surrounding whitespace", "   meadow-cobalt-jigsaw-hornet-fresco-lantern\n"},
		{"leading and trailing dashes", "--meadow-cobalt-jigsaw-hornet-fresco-lantern--"},
		{"repeated separators", "meadow---cobalt   jigsaw__hornet - fresco -- lantern"},
		{"pasted with newlines", "meadow\ncobalt\njigsaw\nhornet\nfresco\nlantern\r\n"},
		{"stray punctuation", "meadow, cobalt, jigsaw, hornet, fresco, lantern."},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			if got := NormalisePassphrase(v.in); got != want {
				t.Fatalf("NormalisePassphrase(%q) = %q, want %q", v.in, got, want)
			}
		})
	}

	// Idempotent, and a generated phrase is already canonical — otherwise the
	// phrase shown at sign-up would not be the phrase that was hashed.
	p, err := GeneratePassphrase()
	if err != nil {
		t.Fatalf("GeneratePassphrase: %v", err)
	}
	if got := NormalisePassphrase(p); got != p {
		t.Fatalf("a generated passphrase is not canonical: %q -> %q", p, got)
	}
	if got := NormalisePassphrase(NormalisePassphrase(p)); got != p {
		t.Fatalf("NormalisePassphrase is not idempotent: %q -> %q", p, got)
	}

	// Different phrases must stay different: normalisation is forgiving about
	// formatting, not about words.
	if NormalisePassphrase("meadow-cobalt") == NormalisePassphrase("meadow-copper") {
		t.Fatal("distinct passphrases normalise to the same value")
	}
}

func TestNewSessionToken(t *testing.T) {
	const n = 500
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		tok, err := NewSessionToken()
		if err != nil {
			t.Fatalf("NewSessionToken #%d: %v", i, err)
		}
		if seen[tok] {
			t.Fatalf("NewSessionToken repeated a token after %d calls: %q", i, tok)
		}
		seen[tok] = true

		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			t.Fatalf("token %q is not raw base64url: %v", tok, err)
		}
		if len(raw) < 32 {
			t.Fatalf("token carries %d bytes of entropy, want at least 32", len(raw))
		}
		// The token goes in a cookie and in a URL-safe context, so it must
		// contain nothing that needs escaping.
		if strings.ContainsAny(tok, "+/=; ") {
			t.Fatalf("token %q contains characters that need escaping in a cookie", tok)
		}
	}
}

func TestLoadOrCreatePepperPersists(t *testing.T) {
	// The env var takes priority over the file, so make sure the ambient
	// environment cannot influence the test.
	t.Setenv(PepperEnv, "")

	dir := filepath.Join(t.TempDir(), "data")
	first, err := LoadOrCreatePepper(dir)
	if err != nil {
		t.Fatalf("LoadOrCreatePepper (first call): %v", err)
	}
	if len(first) < 16 {
		t.Fatalf("generated pepper is %d bytes, want at least 16", len(first))
	}

	path := filepath.Join(dir, "pepper.key")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pepper.key was not written: %v", err)
	}

	// Stability is the whole point: a pepper that changed between runs would
	// invalidate every stored password on restart.
	second, err := LoadOrCreatePepper(dir)
	if err != nil {
		t.Fatalf("LoadOrCreatePepper (second call): %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("LoadOrCreatePepper returned a different pepper on the second call")
	}

	// And a hash minted under the first load must verify under the second.
	h, err := Hash(first, "secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := Verify(second, h, "secret"); err != nil {
		t.Fatalf("Verify with the reloaded pepper: %v", err)
	}
}

func TestLoadOrCreatePepperFromEnv(t *testing.T) {
	want := []byte("this is thirty-two bytes long!!!")
	t.Setenv(PepperEnv, base64.StdEncoding.EncodeToString(want))

	dir := t.TempDir()
	got, err := LoadOrCreatePepper(dir)
	if err != nil {
		t.Fatalf("LoadOrCreatePepper: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("pepper = %q, want %q", got, want)
	}
	// The env var means "keep the secret off the data volume", so nothing may
	// be written to disk.
	if _, err := os.Stat(filepath.Join(dir, "pepper.key")); err == nil {
		t.Fatal("pepper.key was written even though MUX_PEPPER was set")
	}

	// Surrounding whitespace is what you get from a shell heredoc or a
	// docker secret file; it must not change the decoded value.
	t.Setenv(PepperEnv, "  "+base64.StdEncoding.EncodeToString(want)+"\n")
	got, err = LoadOrCreatePepper(dir)
	if err != nil {
		t.Fatalf("LoadOrCreatePepper with padded value: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("pepper = %q, want %q", got, want)
	}
}

// A bad MUX_PEPPER must be a startup failure, not a silent fallback to a
// generated file: booting with the wrong pepper would reject every existing
// login and look like mass credential corruption.
func TestLoadOrCreatePepperRejectsBadEnv(t *testing.T) {
	cases := []struct{ name, value string }{
		{"not base64", "this is not base64!!!"},
		{"too short", base64.StdEncoding.EncodeToString([]byte("15 bytes only!!"))},
		{"one byte", base64.StdEncoding.EncodeToString([]byte("x"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(PepperEnv, tc.value)
			dir := t.TempDir()
			if _, err := LoadOrCreatePepper(dir); err == nil {
				t.Fatalf("LoadOrCreatePepper accepted %s = %q", PepperEnv, tc.value)
			}
			if _, err := os.Stat(filepath.Join(dir, "pepper.key")); err == nil {
				t.Fatal("a pepper file was created despite the invalid environment value")
			}
		})
	}
}

// A corrupt on-disk pepper must also fail loudly rather than be replaced with
// a fresh one, for the same reason.
func TestLoadOrCreatePepperRejectsCorruptFile(t *testing.T) {
	t.Setenv(PepperEnv, "")
	for _, tc := range []struct{ name, contents string }{
		{"not base64", "@@@@ not base64 @@@@"},
		{"too short", base64.StdEncoding.EncodeToString([]byte("short"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pepper.key"), []byte(tc.contents), 0o600); err != nil {
				t.Fatalf("write pepper.key: %v", err)
			}
			if _, err := LoadOrCreatePepper(dir); err == nil {
				t.Fatalf("LoadOrCreatePepper accepted a %s pepper file", tc.name)
			}
		})
	}
}
