// Exporting a user's app database.
//
// The apps expose their own /backup, but it works by running
// `PRAGMA wal_checkpoint(TRUNCATE)` and then http.ServeFile on the live
// database. That has a real race: once the checkpoint returns, any concurrent
// write goes to a fresh WAL and SQLite's automatic checkpoint can rewrite the
// main file part-way through the download, so a busy instance can hand back a
// file spliced from two different points in time. ServeFile also advertises
// byte ranges, so a resumed download can do the same thing on purpose.
//
// The gateway does not need to go through the app at all: it knows where the
// file is. `VACUUM INTO` produces a consistent, fully-checkpointed snapshot of
// the database as of one transaction, without blocking writers, and it works
// whether or not the instance is currently running. That is the correct
// primitive for "give this user a copy of their data", so that is what this
// uses.
package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"local-multiplexer/internal/config"

	_ "modernc.org/sqlite"
)

// ExportDB streams a consistent snapshot of one instance's database.
//
// The caller is responsible for having checked that the requester is allowed
// to do this — the admin handlers do, and nothing else calls it.
func (g *Gateway) ExportDB(w http.ResponseWriter, r *http.Request, username string, app *config.App) {
	src := filepath.Join(g.cfg.InstanceDir(username, app.Name), app.DBFile)
	if _, err := os.Stat(src); err != nil {
		writeGatewayError(w, r, http.StatusNotFound,
			"That instance has no database yet — the app has never been opened.")
		return
	}

	tmpDir, err := os.MkdirTemp("", "mux-export-")
	if err != nil {
		writeGatewayError(w, r, http.StatusInternalServerError, "Could not prepare the export.")
		return
	}
	defer os.RemoveAll(tmpDir)
	snapshot := filepath.Join(tmpDir, app.Name+".db")

	if err := vacuumInto(r.Context(), src, snapshot); err != nil {
		writeGatewayError(w, r, http.StatusInternalServerError, "Could not read the database: "+err.Error())
		return
	}

	f, err := os.Open(snapshot)
	if err != nil {
		writeGatewayError(w, r, http.StatusInternalServerError, "Could not open the export.")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeGatewayError(w, r, http.StatusInternalServerError, "Could not size the export.")
		return
	}

	name := fmt.Sprintf("%s-%s-%s.db", sanitiseFilename(username), sanitiseFilename(app.Name),
		time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", fmt.Sprint(fi.Size()))
	w.Header().Set("Cache-Control", "no-store")
	// ServeContent would add range support against a file we are about to
	// delete; a plain copy is what we want.
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// vacuumInto writes a consistent copy of src to dst. dst must not exist.
func vacuumInto(ctx context.Context, src, dst string) error {
	// Open read-only so an export can never be the thing that corrupts the
	// database it is trying to preserve. The instance's own process may well
	// have it open for writing at the same time; WAL mode makes that fine.
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	return nil
}

// sanitiseFilename keeps a Content-Disposition filename to characters that
// cannot break out of the quoted string or confuse a filesystem.
func sanitiseFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "export"
	}
	return out
}
