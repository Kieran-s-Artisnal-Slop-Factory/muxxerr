//go:build !windows

package supervisor

import (
	"os"
	"strings"
	"syscall"
	"time"
)

// killOrphan verifies a recorded process really is the child we started before
// killing it.
//
// Signal 0 is the portable liveness probe: it performs the permission and
// existence checks without delivering anything. For identity, /proc/<pid>/comm
// names the running executable on Linux; where that is unavailable (macOS,
// BSD) we fall back to killing on liveness alone. That is a slightly weaker
// check, but the window for PID reuse to matter is the gap between two runs of
// this gateway on the same machine, and the file we are protecting is one this
// gateway owns.
func killOrphan(rec pidRecord) orphanResult {
	p, err := os.FindProcess(rec.PID)
	if err != nil {
		return orphanGone
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return orphanGone
	}
	if want := baseName(rec.Binary); want != "" {
		if comm, ok := processComm(rec.PID); ok && !strings.HasPrefix(want, comm) && comm != want {
			return orphanNotOurs
		}
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return orphanNotOurs
	}
	// Give it a moment to close its database handles, then insist.
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		if err := p.Signal(syscall.Signal(0)); err != nil {
			return orphanKilled
		}
	}
	_ = p.Kill()
	return orphanKilled
}

// processComm reads the executable name Linux records for a pid. The value is
// truncated to 15 characters, which is why the caller compares with a prefix.
func processComm(pid int) (string, bool) {
	blob, err := os.ReadFile("/proc/" + itoa(pid) + "/comm")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(blob)), true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
