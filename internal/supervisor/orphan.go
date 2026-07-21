// Reaping orphaned children from a previous run.
//
// The supervisor stops every child it started when it shuts down cleanly. It
// cannot do that when it is killed outright — SIGKILL, `Stop-Process -Force`,
// a power cut — and the children are ordinary processes with no parent-death
// link, so they carry on running. Discovered the hard way: after a few
// force-killed test runs there were seven abandoned readerr and workoutt
// processes still holding their databases open.
//
// That is not merely untidy. The next boot would start a SECOND process for
// the same (user, app) pointing at the same SQLite file, and while WAL mode
// keeps the file intact, both apps track a single global `sync_state.last_seq`
// row and neither expects another writer. Two of them racing on that counter
// is exactly the corruption-of-meaning that per-tenant processes exist to
// avoid.
//
// So each instance records its child's PID and start time beside its database,
// and startup reaps anything left over. The start time is what makes this safe:
// a bare PID can be recycled by the operating system, and killing a stranger
// because it inherited a number would be far worse than leaving an orphan
// alive. If the PID is live but its start time does not match what we recorded,
// it is somebody else's process and we leave it alone.
package supervisor

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// pidFileName sits next to the instance database, so it is scoped to exactly
// the (user, app) pair it describes and needs no central registry.
const pidFileName = "instance.pid"

type pidRecord struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	StartedAt time.Time `json:"started_at"`
	Binary    string    `json:"binary"`
}

func pidPath(dir string) string { return filepath.Join(dir, pidFileName) }

// writePIDFile records a freshly started child.
func writePIDFile(dir string, rec pidRecord) {
	blob, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.WriteFile(pidPath(dir), blob, 0o600); err != nil {
		slog.Warn("could not record instance pid", "dir", dir, "error", err)
	}
}

func removePIDFile(dir string) {
	if err := os.Remove(pidPath(dir)); err != nil && !os.IsNotExist(err) {
		slog.Warn("could not remove instance pid file", "dir", dir, "error", err)
	}
}

func readPIDFile(dir string) (pidRecord, bool) {
	blob, err := os.ReadFile(pidPath(dir))
	if err != nil {
		return pidRecord{}, false
	}
	var rec pidRecord
	if err := json.Unmarshal(blob, &rec); err != nil || rec.PID <= 0 {
		return pidRecord{}, false
	}
	return rec, true
}

// ReapOrphans kills leftover children from a previous run of the gateway.
//
// Call it once at startup, before serving. It walks every instance directory
// under the data root, and for each stale PID file checks whether that process
// is both alive and actually the child we started before killing it.
func (s *Supervisor) ReapOrphans() {
	root := filepath.Join(s.cfg.InstanceDir("", ""))
	entries, err := os.ReadDir(root)
	if err != nil {
		return // no instances yet; nothing to reap
	}
	var reaped, skipped int
	for _, userDir := range entries {
		if !userDir.IsDir() {
			continue
		}
		apps, err := os.ReadDir(filepath.Join(root, userDir.Name()))
		if err != nil {
			continue
		}
		for _, appDir := range apps {
			if !appDir.IsDir() {
				continue
			}
			dir := filepath.Join(root, userDir.Name(), appDir.Name())
			rec, ok := readPIDFile(dir)
			if !ok {
				continue
			}
			switch killOrphan(rec) {
			case orphanKilled:
				reaped++
				slog.Warn("killed an orphaned app process left by a previous run",
					"user", userDir.Name(), "app", appDir.Name(), "pid", rec.PID)
			case orphanNotOurs:
				skipped++
				slog.Debug("stale pid file points at somebody else's process; leaving it alone",
					"user", userDir.Name(), "app", appDir.Name(), "pid", rec.PID)
			}
			removePIDFile(dir)
		}
	}
	if reaped > 0 || skipped > 0 {
		slog.Info("reaped orphaned instances", "killed", reaped, "skipped", skipped)
	}
}

type orphanResult int

const (
	orphanGone orphanResult = iota
	orphanKilled
	orphanNotOurs
)
