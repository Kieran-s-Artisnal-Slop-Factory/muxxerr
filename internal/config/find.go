package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultFile is the config filename both commands look for.
const DefaultFile = "apps.json"

// Find locates apps.json when the caller did not name one explicitly.
//
// Running `mux` from a checkout should just work, and it did — the flag already
// defaulted to "apps.json" relative to the working directory. What did not work
// was running the compiled binary from anywhere else, which is exactly what
// happens once it is installed somewhere or invoked from a service manager:
// the relative default resolved against whatever directory the process happened
// to start in, and the error was a bare "no such file".
//
// So look in the obvious places, in the order somebody would expect:
//
//  1. the working directory — the checkout case, unchanged
//  2. next to the executable — an unpacked release, or a container image
//  3. the directory above the executable — the bin/ layout
//
// and if none of them has it, say which places were tried rather than naming
// one path that was never going to exist.
func Find(explicit string) (string, error) {
	if explicit != "" && explicit != DefaultFile {
		// Named outright: no searching, and a missing file is an error rather
		// than a reason to look elsewhere. Silently loading a different config
		// than the one asked for is worse than failing.
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config %s: %w", explicit, err)
		}
		return explicit, nil
	}

	tried := make([]string, 0, 3)
	consider := func(dir string) (string, bool) {
		if dir == "" {
			return "", false
		}
		p := filepath.Join(dir, DefaultFile)
		tried = append(tried, p)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
		return "", false
	}

	if wd, err := os.Getwd(); err == nil {
		if p, ok := consider(wd); ok {
			return p, nil
		}
	}
	if exe, err := os.Executable(); err == nil {
		// Resolve symlinks so a binary linked into /usr/local/bin still finds
		// the config beside its real location.
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		dir := filepath.Dir(exe)
		if p, ok := consider(dir); ok {
			return p, nil
		}
		if p, ok := consider(filepath.Dir(dir)); ok {
			return p, nil
		}
	}

	return "", fmt.Errorf(
		"could not find %s. Looked in:\n  - %s\nRun this from a checkout, or pass -config <path>.",
		DefaultFile, strings.Join(tried, "\n  - "))
}

// LoadDefault finds and loads the config in one step. Both commands use it so
// they agree on where the file lives.
func LoadDefault(explicit string) (*Config, error) {
	path, err := Find(explicit)
	if err != nil {
		return nil, err
	}
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}
