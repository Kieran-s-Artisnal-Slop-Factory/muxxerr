//go:build windows

// Windows half of the two platform differences the supervisor cares about:
// how you ask a child to stop, and whether environment variable names are
// case sensitive.
package supervisor

import (
	"os"
	"strings"
)

// interrupt stops the child. On Windows os.Process.Signal(os.Interrupt) is not
// implemented — it returns "not supported by windows" and the child keeps
// running — and the alternative, attaching to the child's console and raising
// CTRL_BREAK, means either sharing a console group (so Ctrl-C in the terminal
// kills every tenant at once) or a chunk of syscall plumbing for one platform.
// So: kill it. The cost is that the child gets no shutdown hook, which is
// tolerable here because SQLite in WAL mode recovers from a killed writer on
// the next open, and an interrupted sync push is retried by the client.
func interrupt(p *os.Process) error { return p.Kill() }

// envFold normalises an environment variable name for comparison. Windows
// environment names are case insensitive, so PATH and Path are one variable
// and setting both produces a child with an unpredictable one.
func envFold(name string) string { return strings.ToUpper(name) }
