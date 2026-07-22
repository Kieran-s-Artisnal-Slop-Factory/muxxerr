package web

import (
	"sync"
	"time"

	"muxxerr/internal/auth"
)

// resetTokenTTL bounds the window between proving ownership of a passphrase
// and choosing the new password. Long enough to think about a password, short
// enough that an abandoned browser tab is not a standing key to the account.
const resetTokenTTL = 15 * time.Minute

// resetTokens holds the short-lived authorisations that sit between the two
// halves of a password reset.
//
// These live in memory rather than in the database on purpose. They are
// single-use and expire in minutes, so persistence would buy nothing except a
// table of live credentials surviving a restart — the safer failure mode is
// that a gateway restart invalidates every in-flight reset and the user starts
// over. The map is small and self-pruning.
type resetTokenStore struct {
	mu     sync.Mutex
	tokens map[string]resetGrant
}

type resetGrant struct {
	userID   int64
	username string
	expires  time.Time
}

func newResetTokenStore() *resetTokenStore {
	return &resetTokenStore{tokens: map[string]resetGrant{}}
}

// issue mints a token authorising a password change for one user.
func (s *resetTokenStore) issue(userID int64, username string) string {
	tok, err := auth.NewSessionToken()
	if err != nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.tokens[tok] = resetGrant{userID: userID, username: username, expires: time.Now().Add(resetTokenTTL)}
	return tok
}

// consume validates and immediately destroys a token. The username must match
// the one the token was issued for, so a token cannot be replayed against a
// different account by editing the form.
func (s *resetTokenStore) consume(token, username string) (int64, bool) {
	if token == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.tokens[token]
	if !ok {
		return 0, false
	}
	delete(s.tokens, token) // single use, even on a failed match
	if time.Now().After(g.expires) || g.username != username {
		return 0, false
	}
	return g.userID, true
}

func (s *resetTokenStore) pruneLocked() {
	now := time.Now()
	for k, v := range s.tokens {
		if now.After(v.expires) {
			delete(s.tokens, k)
		}
	}
}
