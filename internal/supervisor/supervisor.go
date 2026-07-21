// Package supervisor owns the child processes that turn single-tenant apps
// into multi-tenant ones: one OS process and one SQLite file per (user, app)
// pair.
//
// The apps know nothing about any of this. They are configured entirely by
// environment — DB_PATH, PORT, STATIC_DIR — they serve root-anchored routes,
// and they answer a health path once their migrations have run. That contract
// is the entire isolation story: two users of workoutt are two processes with
// two files and no shared state, so a missing WHERE clause in the app cannot
// leak one user's data into another's.
//
// Why a process per tenant rather than one process with a tenant column: the
// apps are already written single-tenant. Retrofitting a tenant key into every
// query is a permanent invitation to forget one, and the forgetting is silent.
// Cold start measured ~111ms on a fresh database, ~55ms warm, at ~12MB RSS —
// so a process per tenant is cheaper than auditing every SQL statement in
// three codebases forever. It also means an app crash blast-radius is one
// user, and "export this user's data" is `cp`.
//
// Instances start lazily on the first request that needs them and stop again
// after an idle timeout, except for apps marked always_on: workoutt's
// push-notification scheduler is a goroutine inside the child, so its
// reminders only fire while that process is alive.
//
// The child inherits the gateway's environment, which is how TZ reaches it —
// workoutt builds reminder instants in the host timezone, and a child running
// in UTC while the host is in Halifax sends notifications at the wrong hour.
// Anything the host does not define can be set per-app in apps.json.
package supervisor

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"local-multiplexer/internal/config"
)

const (
	// startAttempts bounds the retry loop that exists solely because port
	// allocation is racy: we ask the kernel for a free port, close the
	// listener, and hand the number to a child that binds it a few
	// milliseconds later. Anything else on the machine can take it in
	// between. Three attempts makes that lottery loss vanishingly unlikely
	// while still failing fast on an app that simply cannot start.
	startAttempts = 3

	// healthInterval is the readiness poll period. 50ms against a ~55ms warm
	// start means a warm instance is usually ready on the first or second
	// probe; the cost of polling this fast is a handful of refused connects.
	healthInterval = 50 * time.Millisecond
	// healthTimeout caps one probe. A health handler that takes seconds is
	// broken, not slow.
	healthTimeout = 2 * time.Second
	// startupTimeout caps the whole launch, including retries. A first-run
	// migration on a big import can take a while; a minute is generous and
	// still bounded, so a wedged child cannot pin a request forever.
	startupTimeout = 60 * time.Second

	// stopGrace is how long a child gets to exit after being asked politely
	// before it is killed. SQLite in WAL mode recovers from a kill, so this
	// is about letting in-flight requests finish, not about data safety.
	stopGrace = 5 * time.Second

	// Crash-loop damping. An app that dies instantly on every start (bad
	// migration, corrupt database, missing asset) would otherwise spawn a
	// process per request and burn a core. After crashThreshold deaths inside
	// crashWindow, refuse to start it again for crashPause and hand the
	// gateway an error it can render as 503.
	crashWindow    = time.Minute
	crashThreshold = 3
	crashPause     = 30 * time.Second

	// janitorInterval is how often idle instances are reaped. Idle timeouts
	// are minutes; checking every ten seconds is precise enough and costs a
	// map walk.
	janitorInterval = 10 * time.Second

	// maxLogLine caps how much of a child's output is buffered while waiting
	// for a newline, so a child that emits a megabyte without one cannot grow
	// the gateway's heap.
	maxLogLine = 64 << 10
)

var (
	// ErrShutdown means the supervisor is stopping; nothing new will start.
	ErrShutdown = errors.New("supervisor is shutting down")
	// ErrCrashLooping means the app died repeatedly and is being held down.
	// The gateway should render this as 503 with a Retry-After.
	ErrCrashLooping = errors.New("app is crash-looping")
	// ErrNotSupervised means the app has no child process to run.
	ErrNotSupervised = errors.New("app has no backend to supervise")

	// errChildExited marks the one failure mode worth retrying on a fresh
	// port: the child was started and then died before it answered health.
	errChildExited = errors.New("child exited during startup")
)

// BackoffError is returned while a crash-looping app is held down. It carries
// the deadline so the gateway can emit an honest Retry-After.
type BackoffError struct {
	Username string
	App      string
	Until    time.Time
	Deaths   int
}

func (e *BackoffError) Error() string {
	return fmt.Sprintf("%s/%s: %v (%d deaths in the last %v), retrying after %s",
		e.Username, e.App, ErrCrashLooping, e.Deaths, crashWindow, e.Until.Format(time.RFC3339))
}

func (e *BackoffError) Unwrap() error { return ErrCrashLooping }

// RetryAfter is how long the caller should wait, never negative.
func (e *BackoffError) RetryAfter(now time.Time) time.Duration {
	if d := e.Until.Sub(now); d > 0 {
		return d
	}
	return 0
}

// Pair identifies one provisioned (user, app) instance.
type Pair struct {
	Username string
	App      string
}

// Status is a snapshot for the admin page. Restarts counts unexpected exits
// for this pair, not currently-live processes, so it keeps counting across a
// crash loop — which is exactly the number an operator wants to see.
type Status struct {
	Username  string
	App       string
	PID       int
	Port      int
	StartedAt time.Time
	LastUsed  time.Time
	Restarts  int
}

// Supervisor is the process table. One per gateway.
type Supervisor struct {
	cfg   *config.Config
	probe *http.Client
	now   func() time.Time

	// mu guards instances, crashes and closed. It is never held across a
	// process spawn or a health poll — Ensure registers a placeholder under
	// the lock and every caller then waits on that instance's ready channel,
	// which is what makes concurrent cold starts collapse into one process.
	mu        sync.Mutex
	instances map[string]*Instance
	crashes   map[string]*crashTracker
	closed    bool

	// logs holds a short tail of each instance's output, keyed the same way
	// as instances but with its own lock: a page reading logs must never
	// contend with a cold start, and a child writing a line must never wait
	// on the map that governs process lifecycle. See logbuf.go.
	logMu sync.Mutex
	logs  map[string]*logRing

	done chan struct{} // closed by StopAll; stops the janitor
}

// New builds a supervisor and starts its idle janitor. Call StopAll to release
// both the children and the janitor goroutine.
func New(cfg *config.Config) *Supervisor {
	s := &Supervisor{
		cfg:       cfg,
		now:       time.Now,
		instances: make(map[string]*Instance),
		crashes:   make(map[string]*crashTracker),
		logs:      make(map[string]*logRing),
		done:      make(chan struct{}),
		probe: &http.Client{
			Timeout: healthTimeout,
			// An explicit transport with Proxy nil: the health probe talks to
			// 127.0.0.1 and must never be routed through whatever HTTP_PROXY
			// the host happens to export. Keep-alives are off because the
			// peer may be killed moments later and a pooled connection to a
			// dead child is a confusing failure two minutes from now.
			Transport: &http.Transport{
				Proxy:               nil,
				DisableKeepAlives:   true,
				DialContext:         (&net.Dialer{Timeout: healthTimeout}).DialContext,
				TLSHandshakeTimeout: healthTimeout,
			},
		},
	}
	go s.janitor()
	return s
}

// Ensure returns a ready instance for (username, app), starting one if needed,
// and blocks until the child answers its health path or ctx expires.
//
// Concurrent callers for the same pair all wait on the same instance, so a
// browser that fires six parallel requests at a cold app starts one process,
// not six.
func (s *Supervisor) Ensure(ctx context.Context, username string, app *config.App) (*Instance, error) {
	if app == nil || app.Kind != config.KindSync {
		return nil, fmt.Errorf("%s: %w", username, ErrNotSupervised)
	}
	inst, created, err := s.acquire(username, app)
	if err != nil {
		return nil, err
	}
	if created {
		go s.launch(inst)
	}
	select {
	case <-inst.ready:
		if err := inst.startErr; err != nil {
			return nil, err
		}
		inst.Touch()
		return inst, nil
	case <-ctx.Done():
		// The launch keeps going: another caller may still be waiting, and an
		// instance that becomes ready two seconds after this request gave up
		// is still useful to the next one.
		return nil, fmt.Errorf("waiting for %s/%s: %w", username, app.Name, ctx.Err())
	}
}

// acquire hands back the instance for a pair, registering a new (not yet
// started) one if there is none. It is the whole of the single-flight: the map
// write and the crash-loop check happen under one lock, and created is true
// for exactly one caller.
func (s *Supervisor) acquire(username string, app *config.App) (inst *Instance, created bool, err error) {
	key := instanceKey(username, app.Name)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, ErrShutdown
	}
	if existing, ok := s.instances[key]; ok {
		return existing, false, nil
	}
	if t := s.crashes[key]; t != nil {
		if until, blocked := t.blockedUntil(s.now()); blocked {
			return nil, false, &BackoffError{Username: username, App: app.Name, Until: until, Deaths: len(t.deaths)}
		}
	}
	inst = newInstance(s.cfg, username, app, s.now())
	s.instances[key] = inst
	return inst, true, nil
}

// Get returns a serving instance without starting one. Instances that are
// still in startup are deliberately invisible here: callers of Get want
// something they can proxy to right now.
func (s *Supervisor) Get(username, app string) (*Instance, bool) {
	s.mu.Lock()
	inst, ok := s.instances[instanceKey(username, app)]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	select {
	case <-inst.ready:
		if inst.startErr != nil {
			return nil, false
		}
		return inst, true
	default:
		return nil, false
	}
}

// Stop shuts one instance down. Stopping something that is not running is not
// an error — the gateway calls this from admin actions and from deprovision,
// where "already gone" is the desired end state.
func (s *Supervisor) Stop(username, app string) error {
	key := instanceKey(username, app)
	s.mu.Lock()
	inst := s.instances[key]
	delete(s.instances, key)
	s.mu.Unlock()
	if inst == nil {
		return nil
	}
	return s.stop(inst)
}

// StopAll stops every instance in parallel and retires the supervisor. After
// it returns, Ensure fails with ErrShutdown.
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	insts := make([]*Instance, 0, len(s.instances))
	for _, inst := range s.instances {
		insts = append(insts, inst)
	}
	s.instances = make(map[string]*Instance)
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, inst := range insts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.stop(inst); err != nil {
				slog.Warn("stopping instance", "user", inst.Username, "app", inst.App.Name, "error", err)
			}
		}()
	}
	wg.Wait()
}

// Running lists live instances, newest bookkeeping included, sorted so the
// admin page does not reshuffle on every refresh.
func (s *Supervisor) Running() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.instances))
	for key, inst := range s.instances {
		st := inst.status()
		if t := s.crashes[key]; t != nil {
			st.Restarts = t.total
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b Status) int {
		return cmp.Or(strings.Compare(a.Username, b.Username), strings.Compare(a.App, b.App))
	})
	return out
}

// StartAlwaysOn boots every always_on app for every provisioned pair, in
// parallel but bounded — a hundred users of workoutt should not fork a hundred
// processes in the same millisecond and make the gateway look hung at boot.
// Failures are logged, not returned: one user's broken database must not stop
// the gateway from starting.
func (s *Supervisor) StartAlwaysOn(ctx context.Context, pairs []Pair) {
	const parallel = 4
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	started := 0
	for _, p := range pairs {
		app, ok := s.cfg.App(p.App)
		if !ok || app.Kind != config.KindSync || !app.AlwaysOn {
			continue
		}
		started++
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if _, err := s.Ensure(ctx, p.Username, app); err != nil {
				slog.Error("start always-on instance", "user", p.Username, "app", app.Name, "error", err)
				return
			}
			slog.Info("always-on instance ready", "user", p.Username, "app", app.Name)
		}()
	}
	wg.Wait()
	if started > 0 {
		slog.Info("always-on startup complete", "instances", started)
	}
}

// ------------------------------------------------------------------ launching

// launch runs in its own goroutine so that no caller's cancelled context can
// abandon a half-started child, and so the global map lock is free the whole
// time. It retries on a fresh port when the child dies immediately, which is
// the observable symptom of losing the bind race.
func (s *Supervisor) launch(inst *Instance) {
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()

	key := instanceKey(inst.Username, inst.App.Name)
	var err error
	for attempt := 1; attempt <= startAttempts; attempt++ {
		if inst.stopping() {
			err = ErrShutdown
			break
		}
		err = s.spawn(ctx, inst)
		if err == nil {
			slog.Info("instance ready",
				"user", inst.Username, "app", inst.App.Name,
				"pid", inst.PID(), "port", inst.Port(),
				"startup", time.Since(inst.startedAt()).Round(time.Millisecond))
			break
		}
		if !errors.Is(err, errChildExited) {
			break // a missing binary or an unwritable data dir will not fix itself
		}
		// The child was alive and then wasn't. Count it: this is the signal
		// the crash-loop damper is built on.
		s.noteCrash(key)
		slog.Warn("instance died during startup",
			"user", inst.Username, "app", inst.App.Name,
			"attempt", attempt, "of", startAttempts, "error", err)
	}

	if err != nil {
		slog.Error("instance failed to start", "user", inst.Username, "app", inst.App.Name, "error", err)
		// Drop it from the table before releasing the waiters, so the next
		// request starts a genuinely fresh attempt rather than joining a
		// corpse.
		s.forget(inst)
	}
	inst.finish(err)
}

// spawn starts one child on one freshly allocated port and waits for it to
// answer health.
func (s *Supervisor) spawn(ctx context.Context, inst *Instance) error {
	if err := os.MkdirAll(inst.dir, 0o700); err != nil {
		return fmt.Errorf("create instance dir %s: %w", inst.dir, err)
	}
	bin := s.cfg.BinaryPath(inst.App)
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("app binary %s: %w (run muxbuild)", bin, err)
	}
	port, err := allocatePort()
	if err != nil {
		return fmt.Errorf("allocate port: %w", err)
	}

	host := os.Environ()
	cmd := exec.Command(bin)
	// Run in the instance's own directory so a child that writes a stray
	// relative path lands inside its sandbox and not in the gateway's cwd.
	cmd.Dir = inst.dir
	cmd.Env = buildEnv(host, childVars(inst.App, inst.dbPath, s.cfg.DistDir(inst.App), port, host))

	base := slog.With("user", inst.Username, "app", inst.App.Name, "port", port)
	outLog := newLineLogger(base.With("stream", "stdout"), slog.LevelInfo)
	errLog := newLineLogger(base.With("stream", "stderr"), slog.LevelWarn)
	// Every line goes two places: the gateway's own log, as before, and a
	// small per-instance ring the owner can read from the dashboard. Tapping
	// the same writer rather than adding a second pipe keeps the ordering
	// guarantee that made io.Writer the right choice here in the first place.
	key := instanceKey(inst.Username, inst.App.Name)
	outLog.tap = func(line string) { s.record(key, "stdout", line) }
	errLog.tap = func(line string) { s.record(key, "stderr", line) }
	// Assigning io.Writers rather than using StdoutPipe is deliberate: os/exec
	// then owns the pipes and cmd.Wait blocks until both have been drained, so
	// the last lines of a dying child are never lost to a race between Wait
	// and a scanner goroutine.
	cmd.Stdout = outLog
	cmd.Stderr = errLog

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", bin, err)
	}
	p := &proc{cmd: cmd, done: make(chan struct{}), logs: []*lineLogger{outLog, errLog}}
	inst.adopt(port, p, s.now())
	// Record the child so a gateway that is killed outright can find and stop
	// it on the next boot. See orphan.go for why leaving one running matters.
	writePIDFile(inst.dir, pidRecord{
		PID: cmd.Process.Pid, Port: port, StartedAt: s.now(), Binary: bin,
	})
	slog.Info("instance starting",
		"user", inst.Username, "app", inst.App.Name,
		"pid", cmd.Process.Pid, "port", port, "db", inst.dbPath)
	go s.watch(inst, p)

	if err := s.waitReady(ctx, inst, p); err != nil {
		// Whatever went wrong, do not leave an orphan holding the port and
		// the database file.
		s.terminate(inst, p)
		return err
	}
	return nil
}

// waitReady polls the child's health path. Connection refused is the normal
// case for the first few hundred milliseconds while SQLite migrations run, so
// it is not an error — but a child that has already exited is, immediately,
// because polling a dead process for a minute helps nobody.
func (s *Supervisor) waitReady(ctx context.Context, inst *Instance, p *proc) error {
	target := "http://127.0.0.1:" + strconv.Itoa(inst.Port()) + healthPath(inst.App)
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return fmt.Errorf("build health request %s: %w", target, err)
		}
		resp, err := s.probe.Do(req)
		if err == nil {
			// Drain a little so the connection can be reused or closed
			// cleanly; the body of a health check is never interesting.
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-p.done:
			return fmt.Errorf("%w: %s", errChildExited, exitDetail(p))
		case <-inst.quit:
			return ErrShutdown
		case <-ctx.Done():
			return fmt.Errorf("health check %s: %w", target, ctx.Err())
		case <-ticker.C:
		}
	}
}

// watch reaps one child. An instance that dies after it was serving is removed
// from the table so the next request starts a fresh one; an instance that dies
// during startup is left to launch, which owns the retry loop and the error
// reported to waiting callers.
func (s *Supervisor) watch(inst *Instance, p *proc) {
	p.err = p.cmd.Wait()
	close(p.done)
	for _, l := range p.logs {
		l.flush() // Wait has drained the pipes; a trailing partial line is ours now
	}

	code := -1
	if p.cmd.ProcessState != nil {
		code = p.cmd.ProcessState.ExitCode()
	}
	if p.killed.Load() {
		slog.Info("instance stopped", "user", inst.Username, "app", inst.App.Name,
			"pid", p.cmd.Process.Pid, "exit_code", code)
		return
	}

	select {
	case <-inst.ready:
		// It was serving. This is a real crash.
	default:
		return // still starting; launch will report and retry
	}

	slog.Error("instance exited unexpectedly",
		"user", inst.Username, "app", inst.App.Name,
		"pid", p.cmd.Process.Pid, "exit_code", code, "error", p.err)
	s.noteCrash(instanceKey(inst.Username, inst.App.Name))
	s.forget(inst)
}

// stop asks a child to exit, then insists. Interrupt-then-kill rather than
// kill outright so an app with an in-flight sync push gets to finish it; five
// seconds is long enough for that and short enough that a gateway restart is
// not a coffee break.
func (s *Supervisor) stop(inst *Instance) error {
	inst.beginStop()
	p := inst.current()
	if p == nil {
		removePIDFile(inst.dir)
		return nil // never got as far as a process
	}
	err := s.terminate(inst, p)
	// The crash path clears this in forget(), but an orderly stop — a shutdown,
	// an idle timeout, an admin pressing Stop — never went through there, so
	// the record would outlive the process and the next boot would go hunting
	// a PID that has already been recycled by somebody else.
	removePIDFile(inst.dir)
	return err
}

func (s *Supervisor) terminate(inst *Instance, p *proc) error {
	select {
	case <-p.done:
		return nil // already gone
	default:
	}
	p.killed.Store(true)
	if err := interrupt(p.cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
		slog.Warn("interrupting instance", "user", inst.Username, "app", inst.App.Name, "error", err)
	}
	select {
	case <-p.done:
		return nil
	case <-time.After(stopGrace):
	}
	slog.Warn("instance ignored interrupt, killing",
		"user", inst.Username, "app", inst.App.Name, "grace", stopGrace)
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill %s/%s: %w", inst.Username, inst.App.Name, err)
	}
	<-p.done
	return nil
}

// forget removes an instance from the table, but only if it is still the
// current one: a slow crash report must never evict its own replacement.
func (s *Supervisor) forget(inst *Instance) {
	key := instanceKey(inst.Username, inst.App.Name)
	s.mu.Lock()
	if s.instances[key] == inst {
		delete(s.instances, key)
	}
	s.mu.Unlock()
	// The process is gone, so the record of it would only mislead the next
	// startup into hunting a PID that has moved on.
	removePIDFile(inst.dir)
}

func (s *Supervisor) noteCrash(key string) {
	s.mu.Lock()
	t := s.crashes[key]
	if t == nil {
		t = &crashTracker{}
		s.crashes[key] = t
	}
	t.record(s.now())
	s.mu.Unlock()
}

// ------------------------------------------------------------------- janitor

func (s *Supervisor) janitor() {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			for _, inst := range s.idleVictims(s.now()) {
				slog.Info("stopping idle instance",
					"user", inst.Username, "app", inst.App.Name,
					"idle_for", s.now().Sub(inst.LastUsed()).Round(time.Second))
				if err := s.Stop(inst.Username, inst.App.Name); err != nil {
					slog.Warn("stopping idle instance", "user", inst.Username, "app", inst.App.Name, "error", err)
				}
			}
		}
	}
}

// idleVictims collects under the lock and stops outside it.
func (s *Supervisor) idleVictims(now time.Time) []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Instance
	for _, inst := range s.instances {
		select {
		case <-inst.ready:
		default:
			continue // still starting: nothing has had a chance to use it yet
		}
		if inst.startErr != nil {
			continue
		}
		if shouldIdleStop(inst.App, inst.LastUsed(), now) {
			out = append(out, inst)
		}
	}
	return out
}

// shouldIdleStop is the whole idle policy, kept pure so it can be tested
// without a clock or a process. Always-on apps are never idle: workoutt's
// reminder ticker is inside the child, and an idle user is exactly the user
// who needs the reminder.
func shouldIdleStop(a *config.App, lastUsed, now time.Time) bool {
	if a == nil || a.AlwaysOn || a.IdleTimeout.D() <= 0 {
		return false
	}
	if lastUsed.IsZero() {
		return false
	}
	return now.Sub(lastUsed) >= a.IdleTimeout.D()
}

// ------------------------------------------------------------------ instances

// proc is one child process attempt. It exists separately from Instance
// because a launch may burn through several processes (port races) before one
// of them serves, and each needs its own exit channel.
type proc struct {
	cmd    *exec.Cmd
	done   chan struct{} // closed after Wait returns
	err    error         // set before done closes
	killed atomic.Bool   // we ended it on purpose; do not report a crash
	logs   []*lineLogger
}

// Instance is one running (user, app) pair.
type Instance struct {
	Username string
	App      *config.App

	dir    string
	dbPath string

	// ready is closed exactly once, when startup has concluded. startErr is
	// written before the close and read after it, which is the happens-before
	// edge that makes it safe without a lock.
	ready    chan struct{}
	startErr error

	quit     chan struct{}
	stopOnce sync.Once

	mu       sync.Mutex
	cur      *proc
	port     int
	pid      int
	url      *url.URL
	started  time.Time
	lastUsed time.Time
}

func newInstance(cfg *config.Config, username string, app *config.App, now time.Time) *Instance {
	dir := cfg.InstanceDir(username, app.Name)
	db := app.DBFile
	if db == "" {
		db = app.Name + ".db"
	}
	return &Instance{
		Username: username,
		App:      app,
		dir:      dir,
		dbPath:   filepath.Join(dir, db),
		ready:    make(chan struct{}),
		quit:     make(chan struct{}),
		started:  now,
		lastUsed: now,
	}
}

// URL is where the gateway proxies to. A copy, because a reverse proxy that
// mutates the URL it was handed must not mutate the supervisor's copy.
func (i *Instance) URL() *url.URL {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.url == nil {
		return nil
	}
	u := *i.url
	return &u
}

// Touch records activity. Every proxied request calls it; it is the only
// input to the idle timer.
func (i *Instance) Touch() {
	i.mu.Lock()
	i.lastUsed = time.Now()
	i.mu.Unlock()
}

// DBPath is the SQLite file this instance owns — the admin export path when
// the app has no /backup endpoint.
func (i *Instance) DBPath() string { return i.dbPath }

// Dir is the instance's private data directory.
func (i *Instance) Dir() string { return i.dir }

func (i *Instance) Port() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.port
}

func (i *Instance) PID() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.pid
}

func (i *Instance) LastUsed() time.Time {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.lastUsed
}

func (i *Instance) startedAt() time.Time {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.started
}

func (i *Instance) adopt(port int, p *proc, now time.Time) {
	i.mu.Lock()
	i.cur = p
	i.port = port
	i.pid = p.cmd.Process.Pid
	i.url = &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port)}
	i.started = now
	i.lastUsed = now
	i.mu.Unlock()
}

func (i *Instance) current() *proc {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.cur
}

func (i *Instance) finish(err error) {
	i.startErr = err
	close(i.ready)
}

func (i *Instance) beginStop() { i.stopOnce.Do(func() { close(i.quit) }) }

func (i *Instance) stopping() bool {
	select {
	case <-i.quit:
		return true
	default:
		return false
	}
}

func (i *Instance) status() Status {
	i.mu.Lock()
	defer i.mu.Unlock()
	return Status{
		Username:  i.Username,
		App:       i.App.Name,
		PID:       i.pid,
		Port:      i.port,
		StartedAt: i.started,
		LastUsed:  i.lastUsed,
	}
}

// --------------------------------------------------------------- crash damper

// crashTracker is the per-pair death record behind the backoff. Deaths outside
// the window are pruned on every use, so a long-running app that crashed twice
// last week is not one crash away from being held down.
type crashTracker struct {
	deaths []time.Time
	total  int
}

func (c *crashTracker) record(now time.Time) {
	c.total++
	c.deaths = append(c.deaths, now)
	c.prune(now)
}

func (c *crashTracker) prune(now time.Time) {
	cut := now.Add(-crashWindow)
	keep := c.deaths[:0]
	for _, t := range c.deaths {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	c.deaths = keep
}

// blockedUntil reports whether starts are currently refused, and until when.
// The pause runs from the most recent death rather than from the first, so a
// process that keeps dying keeps extending its own timeout.
func (c *crashTracker) blockedUntil(now time.Time) (time.Time, bool) {
	c.prune(now)
	if len(c.deaths) < crashThreshold {
		return time.Time{}, false
	}
	until := c.deaths[len(c.deaths)-1].Add(crashPause)
	if now.Before(until) {
		return until, true
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------------- env

// reservedEnv are the three variables the supervisor owns outright. They are
// the isolation boundary — an app that could set its own DB_PATH from
// apps.json could read another tenant's database — so config never wins here.
var reservedEnv = map[string]bool{"DB_PATH": true, "PORT": true, "STATIC_DIR": true}

// childVars computes the variables the supervisor sets on top of the inherited
// environment.
//
// VAPID_* is copied from the host on purpose, and last: those keys identify
// the application server to the push service, not the user. Generating a pair
// per database would mean every tenant's browser subscribing to a different
// application server, and rotating them would silently invalidate every
// existing subscription. One set of keys, injected from the host environment,
// shared by every instance.
func childVars(a *config.App, dbPath, distDir string, port int, host []string) map[string]string {
	vars := make(map[string]string, len(a.Env)+4)
	for k, v := range a.Env {
		k = envFold(strings.TrimSpace(k))
		if k == "" || reservedEnv[k] {
			continue
		}
		vars[k] = v
	}
	vars["DB_PATH"] = dbPath
	vars["PORT"] = strconv.Itoa(port)
	vars["STATIC_DIR"] = distDir
	for _, kv := range host {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if k = envFold(k); strings.HasPrefix(k, "VAPID_") {
			vars[k] = v
		}
	}
	return vars
}

// buildEnv merges vars over base, dropping any base entry that vars replaces.
// Duplicate keys in a child environment are resolved differently by different
// platforms, so this emits each name exactly once; the result is sorted after
// the inherited block for readable process listings.
func buildEnv(base []string, vars map[string]string) []string {
	out := make([]string, 0, len(base)+len(vars))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue // malformed; os/exec would ignore it too
		}
		if _, replaced := vars[envFold(k)]; replaced {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+vars[k])
	}
	return out
}

// --------------------------------------------------------------------- misc

// allocatePort asks the kernel for a free loopback port and immediately gives
// it back. There is an unavoidable window between the close and the child's
// bind — the alternative, passing an inherited listening socket, would require
// every app to speak systemd socket activation, which is a much bigger ask
// than retrying a lost race three times.
func allocatePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func instanceKey(username, app string) string { return username + "/" + app }

func healthPath(a *config.App) string {
	p := a.HealthPath
	if p == "" {
		p = "/healthz"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func exitDetail(p *proc) string {
	if p.cmd.ProcessState == nil {
		return "no exit status"
	}
	return fmt.Sprintf("exit code %d", p.cmd.ProcessState.ExitCode())
}

// lineLogger re-emits a child's output as gateway log records. Children log
// JSON to stdout; splicing that raw into the gateway's own stream would give
// operators two interleaved log formats and no idea which tenant produced a
// line. One record per line, tagged with user and app, is worth the copy.
type lineLogger struct {
	log   *slog.Logger
	level slog.Level
	emit  func(string)
	// tap, if set, receives every line in addition to the logger. It is
	// assigned once at spawn before the child exists, and only ever called
	// from the single os/exec copier goroutine, so it needs no lock.
	tap func(string)
	buf []byte
}

func newLineLogger(log *slog.Logger, level slog.Level) *lineLogger {
	l := &lineLogger{log: log, level: level}
	l.emit = l.write
	return l
}

func (l *lineLogger) write(line string) {
	l.log.Log(context.Background(), l.level, line)
}

// Write is called from the single goroutine os/exec dedicates to this stream,
// so it needs no lock of its own.
func (l *lineLogger) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.send(l.buf[:i])
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) >= maxLogLine {
		l.send(l.buf)
		l.buf = l.buf[:0]
	}
	return len(p), nil
}

// flush emits whatever the child left without a trailing newline. Safe to call
// once cmd.Wait has returned: Wait does not return until this stream's copier
// is done, which orders the last Write before this call.
func (l *lineLogger) flush() {
	if len(l.buf) > 0 {
		l.send(l.buf)
		l.buf = l.buf[:0]
	}
}

func (l *lineLogger) send(line []byte) {
	s := strings.TrimRight(string(line), "\r")
	if s == "" {
		return
	}
	l.emit(s)
	if l.tap != nil {
		l.tap(s)
	}
}
