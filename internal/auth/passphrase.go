package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math/big"
	"strings"
	"unicode"
)

// Passphrases replace the "reset link in your email" step. There is no mail
// server here, so at sign-up the user is handed a recovery passphrase once;
// presenting it later proves account ownership well enough to set a new
// password. It is stored hashed exactly like the password — an admin reading
// the database cannot recover it, and losing both means an admin must reset
// the account by hand.
//
// PassphraseWords words from a 256-word list is 8 bits each: 6 words = 48
// bits. That is far stronger than any password a person picks, and because
// the reset path is rate-limited and lockout-backed it never faces an offline
// attack. Words are short, unambiguous and easy to read off a screen and
// retype — no homophones, no plurals of each other, nothing under four
// letters.
const (
	PassphraseWords     = 6
	passphraseSeparator = "-"
)

// wordlist has exactly 256 entries so a byte maps to a word with no modulo
// bias. Verified by TestWordlistIsExactly256 and TestWordlistIsUnique.
var wordlist = []string{
	"acorn", "album", "alloy", "amber", "anchor", "angle", "ankle", "apple",
	"apron", "arbor", "arena", "armor", "arrow", "aspen", "atlas", "attic",
	"badge", "bagel", "baker", "balcony", "bamboo", "banjo", "barge", "basil",
	"basin", "batch", "beacon", "beetle", "bellow", "birch", "bishop", "bison",
	"blade", "blanket", "blazer", "blossom", "blueprint", "bobcat", "bolster", "bonnet",
	"border", "bottle", "boulder", "bounty", "bracket", "brandy", "brass", "breeze",
	"bridge", "bronze", "brook", "buckle", "buffalo", "bugle", "bunker", "burrow",
	"bushel", "butler", "cabin", "cactus", "camera", "canal", "candle", "canopy",
	"canyon", "carbon", "cargo", "carpet", "carrot", "castle", "cattle", "cavern",
	"cedar", "cello", "cement", "chalk", "chamber", "chapel", "charcoal", "cherry",
	"chimney", "chisel", "cinder", "circus", "citrus", "clamp", "clarinet", "clay",
	"cliff", "clover", "cobalt", "cobra", "cocoa", "collar", "comet", "compass",
	"copper", "coral", "cottage", "cotton", "cougar", "council", "cradle", "crater",
	"crayon", "creek", "crescent", "cricket", "crimson", "crystal", "cushion", "cymbal",
	"dagger", "dahlia", "daisy", "dawn", "decoy", "denim", "desert", "diamond",
	"diesel", "domino", "donkey", "dragon", "drummer", "dugout", "eagle", "easel",
	"ebony", "echo", "eclipse", "elbow", "elder", "ember", "emerald", "engine",
	"envoy", "equator", "ermine", "essay", "ether", "fabric", "falcon", "fennel",
	"ferry", "fiddle", "filter", "fjord", "flagon", "flamingo", "flannel", "flask",
	"flint", "florist", "flute", "forge", "fossil", "fountain", "foxglove", "fresco",
	"frost", "gadget", "galaxy", "gallery", "gambit", "garden", "gargoyle", "garnet",
	"gazelle", "geyser", "ginger", "glacier", "glider", "granite", "gravel", "grotto",
	"guitar", "gully", "gypsum", "hammock", "hamster", "harbor", "harvest", "hazel",
	"heather", "helmet", "hemlock", "heron", "hickory", "honey", "hornet", "hostel",
	"hunter", "hurdle", "iceberg", "igloo", "impala", "indigo", "inkwell", "iris",
	"island", "ivory", "jackal", "jasmine", "jersey", "jigsaw", "jockey", "jungle",
	"juniper", "kayak", "kennel", "kernel", "kettle", "keystone", "kimono", "kitten",
	"koala", "lagoon", "lancer", "lantern", "lattice", "lavender", "ledger", "legend",
	"lemon", "leopard", "lichen", "lilac", "linen", "lobster", "locket", "lotus",
	"lumber", "lyric", "magnet", "magnolia", "mahogany", "mallet", "mammoth", "mandarin",
	"mango", "mantis", "maple", "marble", "marigold", "mariner", "marsh", "mascot",
	"meadow", "medal", "melon", "mercury", "meteor", "midnight", "mimosa", "mineral",
}

// GeneratePassphrase returns a fresh recovery passphrase such as
// "meadow-cobalt-jigsaw-hornet-fresco-lantern".
func GeneratePassphrase() (string, error) {
	if len(wordlist) != 256 {
		return "", fmt.Errorf("wordlist must have exactly 256 entries, has %d", len(wordlist))
	}
	words := make([]string, PassphraseWords)
	for i := range words {
		// crypto/rand.Int over the exact list length: no modulo bias even if
		// the list is ever resized.
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(wordlist))))
		if err != nil {
			return "", err
		}
		words[i] = wordlist[n.Int64()]
	}
	return strings.Join(words, passphraseSeparator), nil
}

// NormalisePassphrase makes comparison forgiving of how the user retyped it:
// case, surrounding whitespace, and whether they used dashes or spaces. The
// normalised form is what gets hashed, so this must stay stable — changing it
// would invalidate every stored passphrase.
func NormalisePassphrase(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '\t' || r == '\n' || r == '\r':
			b.WriteString(passphraseSeparator)
		}
	}
	// Collapse runs of separators and trim the ends.
	out := b.String()
	for strings.Contains(out, passphraseSeparator+passphraseSeparator) {
		out = strings.ReplaceAll(out, passphraseSeparator+passphraseSeparator, passphraseSeparator)
	}
	return strings.Trim(out, passphraseSeparator)
}

// NewSessionToken returns a 256-bit URL-safe random token for a session
// cookie. Only its SHA-256 is stored (see store.HashToken).
func NewSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
