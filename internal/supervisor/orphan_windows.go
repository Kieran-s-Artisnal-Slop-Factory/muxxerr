//go:build windows

package supervisor

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// killOrphan verifies a recorded process really is the child we started before
// killing it.
//
// On Windows os.FindProcess always succeeds regardless of whether the PID
// exists, so it proves nothing. `tasklist` is the dependency-free way to ask
// whether a PID is live and what image it is running; matching the image name
// against the binary we launched is the identity check that stops us killing a
// stranger that inherited a recycled PID.
func killOrphan(rec pidRecord) orphanResult {
	image, alive := processImage(rec.PID)
	if !alive {
		return orphanGone
	}
	want := baseName(rec.Binary)
	if want != "" && !strings.EqualFold(image, want) {
		return orphanNotOurs
	}
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(rec.PID), "/F", "/T")
	if err := cmd.Run(); err != nil {
		return orphanNotOurs
	}
	// taskkill returns as soon as the request is made; give the handle a moment
	// to actually close so the database file is free before we reopen it.
	time.Sleep(150 * time.Millisecond)
	return orphanKilled
}

// processImage returns the executable name for a live PID.
func processImage(pid int) (string, bool) {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH", "/FO", "CSV").Output()
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(out))
	// A miss prints an informational sentence rather than a CSV row, and
	// returns exit status 0 either way — so the shape of the output is the
	// only reliable signal.
	if line == "" || !strings.HasPrefix(line, `"`) {
		return "", false
	}
	fields := strings.SplitN(line, `","`, 2)
	return strings.Trim(fields[0], `"`), true
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
