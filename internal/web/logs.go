// The per-instance log viewer.
//
// When an app misbehaves the useful question is "what did my instance say?",
// and until now the only answer was "ask whoever runs the server to grep their
// logs for your username". That is a poor thing to require of the person whose
// reading list will not sync, so each user can read the tail of their own
// instances' output.
//
// Scope is deliberately narrow. A user sees their own instances and nothing
// else; an admin can read anyone's, and that read is written to the audit log
// like every other time an admin looks at somebody's data. There is no
// download, no search and no history beyond what is in memory — this is for
// answering "why did that just fail", not for keeping records.
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"local-multiplexer/internal/config"
	"local-multiplexer/internal/supervisor"
)

// logTailDefault is what the page shows. The supervisor keeps rather more than
// this so that a restart mid-window still leaves a full page of context.
const logTailDefault = 50

type logEntry struct {
	At     string
	Stream string
	Text   string
	IsErr  bool
	// Level and Fields are filled when the line was structured JSON, which is
	// what all of these apps emit. Text then holds just the message.
	Level  string
	Fields string
}

// levelClass maps a slog level to the CSS modifier used to tint the row.
func levelClass(level string) string {
	switch strings.ToUpper(level) {
	case "ERROR", "FATAL":
		return "err"
	case "WARN", "WARNING":
		return "warn"
	default:
		return ""
	}
}

// formatLine turns one line of child output into something a person can read.
//
// These apps log structured JSON to stdout — `slog.New(slog.NewJSONHandler(...))`
// in every one of them — so a raw tail is a wall of escaped braces with the
// actual message buried in the middle. Parsing it back out costs nothing (the
// lines are short and there are fifty of them) and turns the page from
// technically-correct into actually useful.
//
// Anything that is not JSON is shown verbatim. That matters: a Go panic, which
// is the output you most want to read, is not JSON.
func formatLine(l supervisor.LogLine) logEntry {
	e := logEntry{
		At:     l.At.Local().Format("15:04:05"),
		Stream: l.Stream,
		Text:   l.Text,
		IsErr:  l.Stream == "stderr",
	}

	trimmed := strings.TrimSpace(l.Text)
	if !strings.HasPrefix(trimmed, "{") {
		return e
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(trimmed), &fields); err != nil {
		return e
	}
	msg, ok := fields["msg"].(string)
	if !ok {
		return e
	}
	e.Text = msg

	if lvl, ok := fields["level"].(string); ok {
		e.Level = lvl
		if c := levelClass(lvl); c == "err" {
			e.IsErr = true
		}
	}
	// The app's own timestamp is more precise than when we happened to read
	// the line, and on a busy boot they can differ by milliseconds.
	if ts, ok := fields["time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.At = t.Local().Format("15:04:05")
		}
	}

	// Everything else, in a stable order so the same log line always renders
	// the same way rather than reshuffling on each reload.
	keys := make([]string, 0, len(fields))
	for k := range fields {
		switch k {
		case "time", "level", "msg":
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%v", k, fields[k])
	}
	e.Fields = b.String()
	return e
}

type logsView struct {
	Page     PageData
	App      string
	Title    string
	Owner    string
	AppURL   string
	Running  bool
	Lines    []logEntry
	Limit    int
	Capacity int
	// ForOther is set when an admin is reading somebody else's instance, so
	// the page can say so plainly rather than looking like their own.
	ForOther bool
}

// HandleLogs shows the tail of one instance's output.
//
// The {user} segment is optional in effect: /apps/{app}/logs means "mine", and
// /admin/instances/{user}/{app}/logs means "theirs" and requires admin. Both
// land here.
func (s *Server) HandleLogs(w http.ResponseWriter, r *http.Request) {
	caller := s.requireUser(w, r)
	if caller == nil {
		return
	}
	owner := caller.Username
	forOther := false
	if u := r.PathValue("user"); u != "" && !equalFoldASCII(u, caller.Username) {
		if !caller.IsAdmin {
			s.fail(w, r, http.StatusForbidden, "Those are another user's logs.")
			return
		}
		owner = NormaliseUsername(u)
		forOther = true
	}

	app, ok := s.cfg.App(r.PathValue("app"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, "No such app.")
		return
	}
	if app.Kind != config.KindSync {
		s.fail(w, r, http.StatusNotFound,
			app.Title+" has no backend, so there is nothing for it to log.")
		return
	}

	target, err := s.store.UserByName(r.Context(), owner)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "No such user.")
		return
	}
	has, err := s.store.HasInstance(r.Context(), target.ID, app.Name)
	if err != nil || !has {
		s.fail(w, r, http.StatusNotFound, "That app is not set up.")
		return
	}
	if forOther {
		// Reading another person's logs is a look at their data, however
		// mundane, so it leaves the same mark that exporting their database
		// does.
		s.audit(r, caller.Username, "logs.admin", owner+"/"+app.Name, "")
	}

	limit := logTailDefault
	if v := r.URL.Query().Get("n"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, supervisor.LogCapacity)
		}
	}

	_, running := s.sup.Get(owner, app.Name)
	v := logsView{
		Page:     s.page(w, r, app.Title+" logs"),
		App:      app.Name,
		Title:    app.Title,
		Owner:    owner,
		AppURL:   "/" + owner + "/" + app.Name + "/",
		Running:  running,
		Limit:    limit,
		Capacity: supervisor.LogCapacity,
		ForOther: forOther,
	}
	for _, l := range s.sup.Logs(owner, app.Name, limit) {
		v.Lines = append(v.Lines, formatLine(l))
	}
	s.render(w, r, "logs", http.StatusOK, v)
}

// equalFoldASCII compares usernames, which are ASCII by construction
// (see usernameRe), without pulling in full Unicode case folding.
func equalFoldASCII(a, b string) bool {
	return NormaliseUsername(a) == NormaliseUsername(b)
}
