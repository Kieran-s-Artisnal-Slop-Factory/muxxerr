package web

import (
	"crypto/subtle"
	"regexp"
	"strings"

	"muxerr/internal/config"
)

// isReservedName keeps usernames out of the URL segments the gateway owns.
// Sharing the list with app names is deliberate: /login must mean the login
// page whether somebody tried to register it as a username or as an app.
func isReservedName(s string) bool { return config.Reserved(s) }

func subtleCompare(a, b string) bool {
	// Length is not secret here (both are fixed-size tokens), but comparing
	// this way keeps the timing flat regardless.
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// hostRe matches a plausible host[:port] for display in the sign-up preview.
var hostRe = regexp.MustCompile(`^[A-Za-z0-9._-]+(:[0-9]{1,5})?$`)

// usernameRe matches what can safely be a URL path segment and a directory
// name on every platform this runs on: lowercase, no dots, no separators.
var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,30}$`)

// NormaliseUsername lowercases and trims. Usernames are compared and stored in
// this form so that "Kieran" and "kieran" can never become two accounts whose
// URLs differ only in case.
func NormaliseUsername(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidateUsername rejects anything that would make a confusing or unsafe URL
// segment. The reserved list is shared with the app names so that a user can
// never claim /admin or /login as their namespace.
func ValidateUsername(s string) string {
	switch {
	case s == "":
		return "Choose a username."
	case len(s) < 2:
		return "Usernames must be at least 2 characters."
	case len(s) > 31:
		return "Usernames must be at most 31 characters."
	case !usernameRe.MatchString(s):
		return "Usernames can contain lowercase letters, numbers, dashes and underscores, and must start with a letter or number."
	case isReservedName(s):
		return "That username is reserved. Please choose another."
	}
	return ""
}
