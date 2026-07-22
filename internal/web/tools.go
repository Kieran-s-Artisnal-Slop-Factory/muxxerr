// The two database tools, and what they have in common.
//
// /tools/sqlite loads a *snapshot* into the browser. SQLite compiled to
// WebAssembly runs the queries client-side, so nothing typed there can reach
// the server. It is always available.
//
// /tools/sql runs statements against the *live* database, and is off unless
// apps.json enables it. See sqlconsole.go.
//
// The split is the whole design. "Let me look at my data" and "let me change my
// data" are different requests with wildly different consequences, and giving
// them one door with a checkbox on it would mean the safe operation carries the
// dangerous one's warnings until people stop reading them.
package web

import (
	"net/http"
	"sort"

	"muxxerr/internal/config"
	"muxxerr/internal/store"
)

type viewerApp struct {
	Name  string
	Title string
	URL   string
}

type sqliteView struct {
	Page         PageData
	Apps         []viewerApp
	PreselectApp string
	SQLConsole   bool
}

// HandleSQLiteViewer renders the snapshot viewer shell. Everything interesting
// happens in the browser; this handler's only job is to say which databases the
// caller is allowed to pull a snapshot of.
func (s *Server) HandleSQLiteViewer(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	v := sqliteView{
		Page:         s.page(w, r, "SQLite viewer"),
		Apps:         s.snapshotSources(r, u),
		PreselectApp: r.URL.Query().Get("app"),
		SQLConsole:   s.cfg.Site.SQLConsole,
	}
	v.Page.ExtraCSS = []string{"tools.css"}
	s.render(w, r, "sqlite", http.StatusOK, v)
}

// snapshotSources lists the databases this user may load into the viewer: their
// own instances, plus everyone's if they are an admin.
//
// The URLs point at the existing export endpoints, which produce a consistent
// snapshot with VACUUM INTO rather than copying a file out from under a running
// writer. Reusing them means there is exactly one way to get a database out of
// this gateway, and it is the correct one.
func (s *Server) snapshotSources(r *http.Request, u *store.User) []viewerApp {
	ctx := r.Context()
	out := []viewerApp{}

	mine, err := s.store.UserInstances(ctx, u.ID)
	if err == nil {
		for _, in := range mine {
			app, ok := s.cfg.App(in.App)
			if !ok || app.Kind != config.KindSync {
				continue
			}
			out = append(out, viewerApp{
				Name:  app.Name,
				Title: app.Title,
				URL:   "/apps/" + app.Name + "/export",
			})
		}
	}
	if !u.IsAdmin {
		return out
	}

	all, err := s.store.AllInstances(ctx)
	if err != nil {
		return out
	}
	for _, in := range all {
		if in.UserID == u.ID {
			continue // already listed, without the username prefix
		}
		app, ok := s.cfg.App(in.App)
		if !ok || app.Kind != config.KindSync {
			continue
		}
		out = append(out, viewerApp{
			Name:  in.Username + "/" + app.Name,
			Title: app.Title + " — " + in.Username,
			URL:   "/admin/instances/" + in.Username + "/" + app.Name + "/export",
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out
}

// consoleBase fills the parts of the console view that do not depend on which
// stage the page is in.
func (s *Server) consoleBase(w http.ResponseWriter, r *http.Request, u *store.User) consoleView {
	v := consoleView{
		Page:    s.page(w, r, "SQL console"),
		Owner:   u.Username,
		IsAdmin: u.IsAdmin,
		Apps:    s.consoleTargets(r, u),
	}
	v.Page.ExtraCSS = []string{"tools.css"}
	return v
}

// consoleTargets is the same list as the viewer's, in the console's shape.
func (s *Server) consoleTargets(r *http.Request, u *store.User) []consoleApp {
	out := []consoleApp{}
	for _, src := range s.snapshotSources(r, u) {
		owner := u.Username
		name := src.Name
		if o, a, ok := splitOwnerApp(src.Name); ok {
			owner, name = o, a
		}
		app, ok := s.cfg.App(name)
		if !ok {
			continue
		}
		out = append(out, consoleApp{
			Name:  src.Name,
			Title: src.Title,
			Owner: owner,
			Size:  humanBytes(instanceDBBytes(s.cfg, owner, app)),
		})
	}
	return out
}

func splitOwnerApp(v string) (string, string, bool) {
	for i := 0; i < len(v); i++ {
		if v[i] == '/' {
			return v[:i], v[i+1:], true
		}
	}
	return "", v, false
}
