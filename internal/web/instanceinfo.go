package web

import (
	"fmt"
	"os"
	"path/filepath"

	"local-multiplexer/internal/config"
	"local-multiplexer/internal/gateway"
)

// appIcon returns the filename of an app's own icon inside its build, or "".
//
// Resolved once at startup rather than per request: the answer only changes
// when muxbuild runs, and the dashboard would otherwise stat several paths per
// card per page load to learn something that has not changed since boot.
func (s *Server) appIcon(name string) string { return s.icons[name] }

func (s *Server) resolveIcons() {
	s.icons = make(map[string]string, len(s.cfg.Apps))
	for i := range s.cfg.Apps {
		app := &s.cfg.Apps[i]
		if icon := gateway.FindIcon(s.cfg, app); icon != "" {
			s.icons[app.Name] = icon
		}
	}
}

// instanceDBBytes is the on-disk size of one instance's database.
//
// It sums the main file with its -wal and -shm siblings, which is the number
// that matches what a person sees in a file manager and what they need if they
// are wondering whether to export. Reporting only the main file would be
// misleading on a freshly-migrated database, where the schema is still entirely
// in the WAL and the main file is 4 KB.
//
// A missing file is zero, not an error: an app that has been added but never
// opened genuinely has no database yet, and saying "0 bytes" is a truer answer
// than an error message.
func instanceDBBytes(cfg *config.Config, username string, app *config.App) int64 {
	if app.Kind != config.KindSync || app.DBFile == "" {
		return 0
	}
	base := filepath.Join(cfg.InstanceDir(username, app.Name), app.DBFile)
	var total int64
	for _, p := range []string{base, base + "-wal", base + "-shm"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			total += fi.Size()
		}
	}
	return total
}

// humanBytes formats a size the way a person reads it. Deliberately coarse:
// "4.4 MB" answers "is this big?" and "4,613,734 bytes" does not.
func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "empty"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	}
	const unit = 1024
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	val := float64(n) / float64(div)
	format := "%.1f %cB"
	if val >= 100 {
		format = "%.0f %cB"
	}
	return fmt.Sprintf(format, val, "KMGT"[exp])
}
