// Supervisor tests come in two halves.
//
// The first half is hermetic and fast: port allocation, environment
// construction, the crash damper, the idle rule, log line splitting and the
// single-flight bookkeeping are all reachable without forking anything, and
// those are the parts most likely to be quietly wrong.
//
// The second half builds a ~30 line fake app with the local Go toolchain and
// actually runs it, because "the child answers health and then goes away when
// asked" is not a property you can unit test. It needs no network (stdlib
// only, GOPROXY off) and is skipped under -short.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"muxerr/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{
		Dir: dir,
		Site: config.Site{
			Addr:       "127.0.0.1:0",
			DataDir:    filepath.Join(dir, "data"),
			RuntimeDir: filepath.Join(dir, "runtime"),
		},
		Apps: []config.App{{
			Name:        "fake",
			Kind:        config.KindSync,
			Source:      "fake",
			HealthPath:  "/healthz",
			DBFile:      "fake.db",
			IdleTimeout: config.Duration(5 * time.Minute),
		}},
	}
}

func newTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	s := New(testConfig(t))
	t.Cleanup(s.StopAll)
	return s
}

// ------------------------------------------------------------ port allocation

func TestAllocatePortReturnsABindablePort(t *testing.T) {
	port, err := allocatePort()
	if err != nil {
		t.Fatalf("allocatePort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("port %d out of range", port)
	}
	// The whole point of closing the listener is that a child can then bind
	// it. Prove the window is real rather than theoretical.
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d was not free after allocation: %v", port, err)
	}
	l.Close()
}

func TestAllocatePortDoesNotRepeatItself(t *testing.T) {
	seen := map[int]bool{}
	for i := 0; i < 8; i++ {
		port, err := allocatePort()
		if err != nil {
			t.Fatalf("allocatePort: %v", err)
		}
		if seen[port] {
			t.Fatalf("port %d handed out twice in a row", port)
		}
		seen[port] = true
	}
}

// ------------------------------------------------------------------ env rules

func TestChildVarsReservedKeysBeatAppConfig(t *testing.T) {
	app := &config.App{
		Name: "fake",
		Env: map[string]string{
			"DB_PATH":    "/etc/passwd",
			"PORT":       "22",
			"STATIC_DIR": "/",
			"SEED":       "true",
		},
	}
	vars := childVars(app, "/data/kieran/fake/fake.db", "/runtime/dist", 41234, nil)

	if got := vars["DB_PATH"]; got != "/data/kieran/fake/fake.db" {
		t.Errorf("DB_PATH = %q, app config should not be able to redirect it", got)
	}
	if got := vars["PORT"]; got != "41234" {
		t.Errorf("PORT = %q, want 41234", got)
	}
	if got := vars["STATIC_DIR"]; got != "/runtime/dist" {
		t.Errorf("STATIC_DIR = %q, want /runtime/dist", got)
	}
	if got := vars["SEED"]; got != "true" {
		t.Errorf("SEED = %q, non-reserved app env should pass through", got)
	}
}

func TestChildVarsSharesHostVAPIDKeys(t *testing.T) {
	app := &config.App{
		Name: "workoutt",
		Env:  map[string]string{"VAPID_SUBJECT": "mailto:from-config@example.com"},
	}
	host := []string{
		"PATH=/usr/bin",
		"VAPID_PUBLIC_KEY=pub",
		"VAPID_PRIVATE_KEY=priv",
		"VAPID_SUBJECT=mailto:host@example.com",
		"HOME=/home/kieran",
	}
	vars := childVars(app, "/db", "/dist", 1234, host)

	if vars["VAPID_PUBLIC_KEY"] != "pub" || vars["VAPID_PRIVATE_KEY"] != "priv" {
		t.Errorf("host VAPID keys not passed through: %v", vars)
	}
	// The host is the application server identity; config must not shadow it
	// per deployment, or subscriptions made against one key stop working.
	if got := vars["VAPID_SUBJECT"]; got != "mailto:host@example.com" {
		t.Errorf("VAPID_SUBJECT = %q, want the host value", got)
	}
	if _, ok := vars["HOME"]; ok {
		t.Errorf("childVars should only set what it owns, got HOME")
	}
}

func TestBuildEnvReplacesRatherThanDuplicates(t *testing.T) {
	base := []string{"PATH=/usr/bin", "DB_PATH=/wrong/place.db", "malformed", "TZ=America/Halifax"}
	vars := map[string]string{"DB_PATH": "/right/place.db", "PORT": "9000"}

	out := buildEnv(base, vars)

	counts := map[string]int{}
	values := map[string]string{}
	for _, kv := range out {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("buildEnv emitted a malformed entry %q", kv)
		}
		counts[envFold(k)]++
		values[envFold(k)] = v
	}
	for k, n := range counts {
		if n != 1 {
			t.Errorf("%s appears %d times, want exactly 1", k, n)
		}
	}
	if values["DB_PATH"] != "/right/place.db" {
		t.Errorf("DB_PATH = %q, want the supervisor's value", values["DB_PATH"])
	}
	if values["PORT"] != "9000" {
		t.Errorf("PORT = %q, want 9000", values["PORT"])
	}
	// Inherited variables the supervisor does not own must survive: TZ is how
	// workoutt gets the right hour for a reminder.
	if values["TZ"] != "America/Halifax" {
		t.Errorf("TZ = %q, want the inherited value", values["TZ"])
	}
	if values["PATH"] != "/usr/bin" {
		t.Errorf("PATH = %q, want the inherited value", values["PATH"])
	}
}

// -------------------------------------------------------------- crash damper

func TestCrashTrackerHoldsDownARepeatOffender(t *testing.T) {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	var c crashTracker

	c.record(base)
	c.record(base.Add(time.Second))
	if _, blocked := c.blockedUntil(base.Add(2 * time.Second)); blocked {
		t.Fatal("blocked after 2 deaths, want the threshold to be 3")
	}

	c.record(base.Add(2 * time.Second))
	until, blocked := c.blockedUntil(base.Add(3 * time.Second))
	if !blocked {
		t.Fatal("not blocked after 3 deaths inside the window")
	}
	if want := base.Add(2 * time.Second).Add(crashPause); !until.Equal(want) {
		t.Errorf("blocked until %v, want %v (pause runs from the last death)", until, want)
	}

	if _, blocked := c.blockedUntil(until.Add(time.Millisecond)); blocked {
		t.Error("still blocked after the pause elapsed")
	}
	if c.total != 3 {
		t.Errorf("total = %d, want 3", c.total)
	}
}

func TestCrashTrackerForgetsOldDeaths(t *testing.T) {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	var c crashTracker
	c.record(base)
	c.record(base.Add(time.Second))

	// Two deaths a week ago plus one now is not a crash loop.
	later := base.Add(7 * 24 * time.Hour)
	c.record(later)
	if _, blocked := c.blockedUntil(later); blocked {
		t.Fatal("blocked by deaths outside the window")
	}
	if len(c.deaths) != 1 {
		t.Errorf("deaths = %d, want the window pruned to 1", len(c.deaths))
	}
	if c.total != 3 {
		t.Errorf("total = %d, want the lifetime count kept at 3", c.total)
	}
}

// --------------------------------------------------------------- idle policy

func TestShouldIdleStop(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		app      config.App
		lastUsed time.Time
		want     bool
	}{
		{"idle past the timeout", config.App{IdleTimeout: config.Duration(30 * time.Minute)}, now.Add(-31 * time.Minute), true},
		{"exactly at the timeout", config.App{IdleTimeout: config.Duration(30 * time.Minute)}, now.Add(-30 * time.Minute), true},
		{"still warm", config.App{IdleTimeout: config.Duration(30 * time.Minute)}, now.Add(-29 * time.Minute), false},
		{"no timeout configured", config.App{}, now.Add(-100 * time.Hour), false},
		{"always on", config.App{IdleTimeout: config.Duration(time.Minute), AlwaysOn: true}, now.Add(-100 * time.Hour), false},
		{"never used", config.App{IdleTimeout: config.Duration(time.Minute)}, time.Time{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldIdleStop(&tc.app, tc.lastUsed, now); got != tc.want {
				t.Errorf("shouldIdleStop = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIdleVictimsIgnoresStartingInstances(t *testing.T) {
	s := newTestSupervisor(t)
	app, _ := s.cfg.App("fake")
	app.IdleTimeout = config.Duration(time.Millisecond)

	// acquire registers without launching, so the instance is permanently
	// "starting" for the purposes of this test.
	inst, created, err := s.acquire("kieran", app)
	if err != nil || !created {
		t.Fatalf("acquire: inst=%v created=%v err=%v", inst, created, err)
	}
	if v := s.idleVictims(time.Now().Add(time.Hour)); len(v) != 0 {
		t.Fatalf("idleVictims picked up %d starting instances, want 0", len(v))
	}

	inst.finish(nil) // pretend it came up
	if v := s.idleVictims(time.Now().Add(time.Hour)); len(v) != 1 {
		t.Fatalf("idleVictims = %d, want 1 once the instance is ready", len(v))
	}
}

// ------------------------------------------------------------- single flight

func TestAcquireIsSingleFlight(t *testing.T) {
	s := newTestSupervisor(t)
	app, _ := s.cfg.App("fake")

	const callers = 50
	var wg sync.WaitGroup
	insts := make([]*Instance, callers)
	createdFlags := make([]bool, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			inst, created, err := s.acquire("kieran", app)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			insts[i], createdFlags[i] = inst, created
		}()
	}
	close(start)
	wg.Wait()

	creations := 0
	for i, created := range createdFlags {
		if created {
			creations++
		}
		if insts[i] == nil || insts[i] != insts[0] {
			t.Fatalf("caller %d got a different instance", i)
		}
	}
	if creations != 1 {
		t.Fatalf("%d callers started %d processes, want exactly 1", callers, creations)
	}
}

func TestAcquireRefusesACrashLoopingApp(t *testing.T) {
	s := newTestSupervisor(t)
	app, _ := s.cfg.App("fake")
	key := instanceKey("kieran", app.Name)
	for range crashThreshold {
		s.noteCrash(key)
	}

	_, _, err := s.acquire("kieran", app)
	if !errors.Is(err, ErrCrashLooping) {
		t.Fatalf("acquire error = %v, want ErrCrashLooping", err)
	}
	var be *BackoffError
	if !errors.As(err, &be) {
		t.Fatalf("error %v is not a *BackoffError; the gateway needs Retry-After", err)
	}
	if d := be.RetryAfter(time.Now()); d <= 0 || d > crashPause {
		t.Errorf("RetryAfter = %v, want (0, %v]", d, crashPause)
	}
	if be.RetryAfter(be.Until.Add(time.Hour)) != 0 {
		t.Error("RetryAfter should never be negative")
	}
	if len(s.instances) != 0 {
		t.Error("a refused start should not leave an instance in the table")
	}
}

func TestEnsureRejectsAppsWithNoBackend(t *testing.T) {
	s := newTestSupervisor(t)
	static := &config.App{Name: "brochure", Kind: config.KindStatic}
	if _, err := s.Ensure(context.Background(), "kieran", static); !errors.Is(err, ErrNotSupervised) {
		t.Fatalf("Ensure(static) = %v, want ErrNotSupervised", err)
	}
}

func TestEnsureAfterStopAll(t *testing.T) {
	s := New(testConfig(t))
	app, _ := s.cfg.App("fake")
	s.StopAll()
	if _, err := s.Ensure(context.Background(), "kieran", app); !errors.Is(err, ErrShutdown) {
		t.Fatalf("Ensure after StopAll = %v, want ErrShutdown", err)
	}
}

func TestEnsureHonoursContextDeadline(t *testing.T) {
	s := newTestSupervisor(t)
	app, _ := s.cfg.App("fake")
	// Registered but never launched, so the only way out is the context.
	if _, _, err := s.acquire("kieran", app); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := s.Ensure(ctx, "kieran", app)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ensure = %v, want DeadlineExceeded", err)
	}
}

// ---------------------------------------------------------------- instances

func TestInstancePathsAndTouch(t *testing.T) {
	cfg := testConfig(t)
	app, _ := cfg.App("fake")
	inst := newInstance(cfg, "kieran", app, time.Now())

	want := filepath.Join(cfg.InstanceDir("kieran", "fake"), "fake.db")
	if inst.DBPath() != want {
		t.Errorf("DBPath = %q, want %q", inst.DBPath(), want)
	}
	if inst.URL() != nil {
		t.Error("URL should be nil before a process exists")
	}

	inst.mu.Lock()
	inst.port = 41000
	inst.url = &neturl.URL{Scheme: "http", Host: "127.0.0.1:41000"}
	inst.mu.Unlock()

	u := inst.URL()
	if u.String() != "http://127.0.0.1:41000" {
		t.Errorf("URL = %q", u.String())
	}
	u.Host = "evil.example.com"
	if inst.URL().Host != "127.0.0.1:41000" {
		t.Error("URL handed out its own struct; a mutating proxy would corrupt it")
	}

	before := inst.LastUsed()
	time.Sleep(2 * time.Millisecond)
	inst.Touch()
	if !inst.LastUsed().After(before) {
		t.Error("Touch did not move the idle clock")
	}
}

// ------------------------------------------------------------------ logging

func TestLineLoggerSplitsAndFlushes(t *testing.T) {
	var got []string
	l := &lineLogger{emit: func(s string) { got = append(got, s) }}

	l.Write([]byte("one\ntw"))
	l.Write([]byte("o\r\nthree"))
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("lines = %q, want [one two] (partial line held back, CR trimmed)", got)
	}
	l.flush()
	if len(got) != 3 || got[2] != "three" {
		t.Fatalf("lines after flush = %q, want three at the end", got)
	}
	l.flush()
	if len(got) != 3 {
		t.Fatalf("flush emitted an empty buffer: %q", got)
	}
}

func TestLineLoggerCapsAnEndlessLine(t *testing.T) {
	var got []string
	l := &lineLogger{emit: func(s string) { got = append(got, s) }}
	l.Write([]byte(strings.Repeat("x", maxLogLine+10)))
	if len(got) != 1 {
		t.Fatalf("emitted %d records, want 1", len(got))
	}
	if len(l.buf) != 0 {
		t.Errorf("buffer still holds %d bytes after the cap", len(l.buf))
	}
}

// ------------------------------------------------------- the real thing

// fakeAppSource is a stand-in for workoutt/readerr: same environment contract,
// none of the behaviour.
const fakeAppSource = `package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	if os.Getenv("MODE") == "crash" {
		fmt.Fprintln(os.Stderr, "cannot open database")
		os.Exit(3)
	}
	db := os.Getenv("DB_PATH")
	if db == "" {
		fmt.Fprintln(os.Stderr, "no DB_PATH")
		os.Exit(2)
	}
	if err := os.WriteFile(db, []byte("fake"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// A little startup latency, so the readiness poll has something to poll.
	time.Sleep(40 * time.Millisecond)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s|%s|%s", os.Getenv("DB_PATH"), os.Getenv("STATIC_DIR"),
			os.Getenv("VAPID_PUBLIC_KEY"), os.Getenv("SEED"))
	})
	fmt.Println("fake app listening")
	if err := http.ListenAndServe(":"+os.Getenv("PORT"), mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

// buildFakeApp compiles fakeAppSource to every requested destination.
func buildFakeApp(t *testing.T, dests ...string) {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH; skipping the process-level tests")
	}
	src := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fakeapp\n\ngo 1.25.0\n")
	write("main.go", fakeAppSource)

	out := dests[0]
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goBin, "build", "-o", out, ".")
	cmd.Dir = src
	// Stdlib only: nothing to download, and GOPROXY=off proves it.
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the fake app: %v\n%s", err, out)
	}
	body, err := os.ReadFile(dests[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, dest := range dests[1:] {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnsureStartsProxiesAndStopsARealChild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a child process")
	}
	t.Setenv("VAPID_PUBLIC_KEY", "shared-public-key")

	cfg := testConfig(t)
	cfg.Apps[0].Env = map[string]string{
		"SEED":    "true",
		"DB_PATH": "/should/be/ignored.db", // must not win over the supervisor
	}
	app := &cfg.Apps[0]
	buildFakeApp(t, cfg.BinaryPath(app))

	s := New(cfg)
	defer s.StopAll()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inst, err := s.Ensure(ctx, "kieran", app)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if inst.PID() == 0 || inst.Port() == 0 {
		t.Fatalf("instance has pid %d port %d", inst.PID(), inst.Port())
	}
	if _, err := os.Stat(inst.DBPath()); err != nil {
		t.Errorf("child did not create %s: %v", inst.DBPath(), err)
	}

	// A second Ensure must reuse the process, not start another.
	again, err := s.Ensure(ctx, "kieran", app)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if again.PID() != inst.PID() {
		t.Errorf("second Ensure started pid %d, want the existing %d", again.PID(), inst.PID())
	}

	body := get(t, inst.URL().String()+"/env")
	parts := strings.Split(body, "|")
	if len(parts) != 4 {
		t.Fatalf("unexpected /env body %q", body)
	}
	if parts[0] != inst.DBPath() {
		t.Errorf("child DB_PATH = %q, want %q", parts[0], inst.DBPath())
	}
	if parts[1] != cfg.DistDir(app) {
		t.Errorf("child STATIC_DIR = %q, want %q", parts[1], cfg.DistDir(app))
	}
	if parts[2] != "shared-public-key" {
		t.Errorf("child VAPID_PUBLIC_KEY = %q, want the host's", parts[2])
	}
	if parts[3] != "true" {
		t.Errorf("child SEED = %q, want app env to pass through", parts[3])
	}

	running := s.Running()
	if len(running) != 1 || running[0].Username != "kieran" || running[0].App != "fake" {
		t.Fatalf("Running() = %+v", running)
	}
	if running[0].Restarts != 0 {
		t.Errorf("Restarts = %d on a clean start", running[0].Restarts)
	}

	if _, ok := s.Get("kieran", "fake"); !ok {
		t.Error("Get did not find the running instance")
	}
	if err := s.Stop("kieran", "fake"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := s.Get("kieran", "fake"); ok {
		t.Error("Get still returns a stopped instance")
	}
	if len(s.Running()) != 0 {
		t.Errorf("Running() = %+v after Stop", s.Running())
	}
	// The port must actually be free again, i.e. the process is gone.
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", inst.Port()))
		if err == nil {
			l.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("port %d still held after Stop: %v", inst.Port(), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := s.Stop("kieran", "fake"); err != nil {
		t.Errorf("stopping an already-stopped instance = %v, want nil", err)
	}
}

func TestEnsureBacksOffACrashLoopingChild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a child process")
	}
	cfg := testConfig(t)
	cfg.Apps[0].Env = map[string]string{"MODE": "crash"}
	app := &cfg.Apps[0]
	buildFakeApp(t, cfg.BinaryPath(app))

	s := New(cfg)
	defer s.StopAll()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := s.Ensure(ctx, "kieran", app)
	if err == nil {
		t.Fatal("Ensure succeeded for a child that exits immediately")
	}
	if errors.Is(err, ErrCrashLooping) {
		t.Fatalf("first failure should report the crash, not the damper: %v", err)
	}
	if !strings.Contains(err.Error(), "exit code 3") {
		t.Errorf("error %v does not mention the child's exit code", err)
	}
	if len(s.instances) != 0 {
		t.Error("a failed start left an instance in the table")
	}

	// startAttempts deaths in one Ensure is already a crash loop.
	_, err = s.Ensure(ctx, "kieran", app)
	if !errors.Is(err, ErrCrashLooping) {
		t.Fatalf("second Ensure = %v, want ErrCrashLooping", err)
	}
}

func TestStopAllStopsEveryChild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a child process")
	}
	cfg := testConfig(t)
	cfg.Apps = append(cfg.Apps, config.App{
		Name: "other", Kind: config.KindSync, Source: "other",
		HealthPath: "/healthz", DBFile: "other.db", AlwaysOn: true,
	})
	buildFakeApp(t, cfg.BinaryPath(&cfg.Apps[0]), cfg.BinaryPath(&cfg.Apps[1]))

	s := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s.StartAlwaysOn(ctx, []Pair{
		{Username: "kieran", App: "other"}, // always_on: starts
		{Username: "sam", App: "other"},    // always_on: starts
		{Username: "kieran", App: "fake"},  // not always_on: skipped
		{Username: "kieran", App: "ghost"}, // not configured: skipped
	})
	running := s.Running()
	if len(running) != 2 {
		t.Fatalf("StartAlwaysOn produced %+v, want two 'other' instances", running)
	}
	ports := []int{running[0].Port, running[1].Port}

	s.StopAll()
	if len(s.Running()) != 0 {
		t.Errorf("Running() = %+v after StopAll", s.Running())
	}
	for _, port := range ports {
		deadline := time.Now().Add(5 * time.Second)
		for {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err == nil {
				l.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("port %d still held after StopAll", port)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func get(t *testing.T, target string) string {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("reading %s: %v", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", target, resp.StatusCode)
	}
	return string(body)
}
