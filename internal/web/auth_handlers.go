// Sign-up, sign-in, sign-out, and the passphrase-based password reset.
//
// There is no mail server here, so the usual "click the link we emailed you"
// recovery does not exist. Instead every account is handed a random recovery
// passphrase once, at sign-up. Presenting username + passphrase proves
// ownership well enough to set a new password. It is stored hashed exactly
// like the password, so nobody — including whoever runs the server — can read
// it back out of the database. If a user loses both, the only way in is an
// admin resetting the account by hand, and that is stated plainly on the page
// where the passphrase is shown.
package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"muxxerr/internal/auth"
	"muxxerr/internal/store"
)

type loginView struct {
	Page           PageData
	Next           string
	Username       string
	SignupsEnabled bool
}

type signupView struct {
	Page     PageData
	Username string
	// Host and ExampleApp drive the live URL preview under the username field.
	// A username becomes a permanent path segment, and "this can't be changed
	// later" is easy to skim past in a way that watching your own name appear
	// inside a URL is not.
	Host       string
	ExampleApp string
}

type passphraseView struct {
	Page       PageData
	Passphrase string
	Next       string
}

type resetView struct {
	Page     PageData
	Username string
	Stage    string // "verify" or "choose"
	Token    string
}

// ---------------------------------------------------------------- login

func (s *Server) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if u := s.UserFor(r); u != nil {
			http.Redirect(w, r, s.nextForUser(r.URL.Query().Get("next"), u), http.StatusSeeOther)
			return
		}
		v := loginView{
			Page:           s.page(w, r, "Sign in"),
			Next:           safeNext(r.URL.Query().Get("next")),
			SignupsEnabled: s.signupsOpen(r.Context()),
		}
		s.render(w, r, "login", http.StatusOK, v)
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}

	ctx := r.Context()
	username := NormaliseUsername(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))

	show := func(msg string) {
		v := loginView{
			Page:           s.page(w, r, "Sign in"),
			Next:           next,
			Username:       username,
			SignupsEnabled: s.signupsOpen(ctx),
		}
		v.Page.Error = msg
		s.render(w, r, "login", http.StatusUnauthorized, v)
	}

	// Throttle on the account and on the source address independently, so one
	// user being attacked cannot lock out the whole server and one address
	// cannot spray across many accounts.
	keys := []string{"login:" + username, "login:ip:" + s.clientIP(r)}
	for _, k := range keys {
		if locked, until, err := s.store.CheckThrottle(ctx, k); err == nil && locked {
			show("Too many attempts. Try again in " + humanUntil(until) + ".")
			return
		}
	}

	u, err := s.store.UserByName(ctx, username)
	if err != nil {
		// Burn the same Argon2 work as a real check so that a nonexistent
		// account cannot be told apart from a wrong password by timing.
		auth.FakeVerify(s.pepper, password)
		s.recordAuthFailure(r, keys)
		show("That username and password do not match.")
		return
	}
	if u.IsDisabled {
		auth.FakeVerify(s.pepper, password)
		show("That account has been disabled. Contact the administrator.")
		return
	}
	if err := auth.Verify(s.pepper, u.PasswordHash, password); err != nil {
		if !errors.Is(err, auth.ErrMismatch) {
			slog.Error("password hash unusable", "user", username, "error", err)
		}
		s.recordAuthFailure(r, keys)
		s.audit(r, username, "login.failed", username, "")
		show("That username and password do not match.")
		return
	}

	for _, k := range keys {
		_ = s.store.ClearThrottle(ctx, k)
	}

	// Opportunistically upgrade a hash made with weaker parameters. This is
	// the only moment the plaintext is available to do it.
	if auth.NeedsRehash(u.PasswordHash) {
		if nh, err := auth.Hash(s.pepper, password); err == nil {
			if err := s.store.SetCredentials(ctx, u.ID, nh, ""); err != nil {
				slog.Warn("rehash failed", "user", username, "error", err)
			} else if fresh, err := s.store.UserByName(ctx, username); err == nil {
				u = fresh // credential_gen moved; the new session must match it
			}
		}
	}

	if err := s.startSession(w, r, u); err != nil {
		slog.Error("create session", "error", err)
		show("Could not start a session. Try again.")
		return
	}
	_ = s.store.TouchLogin(ctx, u.ID)
	s.audit(r, username, "login", username, "")
	http.Redirect(w, r, s.nextForUser(next, u), http.StatusSeeOther)
}

func (s *Server) recordAuthFailure(r *http.Request, keys []string) {
	for _, k := range keys {
		if _, err := s.store.RecordFailure(r.Context(), k, throttleFree, throttleBase, throttleMax); err != nil {
			slog.Warn("throttle write failed", "key", k, "error", err)
		}
	}
}

func humanUntil(t time.Time) string {
	d := time.Until(t).Round(time.Second)
	if d < time.Second {
		return "a moment"
	}
	if d < time.Minute {
		return d.String()
	}
	return d.Round(time.Minute).String()
}

func (s *Server) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}
	if u := s.UserFor(r); u != nil {
		s.audit(r, u.Username, "logout", u.Username, "")
	}
	s.endSession(w, r)
	s.setFlash(w, "You have been signed out.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --------------------------------------------------------------- signup

func (s *Server) HandleSignup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.signupsOpen(ctx) {
		s.fail(w, r, http.StatusForbidden,
			"Sign-ups are closed on this server. Ask the administrator for an account.")
		return
	}
	if r.Method == http.MethodGet {
		s.render(w, r, "signup", http.StatusOK, s.signupPage(w, r, "", ""))
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}

	username := NormaliseUsername(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("password_confirm")

	show := func(code int, msg string) {
		s.render(w, r, "signup", code, s.signupPage(w, r, username, msg))
	}

	if msg := ValidateUsername(username); msg != "" {
		show(http.StatusBadRequest, msg)
		return
	}
	if err := auth.ValidatePassword(password); err != nil {
		show(http.StatusBadRequest, capitalise(err.Error())+".")
		return
	}
	if password != confirm {
		show(http.StatusBadRequest, "The two passwords do not match.")
		return
	}

	passphrase, err := auth.GeneratePassphrase()
	if err != nil {
		slog.Error("generate passphrase", "error", err)
		show(http.StatusInternalServerError, "Could not create the account. Try again.")
		return
	}
	pwHash, err := auth.Hash(s.pepper, password)
	if err != nil {
		slog.Error("hash password", "error", err)
		show(http.StatusInternalServerError, "Could not create the account. Try again.")
		return
	}
	ppHash, err := auth.Hash(s.pepper, auth.NormalisePassphrase(passphrase))
	if err != nil {
		slog.Error("hash passphrase", "error", err)
		show(http.StatusInternalServerError, "Could not create the account. Try again.")
		return
	}

	// The first account to exist is the administrator. There is no other
	// bootstrap path and no default password to forget to change.
	count, err := s.store.CountUsers(ctx)
	if err != nil {
		slog.Error("count users", "error", err)
		show(http.StatusInternalServerError, "Could not create the account. Try again.")
		return
	}
	isAdmin := count == 0

	u, err := s.store.CreateUser(ctx, username, pwHash, ppHash, isAdmin)
	if err != nil {
		if errors.Is(err, store.ErrUsernameTaken) {
			show(http.StatusConflict, "That username is already taken.")
			return
		}
		slog.Error("create user", "error", err)
		show(http.StatusInternalServerError, "Could not create the account. Try again.")
		return
	}
	if err := s.startSession(w, r, u); err != nil {
		slog.Error("create session", "error", err)
	}
	s.audit(r, username, "signup", username, map[bool]string{true: "first user, made admin", false: ""}[isAdmin])
	slog.Info("account created", "user", username, "admin", isAdmin)

	// The passphrase is rendered exactly once, here, and never stored in
	// recoverable form. It is not put in a redirect or a flash cookie —
	// keeping it out of the URL, out of history, and out of any log.
	v := passphraseView{
		Page:       s.page(w, r, "Save your recovery passphrase"),
		Passphrase: passphrase,
	}
	s.render(w, r, "passphrase", http.StatusOK, v)
}

// signupPage assembles the sign-up view, including the URL preview.
func (s *Server) signupPage(w http.ResponseWriter, r *http.Request, username, errMsg string) signupView {
	v := signupView{
		Page:       s.page(w, r, "Create an account"),
		Username:   username,
		Host:       previewHost(r),
		ExampleApp: s.exampleAppName(),
	}
	v.Page.Error = errMsg
	return v
}

// exampleAppName names a real app in the preview rather than an invented one,
// so the example is something the reader can go and look at.
func (s *Server) exampleAppName() string {
	if len(s.cfg.Apps) > 0 {
		return s.cfg.Apps[0].Name
	}
	return "app"
}

// previewHost is the Host header, used only as display text in the preview.
//
// It is echoed back into the page, so it is checked rather than trusted: the
// value is whatever the client sent, and while html/template makes injection a
// non-issue, a page reading "Your apps will live at <500 characters of
// nonsense>" is its own small failure. Anything that is not a plausible
// host[:port] is dropped and the preview falls back to showing just the path.
func previewHost(r *http.Request) string {
	h := r.Host
	if h == "" || len(h) > 100 || !hostRe.MatchString(h) {
		return ""
	}
	return h
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---------------------------------------------------------------- reset

// Reset is two stages in one handler. Stage "verify" takes username plus
// passphrase; on success it issues a short-lived, single-use token that
// authorises stage "choose", where the new password is set.
//
// The token is a session for a user whose credentials are about to change,
// so it is deliberately NOT the normal session cookie: it is scoped to the
// reset page alone and expires in fifteen minutes.
func (s *Server) HandleReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == http.MethodGet {
		s.render(w, r, "reset", http.StatusOK, resetView{
			Page:  s.page(w, r, "Reset your password"),
			Stage: "verify",
		})
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}

	stage := r.PostFormValue("stage")
	username := NormaliseUsername(r.PostFormValue("username"))

	show := func(code int, st, msg string, token string) {
		v := resetView{
			Page:     s.page(w, r, "Reset your password"),
			Username: username, Stage: st, Token: token,
		}
		v.Page.Error = msg
		s.render(w, r, "reset", code, v)
	}

	switch stage {
	case "choose":
		token := r.PostFormValue("token")
		userID, ok := s.resetTokens.consume(token, username)
		if !ok {
			show(http.StatusBadRequest, "verify", "That reset has expired. Start again.", "")
			return
		}
		password := r.PostFormValue("password")
		if err := auth.ValidatePassword(password); err != nil {
			// Hand back a fresh token so a rejected password does not force
			// the passphrase to be retyped.
			show(http.StatusBadRequest, "choose", capitalise(err.Error())+".",
				s.resetTokens.issue(userID, username))
			return
		}
		if password != r.PostFormValue("password_confirm") {
			show(http.StatusBadRequest, "choose", "The two passwords do not match.",
				s.resetTokens.issue(userID, username))
			return
		}

		pwHash, err := auth.Hash(s.pepper, password)
		if err != nil {
			slog.Error("hash password", "error", err)
			show(http.StatusInternalServerError, "verify", "Could not reset the password. Try again.", "")
			return
		}
		// Rotate the passphrase too: the old one has now been typed into a
		// form and may be sitting in a password manager, a screenshot, or a
		// browser's autofill store. A reset should leave the account with a
		// recovery secret that has not been used.
		newPassphrase, err := auth.GeneratePassphrase()
		if err != nil {
			slog.Error("generate passphrase", "error", err)
			show(http.StatusInternalServerError, "verify", "Could not reset the password. Try again.", "")
			return
		}
		ppHash, err := auth.Hash(s.pepper, auth.NormalisePassphrase(newPassphrase))
		if err != nil {
			slog.Error("hash passphrase", "error", err)
			show(http.StatusInternalServerError, "verify", "Could not reset the password. Try again.", "")
			return
		}

		// SetCredentials bumps credential_gen, which invalidates every session
		// the account had — the point of a reset is that whoever else was
		// signed in no longer is.
		if err := s.store.SetCredentials(ctx, userID, pwHash, ppHash); err != nil {
			slog.Error("set credentials", "error", err)
			show(http.StatusInternalServerError, "verify", "Could not reset the password. Try again.", "")
			return
		}
		_ = s.store.ClearThrottle(ctx, "reset:"+username)
		_ = s.store.ClearThrottle(ctx, "login:"+username)
		s.audit(r, username, "password.reset", username, "self-serve via passphrase")
		slog.Info("password reset", "user", username)

		u, err := s.store.UserByID(ctx, userID)
		if err == nil {
			if err := s.startSession(w, r, u); err != nil {
				slog.Error("create session", "error", err)
			}
		}
		s.render(w, r, "passphrase", http.StatusOK, passphraseView{
			Page:       s.page(w, r, "Save your new recovery passphrase"),
			Passphrase: newPassphrase,
		})
		return

	default: // "verify"
		key := "reset:" + username
		ipKey := "reset:ip:" + s.clientIP(r)
		for _, k := range []string{key, ipKey} {
			if locked, until, err := s.store.CheckThrottle(ctx, k); err == nil && locked {
				show(http.StatusTooManyRequests, "verify",
					"Too many attempts. Try again in "+humanUntil(until)+".", "")
				return
			}
		}

		passphrase := auth.NormalisePassphrase(r.PostFormValue("passphrase"))
		u, err := s.store.UserByName(ctx, username)
		if err != nil || u.IsDisabled {
			auth.FakeVerify(s.pepper, passphrase)
			s.recordAuthFailure(r, []string{key, ipKey})
			show(http.StatusUnauthorized, "verify", "That username and passphrase do not match.", "")
			return
		}
		if err := auth.Verify(s.pepper, u.PassphraseHash, passphrase); err != nil {
			s.recordAuthFailure(r, []string{key, ipKey})
			s.audit(r, username, "reset.failed", username, "")
			show(http.StatusUnauthorized, "verify", "That username and passphrase do not match.", "")
			return
		}
		for _, k := range []string{key, ipKey} {
			_ = s.store.ClearThrottle(ctx, k)
		}
		s.render(w, r, "reset", http.StatusOK, resetView{
			Page:     s.page(w, r, "Choose a new password"),
			Username: username,
			Stage:    "choose",
			Token:    s.resetTokens.issue(u.ID, username),
		})
	}
}
