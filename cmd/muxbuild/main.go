// muxbuild turns the apps declared in apps.json into the two artifacts the
// gateway knows how to serve: a compiled Go backend and a built frontend whose
// every internal URL contains the /__MUX__ sentinel.
//
// This is a Go program rather than a shell script (or a Makefile, or an npm
// "build:all") for one reason: verification. The gateway's whole trick is
// rewriting a placeholder base path into /<user>/<app>/ on the way out. If a
// frontend is ever built without --base — a typo, a stale astro.config, a
// shell that ate the leading slash of "/__MUX__" — the build still succeeds,
// the dist still looks fine, and the app breaks only later, in the browser, as
// 404s on assets that were emitted with the wrong prefix. So after every
// frontend build this program walks the output and refuses to pass unless the
// placeholder actually made it into the files. A shell script would happily
// hand you a broken dist with exit code 0.
//
// The second reason is argument handling. `--base=/__MUX__` has to arrive at
// astro byte for byte. Every argument here goes through exec.Command's
// argument slice, so no shell — not cmd.exe, not PowerShell, not whatever
// wrapper someone invokes muxbuild from — ever gets a chance to reinterpret a
// slash, an equals sign or a leading dash.
//
// Usage:
//
//	muxbuild                        # build everything in apps.json
//	muxbuild -only readerr          # one app (repeatable, or comma-separated)
//	muxbuild -skip-frontend -v      # backends only, streaming child output
//	muxbuild -clean                 # wipe runtime/apps/<name> first
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"muxxerr/internal/config"
	"muxxerr/internal/version"
)

// scanLimit caps how much of a dist file we are willing to read when looking
// for the base placeholder. The sentinel lives in HTML, JS and CSS, all of
// which are small; a 30MB font or video that happens to contain the byte
// sequence would be evidence of nothing, and reading it would only make the
// check slow.
const scanLimit = 8 << 20

func main() {
	// Text rather than JSON logs: muxbuild is a thing a human runs at a
	// terminal and reads immediately. The gateway, which ships its logs
	// somewhere, uses the JSON handler.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	var (
		configPath   = flag.String("config", "", "path to apps.json (default: search the working directory, then next to the binary)")
		only         stringList
		skipFrontend = flag.Bool("skip-frontend", false, "do not build frontends")
		skipBackend  = flag.Bool("skip-backend", false, "do not compile Go backends")
		verbose      = flag.Bool("v", false, "show child process output")
		clean        = flag.Bool("clean", false, "delete each app's runtime directory before building")
	)
	flag.Var(&only, "only", "build only this app; repeatable or comma-separated")
	flag.Parse()

	if err := run(*configPath, only, *skipFrontend, *skipBackend, *verbose, *clean); err != nil {
		// Printed, not logged. The errors that end a build here are
		// deliberately multi-line — the config validator lists every problem,
		// the toolchain check explains what to install, the placeholder check
		// is a paragraph about why the dist is unusable — and slog escapes
		// newlines into a literal \n, which turns the single most important
		// message this program emits into an unreadable smear.
		fmt.Fprintf(os.Stderr, "\nmuxbuild: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string, only stringList, skipFrontend, skipBackend, verbose, clean bool) error {
	cfg, err := config.LoadDefault(configPath)
	if err != nil {
		return err
	}

	apps, err := selectApps(cfg, only)
	if err != nil {
		return err
	}

	// Remote sources are fetched before anything else, because the planning
	// below decides what to build by looking at what is on disk — and for a
	// git+ app, nothing is, until we put it there.
	if err := fetchRemoteSources(cfg, apps, verbose); err != nil {
		return err
	}

	// Plan first, then check the toolchain, then build. Knowing up front
	// exactly which steps will run is what lets us say "npm is missing and you
	// need it for readerr's frontend" instead of dying halfway through with
	// exec: "npm": executable file not found in %PATH%.
	tasks := make([]*task, 0, len(apps))
	for _, a := range apps {
		t := &task{cfg: cfg, app: a}
		t.wantBackend = a.Kind == config.KindSync && !skipBackend
		t.wantFrontend = !skipFrontend && dirExists(cfg.FrontendSrc(a))
		if !skipFrontend && !t.wantFrontend {
			// A sync app without a frontend directory is legitimate (an API-only
			// service); a static app without one is a config error, since the
			// frontend is the entire app.
			if a.Kind == config.KindStatic {
				return fmt.Errorf("app %q is static but has no frontend at %s", a.Name, cfg.FrontendSrc(a))
			}
			slog.Warn("no frontend directory, skipping frontend build",
				"app", a.Name, "dir", cfg.FrontendSrc(a))
		}
		if t.wantBackend && !dirExists(cfg.BackendSrc(a)) {
			return fmt.Errorf("app %q: backend source %s does not exist", a.Name, cfg.BackendSrc(a))
		}
		tasks = append(tasks, t)
	}

	tc, err := findToolchain(tasks)
	if err != nil {
		return err
	}

	if clean {
		for _, t := range tasks {
			dir := filepath.Dir(t.cfg.BinaryPath(t.app))
			slog.Info("cleaning", "app", t.app.Name, "dir", dir)
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("clean %s: %w", dir, err)
			}
		}
	}

	// Ctrl-C should take the whole tree down, not orphan a running `go build`
	// or a node process holding the dist directory open. CommandContext kills
	// each child when this context is cancelled.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Half the cores: `go build` and astro/vite are each already internally
	// parallel, so running one build per core mostly makes them fight over the
	// same cores and thrash memory. Half leaves the machine usable too.
	workers := runtime.NumCPU() / 2
	if workers < 1 {
		workers = 1
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}

	// Child output is captured into a per-app buffer and flushed in one piece
	// when that app finishes. Truly streaming several concurrent builds to the
	// same terminal produces interleaved nonsense; grouped-but-late output is
	// far more useful than live-but-shredded. The exception is a single build,
	// where there is nothing to interleave with, so -v streams for real.
	stream := verbose && len(tasks) == 1
	started := time.Now()
	slog.Info("building", "apps", len(tasks), "workers", workers)

	var (
		wg      sync.WaitGroup
		sem     = make(chan struct{}, workers)
		printMu sync.Mutex
	)
	for _, t := range tasks {
		wg.Add(1)
		go func(t *task) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			t.buf = &bytes.Buffer{}
			t.out = io.Writer(t.buf)
			if stream {
				t.out = io.MultiWriter(t.buf, os.Stderr)
			}
			t.build(ctx, tc)

			printMu.Lock()
			defer printMu.Unlock()
			if t.err != nil {
				// Just the fact, here. The error text itself is reproduced in
				// full at the end, where it can span lines without slog
				// flattening it.
				slog.Error("app failed", "app", t.app.Name, "elapsed", t.elapsed.Round(time.Millisecond))
			} else {
				slog.Info("app done", "app", t.app.Name, "elapsed", t.elapsed.Round(time.Millisecond))
			}
			if verbose && !stream && t.buf.Len() > 0 {
				fmt.Fprintf(os.Stderr, "\n----- %s output -----\n%s-----\n\n", t.app.Name, ensureNewline(t.buf.String()))
			}
		}(t)
	}
	wg.Wait()

	// The summary is a table, so it goes to stdout through tabwriter rather
	// than through slog: key=value pairs do not line up, and lining up is the
	// entire point of a summary.
	printSummary(os.Stdout, tasks, time.Since(started))

	var failed []*task
	for _, t := range tasks {
		if t.err != nil {
			failed = append(failed, t)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	// Report every failure. One run should tell you everything that is broken,
	// not make you fix, rebuild, discover the next one, repeat.
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d app(s) failed:", len(failed), len(tasks))
	for _, t := range failed {
		fmt.Fprintf(&b, "\n\n  %s: %v", t.app.Name, t.err)
		if tail := tailLines(t.buf.String(), 25); tail != "" && !stream {
			fmt.Fprintf(&b, "\n%s", indent(tail, "    | "))
		}
	}
	return errors.New(b.String())
}

// task is one app's build: the plan, the captured output, and the measurements
// that end up in the summary table.
type task struct {
	cfg *config.Config
	app *config.App

	wantBackend  bool
	wantFrontend bool

	buf *bytes.Buffer
	out io.Writer

	binSize      int64
	dist         distStats
	buildInfo    version.AppBuild
	hasChangelog bool
	elapsed      time.Duration
	err          error
}

func (t *task) build(ctx context.Context, tc *toolchain) {
	start := time.Now()
	defer func() { t.elapsed = time.Since(start) }()

	if t.wantBackend {
		if t.err = t.buildBackend(ctx, tc); t.err != nil {
			return
		}
	}
	if t.wantFrontend {
		if t.err = t.buildFrontend(ctx, tc); t.err != nil {
			return
		}
	}
	// Reached only on success (both steps above return early on error), so the
	// metadata we write always describes artifacts that were actually produced.
	t.writeBuildInfo(ctx)
	t.writeChangelog()
}

// buildBackend compiles the app's Go module to <runtime>/apps/<name>/<name>.
//
// CGO_ENABLED=0 is not an optimisation, it is the contract: every app here
// uses modernc.org/sqlite, a pure-Go driver, so there is nothing to link
// against and the result is a static binary that the supervisor can spawn on
// any machine with the same GOOS/GOARCH. It also means the build never depends
// on a C toolchain being installed, which on Windows it usually is not.
func (t *task) buildBackend(ctx context.Context, tc *toolchain) error {
	bin := t.cfg.BinaryPath(t.app)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(bin), err)
	}
	args := []string{
		"build",
		"-trimpath", // absolute paths from this machine do not belong in the binary
		// One argument, not three: the shell form -ldflags="-s -w" collapses to
		// a single argv entry, and splitting it would make `go` treat -w as a
		// package path.
		"-ldflags=-s -w",
		"-o", bin,
		".",
	}
	if err := t.exec(ctx, t.cfg.BackendSrc(t.app), tc.goBin, args, "CGO_ENABLED=0"); err != nil {
		return err
	}
	st, err := os.Stat(bin)
	if err != nil {
		return fmt.Errorf("go build reported success but %s is missing: %w", bin, err)
	}
	t.binSize = st.Size()
	return nil
}

// buildFrontend produces the app's dist directory.
//
// Two shapes are supported, distinguished by whether the frontend directory has
// a package.json. A node project is built with Astro and the sentinel base; a
// plain directory of files — the frontend-only apps this gateway is also meant
// to host — is copied verbatim. Running `npm ci` against a folder of hand-
// written HTML would fail with an npm usage error that says nothing useful
// about what is actually wrong.
func (t *task) buildFrontend(ctx context.Context, tc *toolchain) error {
	src := t.cfg.FrontendSrc(t.app)
	dist := t.cfg.DistDir(t.app)

	if !fileExists(filepath.Join(src, "package.json")) {
		return t.copyFrontend(src, dist)
	}

	// `npm ci` is slow (tens of seconds) and wipes node_modules, so it runs
	// only when there is no node_modules at all. Anyone who has changed
	// package.json can rerun with -clean or delete the directory; making every
	// build pay for a reinstall to catch that rare case is the wrong trade.
	if !dirExists(filepath.Join(src, "node_modules")) {
		if err := t.exec(ctx, src, tc.npm, []string{"ci"}); err != nil {
			return fmt.Errorf("npm ci: %w", err)
		}
	} else {
		fmt.Fprintf(t.out, "# node_modules present, skipping npm ci\n")
	}

	if err := os.MkdirAll(filepath.Dir(dist), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dist), err)
	}
	// --base carries the sentinel; --outDir puts the result under the runtime
	// directory instead of inside the app's source tree, so the source repo
	// stays clean and `git status` in workoutt/readerr shows nothing after a
	// gateway build. Both are single argv entries with no shell in between.
	args := []string{
		"astro", "build",
		"--base=" + t.app.BasePlaceholder,
		"--outDir", dist,
	}
	if err := t.exec(ctx, src, tc.npx, args); err != nil {
		return fmt.Errorf("astro build: %w", err)
	}

	stats, err := inspectDist(dist, t.app.BasePlaceholder)
	if err != nil {
		return err
	}
	t.dist = stats

	if !stats.hasIndex {
		return fmt.Errorf("frontend build produced no index.html in %s (%d files); "+
			"the gateway serves index.html as the app shell and cannot mount this app",
			dist, stats.files)
	}
	// The check this whole program exists for.
	if t.app.BasePlaceholder != "" && stats.withPlaceholder == 0 {
		return fmt.Errorf(
			"frontend built WITHOUT the base placeholder: not one of the %d files in %s contains %q.\n"+
				"That means --base=%s did not take effect (a shell mangled the argument, or astro.config.* "+
				"overrides base, or the build was cached from an earlier run).\n"+
				"The gateway rewrites %s to /<user>/%s/ at serve time; with the sentinel absent it has "+
				"nothing to rewrite and would silently serve URLs that 404 in the browser. "+
				"Refusing to publish this dist — rebuild with -clean once the base flag is honoured",
			stats.files, dist, t.app.BasePlaceholder,
			t.app.BasePlaceholder, t.app.BasePlaceholder, t.app.Name)
	}
	return nil
}

// exec runs one child process with its arguments passed as a slice. The echoed
// command line above the output is for reading, not for copy-pasting: it is
// not shell-quoted, precisely because nothing here goes through a shell.
func (t *task) exec(ctx context.Context, dir, bin string, args []string, extraEnv ...string) error {
	fmt.Fprintf(t.out, "# in %s\n$ %s %s\n", dir, bin, strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdout = t.out
	cmd.Stderr = t.out
	// Nil stdin becomes the null device. npx will offer to install a missing
	// package if it can prompt; with no stdin it fails fast instead of hanging
	// a parallel build forever on a question nobody can see.
	cmd.Stdin = nil
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s: interrupted", filepath.Base(bin))
		}
		return fmt.Errorf("%s %s: %w", filepath.Base(bin), strings.Join(args, " "), err)
	}
	return nil
}

// distStats is what a built frontend looks like from the gateway's point of
// view: how much there is, and whether it is rewritable.
type distStats struct {
	files           int
	bytes           int64
	withPlaceholder int
	hasIndex        bool
}

// inspectDist walks the built output. It is deliberately dumb — count files,
// count bytes, look for a literal byte sequence — because the failure it
// guards against is equally dumb and equally invisible.
func inspectDist(dir, placeholder string) (distStats, error) {
	var st distStats
	needle := []byte(placeholder)

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st.files++
		st.bytes += info.Size()
		if strings.EqualFold(d.Name(), "index.html") {
			st.hasIndex = true
		}
		if len(needle) == 0 || info.Size() > scanLimit {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if bytes.Contains(body, needle) {
			st.withPlaceholder++
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return st, fmt.Errorf("frontend build left no output directory at %s", dir)
		}
		return st, fmt.Errorf("inspect %s: %w", dir, err)
	}
	if st.files == 0 {
		return st, fmt.Errorf("frontend build left %s empty", dir)
	}
	return st, nil
}

// toolchain holds resolved absolute paths to the external programs we shell
// out to. Resolving once up front means the error for a missing tool is a
// sentence about what to install, and means every child is launched by full
// path rather than re-searching PATH per invocation.
type toolchain struct {
	goBin string
	npm   string
	npx   string
}

func findToolchain(tasks []*task) (*toolchain, error) {
	var needGo, needNode []string
	for _, t := range tasks {
		if t.wantBackend {
			needGo = append(needGo, t.app.Name)
		}
		if t.wantFrontend {
			needNode = append(needNode, t.app.Name)
		}
	}

	tc := &toolchain{}
	var missing []string
	if len(needGo) > 0 {
		p, err := lookTool("go")
		if err != nil {
			missing = append(missing, fmt.Sprintf(
				"go — needed to compile the sync backends (%s). Install Go 1.25+ and make sure it is on PATH, or pass -skip-backend",
				strings.Join(needGo, ", ")))
		}
		tc.goBin = p
	}
	if len(needNode) > 0 {
		npm, errNPM := lookTool("npm")
		npx, errNPX := lookTool("npx")
		if errNPM != nil || errNPX != nil {
			missing = append(missing, fmt.Sprintf(
				"npm/npx — needed to build the frontends (%s). Install Node.js 22+ and make sure it is on PATH, or pass -skip-frontend",
				strings.Join(needNode, ", ")))
		}
		tc.npm, tc.npx = npm, npx
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing build tools:\n  - %s", strings.Join(missing, "\n  - "))
	}
	return tc, nil
}

// lookTool resolves an external program, with a Windows wrinkle.
//
// On Windows npm and npx are not executables. They are `npm.cmd` batch shims
// sitting next to extensionless shell scripts of the same name (which exist
// for Git Bash / MSYS). exec.LookPath does consult PATHEXT and will normally
// find the .cmd, but the outcome depends on how PATHEXT happens to be set in
// whichever shell launched muxbuild — and if the extensionless script is
// picked instead, CreateProcess fails with a baffling "%1 is not a valid Win32
// application". Probing the .cmd shim explicitly, first, removes the
// ambiguity, and makes the not-found error name the thing that is genuinely
// absent.
//
// Batch shims are also why arguments matter here: cmd.exe re-parses the
// command line the shim receives. Go's os/exec applies cmd.exe quoting rules
// when the target is a .bat/.cmd, so `--base=/__MUX__` arrives intact — but
// only because we hand it over as one element of an argument slice and never
// build a command string ourselves.
func lookTool(name string) (string, error) {
	candidates := []string{name}
	if runtime.GOOS == "windows" {
		candidates = []string{name + ".cmd", name + ".exe", name + ".bat", name}
	}
	var firstErr error
	for _, c := range candidates {
		p, err := exec.LookPath(c)
		if err == nil {
			return p, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return "", firstErr
}

// selectApps resolves -only into the set of apps to build, rejecting unknown
// names instead of silently building nothing — a typo in -only should not look
// like a successful no-op build.
func selectApps(cfg *config.Config, only stringList) ([]*config.App, error) {
	if len(only) == 0 {
		apps := make([]*config.App, 0, len(cfg.Apps))
		for i := range cfg.Apps {
			apps = append(apps, &cfg.Apps[i])
		}
		return apps, nil
	}
	var (
		apps []*config.App
		seen = map[string]bool{}
	)
	for _, name := range only {
		if seen[name] {
			continue
		}
		seen[name] = true
		a, ok := cfg.App(name)
		if !ok {
			var names []string
			for i := range cfg.Apps {
				names = append(names, cfg.Apps[i].Name)
			}
			return nil, fmt.Errorf("unknown app %q; configured apps: %s", name, strings.Join(names, ", "))
		}
		apps = append(apps, a)
	}
	return apps, nil
}

func printSummary(w io.Writer, tasks []*task, total time.Duration) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nAPP\tKIND\tBINARY\tDIST FILES\tDIST SIZE\tPLACEHOLDER\tBUILD\tTIME\tSTATUS")
	for _, t := range tasks {
		bin := "-"
		if t.binSize > 0 {
			bin = humanBytes(t.binSize)
		} else if !t.wantBackend {
			bin = "skipped"
		}

		files, size, ph := "-", "-", "-"
		switch {
		case !t.wantFrontend:
			files, size, ph = "skipped", "skipped", "skipped"
		case t.dist.files > 0:
			files = fmt.Sprintf("%d", t.dist.files)
			size = humanBytes(t.dist.bytes)
			if t.app.BasePlaceholder == "" {
				ph = "disabled"
			} else {
				ph = fmt.Sprintf("%d/%d", t.dist.withPlaceholder, t.dist.files)
			}
		}

		status := "ok"
		if t.err != nil {
			status = "FAILED"
		}
		build := "-"
		if b := t.buildInfo.Badge(); b != "" {
			build = b
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.app.Name, t.app.Kind, bin, files, size, ph, build,
			t.elapsed.Round(time.Millisecond), status)
	}
	tw.Flush()
	fmt.Fprintf(w, "\ntotal %s\n", total.Round(time.Millisecond))
}

// stringList collects a repeatable flag that also accepts comma-separated
// values, so -only a -only b and -only a,b mean the same thing.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// copyFrontend stages a frontend that needs no build step: a directory of
// static files, copied into the runtime tree so the gateway serves the same
// shape it serves for a built app, and so the source directory is never the
// thing being read from at request time.
//
// The sentinel base is optional here. A hand-written static app can perfectly
// well use relative URLs and never mention it, so unlike the Astro path this
// does not fail when the placeholder is absent — it just reports the count and
// lets the operator notice.
func (t *task) copyFrontend(src, dist string) error {
	fmt.Fprintf(t.out, "# no package.json — copying static files verbatim\n")
	if !dirExists(src) {
		return fmt.Errorf("frontend directory %s does not exist", src)
	}
	if err := os.RemoveAll(dist); err != nil {
		return fmt.Errorf("clear %s: %w", dist, err)
	}
	if err := copyTree(src, dist); err != nil {
		return err
	}
	stats, err := inspectDist(dist, t.app.BasePlaceholder)
	if err != nil {
		return err
	}
	t.dist = stats
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// Symlinks are skipped rather than followed: a link pointing outside
		// the source tree would stage a file the operator never meant to
		// publish, and the gateway serves this directory to logged-in users.
		if !d.Type().IsRegular() {
			fmt.Fprintf(os.Stderr, "# skipping non-regular file %s\n", rel)
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := io.Copy(out, in); err != nil {
			return err
		}
		return out.Close()
	})
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// tailLines returns the last n non-empty-trimmed lines, which for a failed
// build is where the compiler or vite actually said what went wrong.
func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func ensureNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
