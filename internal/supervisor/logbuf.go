// A short, in-memory tail of each instance's output.
//
// When somebody's app misbehaves, the useful question is "what did my instance
// say?" — and the honest answer today is "read the gateway's log and grep for
// your username", which is not something to ask of the person whose reading
// list will not sync. So every line a child writes is kept in a small ring
// buffer alongside being logged, and the owner can read the last few dozen.
//
// Deliberately in memory and deliberately small. These are diagnostics, not an
// audit trail: writing them to disk would mean rotation, retention, disk
// budgets and a new place for an app to leak something sensitive, in exchange
// for answering a question that is almost always about the last minute. A
// gateway restart losing them is the correct behaviour.
//
// The buffer is keyed by (user, app) rather than held on the Instance, so it
// survives a crash and restart — which is exactly when its contents matter
// most, since the lines explaining a crash are written by the process that
// then stops existing.
package supervisor

import (
	"sync"
	"time"
)

// LogCapacity is how many lines are kept per instance. Fifty is what the UI
// shows; keeping a little more means a restart in the middle of the window
// still leaves a full page of context.
const LogCapacity = 200

// LogLine is one line of child output.
type LogLine struct {
	At     time.Time
	Stream string // "stdout" or "stderr"
	Text   string
}

// logRing is a fixed-size circular buffer of the most recent lines.
type logRing struct {
	mu    sync.Mutex
	lines []LogLine
	next  int
	full  bool
}

func (r *logRing) add(l LogLine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lines == nil {
		r.lines = make([]LogLine, LogCapacity)
	}
	r.lines[r.next] = l
	r.next = (r.next + 1) % LogCapacity
	if r.next == 0 {
		r.full = true
	}
}

// tail returns up to n lines, oldest first.
func (r *logRing) tail(n int) []LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lines == nil {
		return nil
	}
	size := r.next
	if r.full {
		size = LogCapacity
	}
	if n > size {
		n = size
	}
	out := make([]LogLine, 0, n)
	// Walk back n positions from the write cursor, wrapping.
	start := ((r.next-n)%LogCapacity + LogCapacity) % LogCapacity
	for i := 0; i < n; i++ {
		out = append(out, r.lines[(start+i)%LogCapacity])
	}
	return out
}

// record appends a line to an instance's buffer, creating it on first use.
func (s *Supervisor) record(key, stream, text string) {
	s.logMu.Lock()
	ring := s.logs[key]
	if ring == nil {
		ring = &logRing{}
		s.logs[key] = ring
	}
	s.logMu.Unlock()
	ring.add(LogLine{At: s.now(), Stream: stream, Text: text})
}

// Logs returns the most recent output from one instance, oldest first. An
// instance that has never run returns nil rather than an error: "nothing yet"
// is a normal answer, not a failure.
func (s *Supervisor) Logs(username, app string, n int) []LogLine {
	s.logMu.Lock()
	ring := s.logs[instanceKey(username, app)]
	s.logMu.Unlock()
	if ring == nil {
		return nil
	}
	return ring.tail(n)
}

// ForgetLogs drops an instance's buffer. Called when a user removes an app or
// an account is deleted, so a stopped tenant's output does not sit in memory
// indefinitely.
func (s *Supervisor) ForgetLogs(username, app string) {
	s.logMu.Lock()
	delete(s.logs, instanceKey(username, app))
	s.logMu.Unlock()
}
