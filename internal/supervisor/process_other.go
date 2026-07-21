//go:build !windows

// Unix half of the two platform differences the supervisor cares about:
// how you ask a child to stop, and whether environment variable names are
// case sensitive.
package supervisor

import "os"

// interrupt asks the child to shut down. SIGINT rather than SIGTERM because
// that is what Ctrl-C sends, so it is the signal an app author is most likely
// to have actually handled; either way the caller escalates to Kill after the
// grace period.
func interrupt(p *os.Process) error { return p.Signal(os.Interrupt) }

// envFold normalises an environment variable name for comparison. Unix
// environments are case sensitive, so this is the identity.
func envFold(name string) string { return name }
