// The live SQL console: arbitrary statements against a real instance database,
// with no undo and no safety net.
//
// This is the dangerous half of the pair. Its sibling at /tools/sqlite loads a
// *snapshot* into the browser, where nothing anyone types can reach the server;
// that is what most people want and it is always available. This one writes to
// the actual file the app reads, which is occasionally the only way to fix
// something and is otherwise a very efficient way to lose your data.
//
// Three things guard it, and each exists for a different failure:
//
//   - It is off unless apps.json says `"sql_console": true`. On a server with
//     open sign-ups, shipping this on by default would hand a data-destruction
//     tool to every new account. That decision belongs in a file the operator
//     edits, not behind an admin checkbox somebody can reach by accident.
//   - Every visit re-gates. You type the full warning sentence to unlock, and
//     the unlock is short-lived and single-page. Navigating away and back makes
//     you type it again — deliberately, because the risk is in forgetting which
//     tab you left open, not in mistyping.
//   - The instance is stopped before anything runs. Two writers on one SQLite
//     file, one of them altering the schema underneath a running app, is a way
//     to corrupt something that WAL will not save you from.
package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"muxerr/internal/config"
	"muxerr/internal/store"
)

// ConsolePhrase is what has to be typed, exactly, to unlock the console.
//
// It is long and specific on purpose. A short "yes" is muscle memory within a
// day; a sentence naming the actual consequence has to be read at least once,
// and typing it is a moment in which somebody can notice they are on the wrong
// database.
const ConsolePhrase = "I understand that using this may permanently and irrevocably break my database with no way to retrieve a backup"

// maxConsoleRows caps what one statement returns to the page. A SELECT * on a
// large table should not try to render a million rows into HTML.
const maxConsoleRows = 500

type consoleView struct {
	Page PageData
	// Stage is "locked" (show the warning and the phrase field) or "unlocked".
	Stage   string
	Phrase  string
	Token   string
	Apps    []consoleApp
	App     string
	Owner   string
	SQL     string
	Running bool
	// Result is filled after a successful execution.
	Result   *consoleResult
	SQLError string
	IsAdmin  bool
}

type consoleApp struct {
	Name  string
	Title string
	Owner string
	Size  string
}

type consoleResult struct {
	Columns      []string
	Rows         [][]string
	Truncated    bool
	RowsAffected int64
	Statements   int
	ElapsedMS    int64
	Stopped      bool
	Notice       string
}

// HandleSQLConsole renders the gate, or the console once unlocked.
func (s *Server) HandleSQLConsole(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if !s.cfg.Site.SQLConsole {
		s.fail(w, r, http.StatusNotFound,
			"The live SQL console is switched off on this server. An administrator can enable it "+
				`by setting "sql_console": true in apps.json.`)
		return
	}
	// Always arrive locked. The token only exists on the page you just
	// unlocked, so following a link or reopening a tab starts over.
	v := s.consoleBase(w, r, u)
	v.Stage = "locked"
	v.Phrase = ConsolePhrase
	v.App = r.URL.Query().Get("app")
	s.render(w, r, "sqlconsole", http.StatusOK, v)
}

// HandleSQLConsoleUnlock checks the typed phrase and issues a page token.
func (s *Server) HandleSQLConsoleUnlock(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if !s.cfg.Site.SQLConsole {
		s.fail(w, r, http.StatusNotFound, "The live SQL console is switched off on this server.")
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}

	v := s.consoleBase(w, r, u)
	v.App = r.PostFormValue("app")

	// Compared after collapsing whitespace, because the phrase is long enough
	// that a copy-paste picks up a line break and a person retyping it adds a
	// double space. Neither is a misunderstanding of the warning.
	if !phraseMatches(r.PostFormValue("phrase")) {
		v.Stage = "locked"
		v.Phrase = ConsolePhrase
		v.Page.Error = "That did not match. Type the sentence exactly as it is shown."
		s.audit(r, u.Username, "sqlconsole.unlock.failed", "", "")
		s.render(w, r, "sqlconsole", http.StatusBadRequest, v)
		return
	}

	s.audit(r, u.Username, "sqlconsole.unlock", "", "")
	v.Stage = "unlocked"
	v.Token = s.consoleTokens.issue(u.ID, u.Username)
	v.Running = s.appIsRunning(v.Owner, v.App)
	s.render(w, r, "sqlconsole", http.StatusOK, v)
}

// HandleSQLConsoleExecute runs the statements. This is the one place in the
// gateway that writes to an app's database.
func (s *Server) HandleSQLConsoleExecute(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if !s.cfg.Site.SQLConsole {
		s.fail(w, r, http.StatusNotFound, "The live SQL console is switched off on this server.")
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}

	v := s.consoleBase(w, r, u)
	v.App = r.PostFormValue("app")
	v.SQL = r.PostFormValue("sql")
	v.Stage = "unlocked"

	// The unlock token is consumed and reissued on every execution, so a
	// replayed form post cannot run the same destructive statement twice.
	if _, ok := s.consoleTokens.consume(r.PostFormValue("token"), u.Username); !ok {
		v.Stage = "locked"
		v.Phrase = ConsolePhrase
		v.Page.Error = "That console session expired. Read the warning and unlock again."
		s.render(w, r, "sqlconsole", http.StatusBadRequest, v)
		return
	}
	v.Token = s.consoleTokens.issue(u.ID, u.Username)

	owner, app, err := s.resolveConsoleTarget(r, u, v.App)
	if err != nil {
		v.SQLError = err.Error()
		s.render(w, r, "sqlconsole", http.StatusBadRequest, v)
		return
	}
	v.Owner = owner
	v.App = app.Name

	if strings.TrimSpace(v.SQL) == "" {
		v.SQLError = "Nothing to run."
		s.render(w, r, "sqlconsole", http.StatusBadRequest, v)
		return
	}

	dbPath := filepath.Join(s.cfg.InstanceDir(owner, app.Name), app.DBFile)

	// Stop the child first. Two processes writing one SQLite file is survivable
	// until one of them changes the schema the other has cached, and the app
	// will start again on the owner's next request anyway.
	stopped := false
	if _, running := s.sup.Get(owner, app.Name); running {
		if err := s.sup.Stop(owner, app.Name); err != nil {
			slog.Warn("stopping instance for SQL console", "user", owner, "app", app.Name, "error", err)
		} else {
			stopped = true
		}
	}

	s.audit(r, u.Username, "sqlconsole.execute", owner+"/"+app.Name, firstLine(v.SQL))
	slog.Warn("live SQL executed against an instance database",
		"actor", u.Username, "user", owner, "app", app.Name, "bytes", len(v.SQL))

	res, err := executeLiveSQL(r.Context(), dbPath, v.SQL)
	if err != nil {
		v.SQLError = err.Error()
		v.Result = &consoleResult{Stopped: stopped}
		s.render(w, r, "sqlconsole", http.StatusOK, v)
		return
	}
	res.Stopped = stopped
	if stopped {
		res.Notice = app.Title + " was stopped to run this. It starts again on the next request."
	}
	v.Result = res
	v.Running = false
	s.render(w, r, "sqlconsole", http.StatusOK, v)
}

// resolveConsoleTarget works out whose database is being written to and checks
// the caller is allowed to.
func (s *Server) resolveConsoleTarget(r *http.Request, caller *store.User, appName string) (string, *config.App, error) {
	owner := caller.Username
	// "user/app" addresses somebody else's instance and is admin-only.
	if u, a, ok := strings.Cut(appName, "/"); ok {
		if !caller.IsAdmin {
			return "", nil, errors.New("that is another user's database")
		}
		owner, appName = NormaliseUsername(u), a
	}
	app, ok := s.cfg.App(appName)
	if !ok {
		return "", nil, errors.New("no such app")
	}
	if app.Kind != config.KindSync {
		return "", nil, errors.New(app.Title + " has no database")
	}
	target, err := s.store.UserByName(r.Context(), owner)
	if err != nil {
		return "", nil, errors.New("no such user")
	}
	has, err := s.store.HasInstance(r.Context(), target.ID, app.Name)
	if err != nil || !has {
		return "", nil, errors.New("that app is not set up for " + owner)
	}
	return owner, app, nil
}

// executeLiveSQL opens the database read-write and runs everything in the box.
//
// One connection, no transaction wrapper: the caller may well be running BEGIN
// and COMMIT themselves, or a PRAGMA that cannot run inside a transaction, and
// silently wrapping their statements would make the console lie about what it
// did.
func executeLiveSQL(ctx context.Context, dbPath, script string) (*consoleResult, error) {
	dsn := "file:" + dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	start := time.Now()
	statements := splitStatements(script)
	res := &consoleResult{Statements: len(statements)}

	for _, stmt := range statements {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		// Try it as a query first. A statement that returns no columns — an
		// INSERT, a CREATE — comes back with an empty column set, and is then
		// re-run through Exec so the affected-rows count is real. Guessing
		// from the leading keyword instead would get CTEs and RETURNING wrong.
		rows, qerr := db.QueryContext(ctx, stmt)
		if qerr != nil {
			return nil, fmt.Errorf("%s: %w", strings.TrimSpace(firstLine(stmt)), qerr)
		}
		cols, cerr := rows.Columns()
		if cerr != nil {
			rows.Close()
			return nil, cerr
		}
		if len(cols) == 0 {
			rows.Close()
			out, eerr := db.ExecContext(ctx, stmt)
			if eerr != nil {
				return nil, fmt.Errorf("%s: %w", strings.TrimSpace(firstLine(stmt)), eerr)
			}
			if n, err := out.RowsAffected(); err == nil {
				res.RowsAffected += n
			}
			continue
		}
		collected, truncated, rerr := scanRows(rows, cols)
		rows.Close()
		if rerr != nil {
			return nil, rerr
		}
		// The last statement that produced a result set is the one shown,
		// which matches what every SQL shell does with a multi-statement paste.
		res.Columns, res.Rows, res.Truncated = cols, collected, truncated
	}

	res.ElapsedMS = time.Since(start).Milliseconds()
	return res, nil
}

func scanRows(rows *sql.Rows, cols []string) ([][]string, bool, error) {
	var out [][]string
	truncated := false
	for rows.Next() {
		if len(out) >= maxConsoleRows {
			truncated = true
			break
		}
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, false, err
		}
		row := make([]string, len(cols))
		for i, v := range raw {
			row[i] = renderCell(v)
		}
		out = append(out, row)
	}
	return out, truncated, rows.Err()
}

// renderCell formats a value for a table cell. NULL is rendered as the marker
// "∅" rather than the word, because a column full of the string 'NULL' and a
// column full of actual nulls are different problems and the page should not
// make them look the same.
func renderCell(v any) string {
	switch t := v.(type) {
	case nil:
		return "∅"
	case []byte:
		if len(t) > 64 {
			return fmt.Sprintf("<%d bytes>", len(t))
		}
		return fmt.Sprintf("x'%x'", t)
	case time.Time:
		return t.Format(time.RFC3339)
	default:
		return fmt.Sprint(v)
	}
}

// splitStatements breaks a script on semicolons that are not inside a string
// literal or a comment.
//
// This is not a SQL parser and does not need to be: it only has to find
// statement boundaries, and the cases that actually appear in a console — a
// semicolon inside a quoted default value, a trailing comment — are exactly the
// ones a naive strings.Split gets wrong. BEGIN...END blocks in a trigger body
// are the known limitation; run those one at a time.
func splitStatements(script string) []string {
	var out []string
	var cur strings.Builder
	runes := []rune(script)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			quote := c
			cur.WriteRune(c)
			for i++; i < len(runes); i++ {
				cur.WriteRune(runes[i])
				if runes[i] == quote {
					// A doubled quote is an escaped quote, not the end.
					if i+1 < len(runes) && runes[i+1] == quote {
						i++
						cur.WriteRune(runes[i])
						continue
					}
					break
				}
			}
		case c == '-' && i+1 < len(runes) && runes[i+1] == '-':
			for ; i < len(runes) && runes[i] != '\n'; i++ {
				cur.WriteRune(runes[i])
			}
			if i < len(runes) {
				cur.WriteRune('\n')
			}
		case c == '/' && i+1 < len(runes) && runes[i+1] == '*':
			cur.WriteString("/*")
			i += 2
			for ; i < len(runes); i++ {
				if runes[i] == '*' && i+1 < len(runes) && runes[i+1] == '/' {
					cur.WriteString("*/")
					i++
					break
				}
				cur.WriteRune(runes[i])
			}
		case c == ';':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

// phraseMatches compares the typed phrase after collapsing runs of whitespace,
// so a pasted line break or a doubled space is not treated as a failure to
// understand the warning.
func phraseMatches(typed string) bool {
	return collapseSpace(typed) == collapseSpace(ConsolePhrase)
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func (s *Server) appIsRunning(owner, app string) bool {
	if owner == "" || app == "" {
		return false
	}
	_, running := s.sup.Get(owner, app)
	return running
}
