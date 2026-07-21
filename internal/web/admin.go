// The administrator's view: who exists, what they have set up, what is
// running, and the handful of actions worth exposing.
//
// The guiding line for what belongs here is that an admin should be able to
// keep the server working and help someone who is locked out, without being
// able to quietly read anybody's data as a matter of course. Exporting a
// user's database is on the list because "give me my data" is a request the
// admin has to be able to answer; opening someone's app as them is not, unless
// the operator turns on allow_admin_impersonation deliberately.
package web

import (
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"muxerr/internal/auth"
	"muxerr/internal/store"
)

type adminUser struct {
	ID         int64
	Username   string
	IsAdmin    bool
	IsDisabled bool
	CreatedAt  string
	LastLogin  string
	Apps       []string
}

type adminRunning struct {
	Username  string
	App       string
	PID       int
	Port      int
	StartedAt string
}

type adminAudit struct {
	At     string
	Actor  string
	Action string
	Target string
	Detail string
}

type adminApp struct {
	Name  string
	Title string
}

type adminView struct {
	Page           PageData
	Users          []adminUser
	Running        []adminRunning
	SignupsEnabled bool
	Audit          []adminAudit
	Apps           []adminApp
}

func (s *Server) HandleAdmin(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	ctx := r.Context()

	users, err := s.store.ListUsers(ctx)
	if err != nil {
		slog.Error("list users", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Could not load the user list.")
		return
	}
	instances, err := s.store.AllInstances(ctx)
	if err != nil {
		slog.Error("list instances", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Could not load the instance list.")
		return
	}
	byUser := map[int64][]string{}
	for _, in := range instances {
		byUser[in.UserID] = append(byUser[in.UserID], in.App)
	}

	v := adminView{
		Page:           s.page(w, r, "Administration"),
		SignupsEnabled: s.signupsOpen(ctx),
	}
	for _, u := range users {
		apps := byUser[u.ID]
		sort.Strings(apps)
		au := adminUser{
			ID: u.ID, Username: u.Username,
			IsAdmin: u.IsAdmin, IsDisabled: u.IsDisabled,
			CreatedAt: u.CreatedAt.Local().Format("2006-01-02"),
			Apps:      apps,
		}
		if u.LastLoginAt != nil {
			au.LastLogin = u.LastLoginAt.Local().Format("2006-01-02 15:04")
		}
		v.Users = append(v.Users, au)
	}
	for _, st := range s.sup.Running() {
		v.Running = append(v.Running, adminRunning{
			Username: st.Username, App: st.App, PID: st.PID, Port: st.Port,
			StartedAt: st.StartedAt.Local().Format("2006-01-02 15:04"),
		})
	}
	sort.Slice(v.Running, func(i, j int) bool {
		if v.Running[i].Username != v.Running[j].Username {
			return v.Running[i].Username < v.Running[j].Username
		}
		return v.Running[i].App < v.Running[j].App
	})
	for i := range s.cfg.Apps {
		v.Apps = append(v.Apps, adminApp{Name: s.cfg.Apps[i].Name, Title: s.cfg.Apps[i].Title})
	}
	if entries, err := s.store.RecentAudit(ctx, 40); err == nil {
		for _, e := range entries {
			v.Audit = append(v.Audit, adminAudit{
				At:    e.At.Local().Format("2006-01-02 15:04"),
				Actor: e.Actor, Action: e.Action, Target: e.Target, Detail: e.Detail,
			})
		}
	}
	s.render(w, r, "admin", http.StatusOK, v)
}

// adminTarget resolves the {id} path value to a user, rejecting the request if
// it is missing or unknown.
func (s *Server) adminTarget(w http.ResponseWriter, r *http.Request) (*adminActionCtx, bool) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return nil, false
	}
	if !s.checkCSRF(w, r) {
		return nil, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "That is not a valid user id.")
		return nil, false
	}
	target, err := s.store.UserByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "No such user.")
		return nil, false
	}
	return &adminActionCtx{admin: admin.Username, adminID: admin.ID, target: target}, true
}

type adminActionCtx struct {
	admin   string
	adminID int64
	target  *store.User
}

// The admin actions are separate handlers rather than one switch, so that each
// is its own URL and the audit trail records what was actually asked for
// rather than a form field that could have been anything.

func (s *Server) HandleAdminSetDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.adminTarget(w, r)
		if !ok {
			return
		}
		if disabled && c.target.ID == c.adminID {
			s.setFlash(w, "You cannot disable your own account.")
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		if disabled && c.target.IsAdmin && s.lastEnabledAdmin(r, c.target.ID) {
			s.setFlash(w, "That is the only administrator left — make someone else an admin first.")
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		if err := s.store.SetDisabled(r.Context(), c.target.ID, disabled); err != nil {
			slog.Error("set disabled", "error", err)
			s.fail(w, r, http.StatusInternalServerError, "Could not update that account.")
			return
		}
		if disabled {
			// Their processes have no business still running.
			for _, st := range s.sup.Running() {
				if st.Username == c.target.Username {
					_ = s.sup.Stop(st.Username, st.App)
				}
			}
		}
		action := "user.enable"
		if disabled {
			action = "user.disable"
		}
		s.audit(r, c.admin, action, c.target.Username, "")
		s.setFlash(w, c.target.Username+map[bool]string{true: " is disabled.", false: " is enabled."}[disabled])
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
}

func (s *Server) HandleAdminToggleAdmin(w http.ResponseWriter, r *http.Request) {
	c, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	makeAdmin := !c.target.IsAdmin
	if !makeAdmin && s.lastEnabledAdmin(r, c.target.ID) {
		s.setFlash(w, "That is the only administrator left — promote someone else first.")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if err := s.store.SetAdmin(r.Context(), c.target.ID, makeAdmin); err != nil {
		slog.Error("set admin", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Could not update that account.")
		return
	}
	s.audit(r, c.admin, map[bool]string{true: "user.promote", false: "user.demote"}[makeAdmin], c.target.Username, "")
	s.setFlash(w, c.target.Username+map[bool]string{true: " is now an administrator.", false: " is no longer an administrator."}[makeAdmin])
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// HandleAdminReset is the last resort for a user who has lost both their
// password and their passphrase. It mints a new passphrase and shows it once —
// the admin then has to convey it to the user out of band, and the user uses
// the normal self-serve reset with it. The admin never sets a password on
// somebody's behalf, so there is no moment where an admin knows a working
// password for another account.
func (s *Server) HandleAdminReset(w http.ResponseWriter, r *http.Request) {
	c, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	phrase, err := auth.GeneratePassphrase()
	if err != nil {
		slog.Error("generate passphrase", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Could not generate a passphrase.")
		return
	}
	ppHash, err := auth.Hash(s.pepper, auth.NormalisePassphrase(phrase))
	if err != nil {
		slog.Error("hash passphrase", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Could not generate a passphrase.")
		return
	}
	// A random unusable password: the account can only be entered through the
	// reset flow with the new passphrase, so an admin-triggered reset cannot
	// leave the admin holding working credentials.
	dead, err := auth.NewSessionToken()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Could not reset that account.")
		return
	}
	pwHash, err := auth.Hash(s.pepper, dead)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Could not reset that account.")
		return
	}
	if err := s.store.SetCredentials(r.Context(), c.target.ID, pwHash, ppHash); err != nil {
		slog.Error("set credentials", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Could not reset that account.")
		return
	}
	s.audit(r, c.admin, "user.reset", c.target.Username, "admin issued a new passphrase")
	slog.Info("admin reset account", "admin", c.admin, "user", c.target.Username)

	s.render(w, r, "passphrase", http.StatusOK, passphraseView{
		Page:       s.page(w, r, "New passphrase for "+c.target.Username),
		Passphrase: phrase,
		Next:       "/admin",
	})
}

// HandleAdminDeleteUser removes an account. On-disk instance data is left
// alone: a mis-click here should not be the thing that destroys somebody's
// years of notes. The flash says so, and the path is printed in the log so it
// can be cleaned up deliberately.
func (s *Server) HandleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	c, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	if c.target.ID == c.adminID {
		s.setFlash(w, "You cannot delete your own account.")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if c.target.IsAdmin && s.lastEnabledAdmin(r, c.target.ID) {
		s.setFlash(w, "That is the only other administrator — promote someone else first.")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	for _, st := range s.sup.Running() {
		if st.Username == c.target.Username {
			_ = s.sup.Stop(st.Username, st.App)
		}
	}
	if err := s.store.DeleteUser(r.Context(), c.target.ID); err != nil {
		slog.Error("delete user", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Could not delete that account.")
		return
	}
	dir := s.cfg.InstanceDir(c.target.Username, "")
	s.audit(r, c.admin, "user.delete", c.target.Username, "data kept at "+dir)
	slog.Warn("account deleted; instance data left in place", "user", c.target.Username, "dir", dir)
	s.setFlash(w, c.target.Username+" is deleted. Their app data is still on disk at "+dir+" — remove it by hand if you mean to.")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// lastEnabledAdmin guards against locking everyone out of administration.
func (s *Server) lastEnabledAdmin(r *http.Request, exceptID int64) bool {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		return true // if we cannot tell, refuse the destructive option
	}
	for _, u := range users {
		if u.ID != exceptID && u.IsAdmin && !u.IsDisabled {
			return false
		}
	}
	return true
}

// HandleAdminStopInstance stops a running child. Useful when an app is wedged
// or after changing its configuration; the next request starts a fresh one.
func (s *Server) HandleAdminStopInstance(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}
	username, appName := r.PathValue("user"), r.PathValue("app")
	if err := s.sup.Stop(username, appName); err != nil {
		slog.Warn("stop instance", "user", username, "app", appName, "error", err)
	}
	s.audit(r, admin.Username, "instance.stop", username+"/"+appName, "")
	s.setFlash(w, "Stopped "+username+"/"+appName+".")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// HandleAdminExport downloads a consistent snapshot of a user's app database.
func (s *Server) HandleAdminExport(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	username, appName := r.PathValue("user"), r.PathValue("app")
	app, ok := s.cfg.App(appName)
	if !ok {
		s.fail(w, r, http.StatusNotFound, "No such app.")
		return
	}
	target, err := s.store.UserByName(r.Context(), username)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "No such user.")
		return
	}
	has, err := s.store.HasInstance(r.Context(), target.ID, app.Name)
	if err != nil || !has {
		s.fail(w, r, http.StatusNotFound, "That user has not set up this app.")
		return
	}
	// Reading somebody else's data is exactly the kind of thing that should
	// leave a mark, whatever the reason for it.
	s.audit(r, admin.Username, "export.admin", username+"/"+app.Name, "")
	slog.Info("admin exported a user database", "admin", admin.Username, "user", username, "app", app.Name)
	s.exporter.ExportDB(w, r, username, app)
}

// HandleAdminSignups opens or closes registration at runtime, so the operator
// does not have to edit apps.json and restart to stop new sign-ups.
func (s *Server) HandleAdminSignups(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}
	enable := r.PostFormValue("enabled") == "true"
	if err := s.store.SetSetting(r.Context(), SettingSignups, strconv.FormatBool(enable)); err != nil {
		slog.Error("set signups", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Could not change that setting.")
		return
	}
	s.audit(r, admin.Username, "settings.signups", "", strconv.FormatBool(enable))
	s.setFlash(w, map[bool]string{true: "Sign-ups are open.", false: "Sign-ups are closed."}[enable])
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func nowString() string { return time.Now().UTC().Format(time.RFC3339Nano) }
