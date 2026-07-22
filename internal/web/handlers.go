// The pages a signed-in person uses: the app chooser and their account.
package web

import (
	"log/slog"
	"net/http"
	"net/url"

	"muxxerr/internal/auth"
	"muxxerr/internal/config"
	"muxxerr/internal/store"
)

type installedApp struct {
	Name        string
	Title       string
	Description string
	URL         string
	Running     bool
	// IconURL points at the app's own icon inside its build. It is served
	// without a session (see gateway/public.go), so it also renders on the
	// login page if that is ever wanted.
	IconURL string
	Added   string // date the user provisioned it
	Size    string // on-disk database size, humanised
	LogsURL string
	// ExploreURL opens this app's database in the snapshot viewer. Only set
	// for apps that have a database to explore.
	ExploreURL string
}

type availableApp struct {
	Name        string
	Title       string
	Description string
}

type chooserView struct {
	Page      PageData
	Username  string
	Installed []installedApp
	Available []availableApp
}

// HandleRoot is the signed-in dashboard, and the sign-in page for everyone
// else. It is also where a user lands after aiming at an app they have not
// added yet — hence the ?add= parameter.
func (s *Server) HandleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.fail(w, r, http.StatusNotFound, "There is nothing at that address.")
		return
	}
	u := s.UserFor(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	ctx := r.Context()
	instances, err := s.store.UserInstances(ctx, u.ID)
	if err != nil {
		slog.Error("list instances", "user", u.Username, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Could not load your apps.")
		return
	}
	have := map[string]bool{}
	v := chooserView{Page: s.page(w, r, "Your apps"), Username: u.Username}
	for _, in := range instances {
		app, ok := s.cfg.App(in.App)
		if !ok {
			// Configured away since it was added. Say so rather than pretend.
			v.Installed = append(v.Installed, installedApp{
				Name: in.App, Title: in.App,
				Description: "This app is no longer configured on the server.",
				URL:         "",
			})
			have[in.App] = true
			continue
		}
		have[app.Name] = true
		_, running := s.sup.Get(u.Username, app.Name)
		base := "/" + u.Username + "/" + app.Name + "/"
		card := installedApp{
			Name:        app.Name,
			Title:       app.Title,
			Description: app.Description,
			URL:         base,
			Running:     running || app.Kind == config.KindStatic,
			Added:       in.CreatedAt.Local().Format("2 Jan 2006"),
			Size:        humanBytes(instanceDBBytes(s.cfg, u.Username, app)),
		}
		// A static app has no child process, so it has nothing to log and no
		// database to size. Offering the links anyway would be two dead ends.
		if app.Kind == config.KindSync {
			card.LogsURL = "/apps/" + app.Name + "/logs"
			card.ExploreURL = "/tools/sqlite?app=" + url.QueryEscape(app.Name)
		} else {
			card.Size = "no database"
		}
		if icon := s.appIcon(app.Name); icon != "" {
			card.IconURL = base + icon
		}
		v.Installed = append(v.Installed, card)
	}
	for i := range s.cfg.Apps {
		a := &s.cfg.Apps[i]
		if !have[a.Name] {
			v.Available = append(v.Available, availableApp{
				Name: a.Name, Title: a.Title, Description: a.Description,
			})
		}
	}

	if add := r.URL.Query().Get("add"); add != "" {
		if a, ok := s.cfg.App(add); ok && !have[add] {
			v.Page.Flash = "You have not set up " + a.Title + " yet. Add it below to start using it."
		}
	}
	s.render(w, r, "chooser", http.StatusOK, v)
}

// HandleAddApp provisions an app for the signed-in user. It only records the
// choice — the instance's database and process are created lazily on first
// use, so adding an app costs nothing until it is opened.
func (s *Server) HandleAddApp(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}
	app, ok := s.cfg.App(r.PathValue("app"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, "No such app.")
		return
	}
	if err := s.store.AddInstance(r.Context(), u.ID, app.Name); err != nil {
		slog.Error("add instance", "user", u.Username, "app", app.Name, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Could not add that app.")
		return
	}
	s.audit(r, u.Username, "instance.add", u.Username+"/"+app.Name, "")
	s.setFlash(w, app.Title+" is ready.")
	http.Redirect(w, r, "/"+u.Username+"/"+app.Name+"/", http.StatusSeeOther)
}

// HandleRemoveApp forgets a user's app. The database is deliberately left on
// disk: removing an app from a dashboard should not be an irreversible way to
// destroy data, and the admin page is where deletion belongs if it is wanted.
func (s *Server) HandleRemoveApp(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}
	name := r.PathValue("app")
	if _, ok := s.cfg.App(name); !ok {
		s.fail(w, r, http.StatusNotFound, "No such app.")
		return
	}
	if err := s.store.RemoveInstance(r.Context(), u.ID, name); err != nil {
		slog.Error("remove instance", "user", u.Username, "app", name, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Could not remove that app.")
		return
	}
	_ = s.sup.Stop(u.Username, name)
	s.audit(r, u.Username, "instance.remove", u.Username+"/"+name, "data kept on disk")
	s.setFlash(w, "Removed. Your data is still on the server — add the app again to get it back.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// HandleExportOwn lets a user download their own app database. The same
// consistent-snapshot path the admin export uses.
func (s *Server) HandleExportOwn(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	app, ok := s.cfg.App(r.PathValue("app"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, "No such app.")
		return
	}
	has, err := s.store.HasInstance(r.Context(), u.ID, app.Name)
	if err != nil || !has {
		s.fail(w, r, http.StatusNotFound, "You have not set up that app.")
		return
	}
	s.audit(r, u.Username, "export.self", u.Username+"/"+app.Name, "")
	s.exporter.ExportDB(w, r, u.Username, app)
}

// -------------------------------------------------------------- account

type accountView struct {
	Page              PageData
	Sessions          int
	PassphraseRotated bool
}

func (s *Server) HandleAccount(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	s.render(w, r, "account", http.StatusOK, s.accountPage(w, r, u, ""))
}

// accountPage builds the account view, optionally with an error banner. The
// three account actions all re-render this page on failure, so the assembly
// lives in one place.
func (s *Server) accountPage(w http.ResponseWriter, r *http.Request, u *store.User, errMsg string) accountView {
	v := accountView{
		Page:     s.page(w, r, "Account"),
		Sessions: s.countSessions(r, u.ID),
		// credential_gen starts at 1 and is bumped by every credential change,
		// so anything above 1 means the passphrase the account was created with
		// is no longer the current one.
		PassphraseRotated: u.CredentialGen > 1,
	}
	v.Page.Error = errMsg
	return v
}

// accountAction is the shared preamble for the three POST handlers below:
// require a session, check the form token, and hand back a way to re-render
// the page with an error.
func (s *Server) accountAction(w http.ResponseWriter, r *http.Request) (*store.User, func(int, string), bool) {
	u := s.requireUser(w, r)
	if u == nil {
		return nil, nil, false
	}
	if !s.checkCSRF(w, r) {
		return nil, nil, false
	}
	show := func(code int, msg string) {
		s.render(w, r, "account", code, s.accountPage(w, r, u, msg))
	}
	return u, show, true
}

func (s *Server) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	u, show, ok := s.accountAction(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	if err := auth.Verify(s.pepper, u.PasswordHash, r.PostFormValue("current_password")); err != nil {
		s.audit(r, u.Username, "password.change.failed", u.Username, "wrong current password")
		show(http.StatusUnauthorized, "Your current password is not correct.")
		return
	}
	// Field names match the signup form deliberately: "password" and
	// "password_confirm" everywhere a password is chosen, so a browser's
	// password manager sees the same shape on every page that sets one.
	next := r.PostFormValue("password")
	if err := auth.ValidatePassword(next); err != nil {
		show(http.StatusBadRequest, capitalise(err.Error())+".")
		return
	}
	if next != r.PostFormValue("password_confirm") {
		show(http.StatusBadRequest, "The two new passwords do not match.")
		return
	}
	hash, err := auth.Hash(s.pepper, next)
	if err != nil {
		slog.Error("hash password", "error", err)
		show(http.StatusInternalServerError, "Could not change the password.")
		return
	}
	if err := s.store.SetCredentials(ctx, u.ID, hash, ""); err != nil {
		slog.Error("set credentials", "error", err)
		show(http.StatusInternalServerError, "Could not change the password.")
		return
	}
	// Every session, including this one, is now invalid — credential_gen
	// moved. Start a fresh one so the user is not thrown out of the page they
	// just used, while every other device is.
	if fresh, err := s.store.UserByID(ctx, u.ID); err == nil {
		if err := s.startSession(w, r, fresh); err != nil {
			slog.Error("create session", "error", err)
		}
	}
	s.audit(r, u.Username, "password.change", u.Username, "")
	s.setFlash(w, "Password changed. Any other devices you were signed in on have been signed out.")
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (s *Server) HandleRotatePassphrase(w http.ResponseWriter, r *http.Request) {
	u, show, ok := s.accountAction(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// The password is required here even though the user is already signed in:
	// this hands out a credential that can reset the account, so it should cost
	// more than an unattended browser.
	if err := auth.Verify(s.pepper, u.PasswordHash, r.PostFormValue("current_password")); err != nil {
		show(http.StatusUnauthorized, "Your current password is not correct.")
		return
	}
	phrase, err := auth.GeneratePassphrase()
	if err != nil {
		slog.Error("generate passphrase", "error", err)
		show(http.StatusInternalServerError, "Could not generate a new passphrase.")
		return
	}
	ppHash, err := auth.Hash(s.pepper, auth.NormalisePassphrase(phrase))
	if err != nil {
		slog.Error("hash passphrase", "error", err)
		show(http.StatusInternalServerError, "Could not generate a new passphrase.")
		return
	}
	// Keep the current password; only the passphrase changes. The existing
	// password hash is passed back in because SetCredentials writes both.
	if err := s.store.SetCredentials(ctx, u.ID, u.PasswordHash, ppHash); err != nil {
		slog.Error("set credentials", "error", err)
		show(http.StatusInternalServerError, "Could not save the new passphrase.")
		return
	}
	if fresh, err := s.store.UserByID(ctx, u.ID); err == nil {
		if err := s.startSession(w, r, fresh); err != nil {
			slog.Error("create session", "error", err)
		}
	}
	s.audit(r, u.Username, "passphrase.rotate", u.Username, "")
	s.render(w, r, "passphrase", http.StatusOK, passphraseView{
		Page:       s.page(w, r, "Your new recovery passphrase"),
		Passphrase: phrase,
		Next:       "/account",
	})
}

func (s *Server) HandleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	u, show, ok := s.accountAction(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteUserSessions(r.Context(), u.ID); err != nil {
		slog.Error("delete sessions", "error", err)
		show(http.StatusInternalServerError, "Could not sign out the other devices.")
		return
	}
	s.audit(r, u.Username, "sessions.revoke", u.Username, "")
	s.endSession(w, r)
	s.setFlash(w, "Signed out everywhere. Sign in again to continue.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// HandlePassphraseAck is the "I have saved it" button on the page that shows a
// recovery passphrase. It does nothing but move the user along — the passphrase
// was already stored when it was generated. Its value is that the page cannot
// be dismissed by accident, and that leaving it requires a deliberate click on
// a checkbox that says what was agreed to.
func (s *Server) HandlePassphraseAck(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}
	http.Redirect(w, r, s.nextForUser(r.PostFormValue("next"), u), http.StatusSeeOther)
}

func (s *Server) countSessions(r *http.Request, userID int64) int {
	var n int
	err := s.store.DB().QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM sessions WHERE user_id = ? AND expires_at > ?`,
		userID, nowString()).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

// HandleHealthz is the gateway's own liveness endpoint. It deliberately
// answers in the same shape the apps do, because their "test connection"
// button calls /healthz at the origin root and finding this is a correct
// answer rather than a confusing one.
func (s *Server) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// HandleRobots keeps a self-hosted control panel out of any crawler that
// wanders in.
func (s *Server) HandleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
}
