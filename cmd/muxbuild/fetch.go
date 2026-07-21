package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"muxerr/internal/config"
)

// Fetching a `git+` source.
//
// This is the only part of the build that touches the network, and it is the
// only reason `git` is a build dependency at all — a submodule checkout or a
// sibling directory needs nothing.
//
// The clone is deliberately shallow and the update is deliberately a hard
// reset. Neither is a working copy anybody should be editing: it is a build
// input under runtime/, and a merge conflict there would be a confusing way to
// fail a build. If you want to work on an app, point `source` at a checkout you
// control instead.

// fetchSource makes an app's remote source present at its cache directory and
// returns the commit that was checked out, for the build log.
func fetchSource(ctx context.Context, cfg *config.Config, app *config.App, gitBin string, out *strings.Builder) (string, error) {
	rem, err := app.Remote()
	if err != nil {
		return "", err
	}
	dir := cfg.CacheDir(app)

	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, gitBin, args...)
		if dirExists(dir) {
			cmd.Dir = dir
		}
		// GIT_TERMINAL_PROMPT=0 turns a private repo from "hangs forever
		// waiting for a username nobody is there to type" into an error.
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		blob, err := cmd.CombinedOutput()
		text := strings.TrimSpace(string(blob))
		if err != nil {
			return text, fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, text)
		}
		return text, nil
	}

	if !dirExists(filepath.Join(dir, ".git")) {
		// A stale non-repo directory from an interrupted build would make every
		// later command fail confusingly. Start clean.
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("clear %s: %w", dir, err)
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		args := []string{"clone", "--depth", "1"}
		if rem.Ref != "" {
			// --branch takes a tag as well as a branch. A raw commit SHA is
			// not accepted here, which is why the fallback below exists.
			args = append(args, "--branch", rem.Ref)
		}
		args = append(args, rem.URL, dir)
		fmt.Fprintf(out, "# cloning %s\n", rem.URL)
		if _, err := run(args...); err != nil {
			if rem.Ref == "" {
				return "", err
			}
			// Probably a commit SHA rather than a branch or tag. Clone the
			// default branch, then fetch that one object.
			fmt.Fprintf(out, "# %q is not a branch or tag; fetching it as a commit\n", rem.Ref)
			if _, err := run("clone", rem.URL, dir); err != nil {
				return "", err
			}
			if _, err := run("fetch", "--depth", "1", "origin", rem.Ref); err != nil {
				return "", err
			}
			if _, err := run("checkout", "--detach", "FETCH_HEAD"); err != nil {
				return "", err
			}
		}
	} else {
		// Already cloned: move it to the current tip of the requested ref.
		fmt.Fprintf(out, "# updating %s\n", rem.URL)
		ref := rem.Ref
		if ref == "" {
			// Whatever the remote calls its default branch — readerr uses
			// main and workoutt uses master, so this cannot be hardcoded.
			head, err := run("symbolic-ref", "--short", "refs/remotes/origin/HEAD")
			if err != nil {
				if _, err := run("remote", "set-head", "origin", "--auto"); err != nil {
					return "", err
				}
				head, err = run("symbolic-ref", "--short", "refs/remotes/origin/HEAD")
				if err != nil {
					return "", err
				}
			}
			ref = strings.TrimPrefix(head, "origin/")
		}
		if _, err := run("fetch", "--depth", "1", "origin", ref); err != nil {
			return "", err
		}
		if _, err := run("checkout", "--detach", "FETCH_HEAD"); err != nil {
			return "", err
		}
		// A build input is not a working copy. Anything left behind by a
		// previous build — a half-written dist, an untracked file — would
		// otherwise be compiled into this one.
		if _, err := run("reset", "--hard", "FETCH_HEAD"); err != nil {
			return "", err
		}
		if _, err := run("clean", "-fdx", "-e", "node_modules"); err != nil {
			return "", err
		}
	}

	commit, err := run("rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "# at %s\n", commit)
	return commit, nil
}

// fetchRemoteSources brings every git+ app in this build up to date.
//
// Sequential rather than concurrent: git writes progress to the same terminal,
// and two clones interleaving their output to say nothing of two of them
// prompting for credentials at once is worse than waiting. There is rarely
// more than one.
func fetchRemoteSources(cfg *config.Config, apps []*config.App, verbose bool) error {
	remotes := make([]*config.App, 0, len(apps))
	for _, a := range apps {
		if a.IsRemote() {
			remotes = append(remotes, a)
		}
	}
	if len(remotes) == 0 {
		return nil
	}

	gitBin, err := lookTool("git")
	if err != nil {
		names := make([]string, len(remotes))
		for i, a := range remotes {
			names[i] = a.Name
		}
		return fmt.Errorf(
			"missing build tools:\n  - git — needed to fetch the git+ sources for %s.\n"+
				"    Install git and make sure it is on PATH, or point those apps at a local\n"+
				"    checkout or a submodule instead of a git+ URL.",
			strings.Join(names, ", "))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	for _, a := range remotes {
		var out strings.Builder
		commit, err := fetchSource(ctx, cfg, a, gitBin, &out)
		if verbose || err != nil {
			fmt.Fprint(os.Stderr, indentBlock(a.Name, out.String()))
		}
		if err != nil {
			return fmt.Errorf("fetching %s: %w", a.Name, err)
		}
		slog.Info("fetched source", "app", a.Name, "commit", commit)
	}
	return nil
}

// indentBlock prefixes a child's output so it is obviously subordinate to the
// app it belongs to, matching how build output is presented.
func indentBlock(name, text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", name)
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		fmt.Fprintf(&b, "    | %s\n", line)
	}
	return b.String()
}
