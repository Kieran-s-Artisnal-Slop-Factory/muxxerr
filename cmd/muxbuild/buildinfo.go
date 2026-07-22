// Capturing each app's version and commit at build time.
//
// The dashboard badges every app with "which build is this?", and this is where
// that information is gathered: the commit from the app's own git checkout (if
// git is available and the source is a repository), and the version from a
// VERSION file in the app's source root (if it has one). Both are best-effort.
// An app with neither yields an empty AppBuild, muxbuild still writes the file,
// and the gateway simply shows no badge — the UI is unchanged when there is
// nothing to show.
//
// git is deliberately optional here. muxbuild already requires git only for
// git+ sources; capturing a commit must never turn a working build into a
// failing one, so every git error is swallowed and treated as "no commit".
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"muxxerr/internal/version"
)

// captureAppBuild inspects an app's source tree for its version and commit.
//
// Commit resolution is git first (the accurate, automatic source), then a plain
// COMMIT file as a fallback. The fallback exists for environments where git
// cannot run against the source — most importantly the Docker image, whose
// .dockerignore drops every .git — but where a hash can still be written to a
// file that survives into the build context. Version is always a VERSION file,
// since there is no git equivalent of "the release name".
func captureAppBuild(ctx context.Context, sourceDir string) version.AppBuild {
	b := version.AppBuild{BuiltAt: time.Now().UTC().Format(time.RFC3339)}
	b.Version = readShortFile(sourceDir, "VERSION")
	if full, dirty, ok := gitCommit(ctx, sourceDir); ok {
		b.CommitFull = full
		b.Commit = short(full)
		b.Dirty = dirty
	} else if c := readShortFile(sourceDir, "COMMIT"); c != "" {
		b.CommitFull = c
		b.Commit = short(c)
	}
	return b
}

func short(full string) string {
	if len(full) > 9 {
		return full[:9]
	}
	return full
}

// readShortFile returns the trimmed first line of a named file in dir, or "" if
// there is none. A length cap keeps a stray large or binary file from becoming a
// nonsense badge.
func readShortFile(dir, name string) string {
	blob, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	line := blob
	if i := bytes.IndexByte(blob, '\n'); i >= 0 {
		line = blob[:i]
	}
	v := strings.TrimSpace(string(line))
	if len(v) > 64 {
		return ""
	}
	return v
}

// gitCommit returns the full HEAD hash and whether the tree has uncommitted
// changes to tracked files. ok is false when the directory is not a git
// checkout, git is not installed, or anything else goes wrong.
func gitCommit(ctx context.Context, dir string) (full string, dirty, ok bool) {
	full, ok = runGit(ctx, dir, "rev-parse", "HEAD")
	if !ok || full == "" {
		return "", false, false
	}
	// -uno: ignore untracked files (node_modules, editor droppings). A build is
	// "dirty" only if a tracked file was modified, which is what a reader of the
	// badge cares about.
	status, statusOK := runGit(ctx, dir, "status", "--porcelain", "-uno")
	dirty = statusOK && status != ""
	return full, dirty, true
}

func runGit(ctx context.Context, dir string, args ...string) (string, bool) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	var out bytes.Buffer
	cmd.Stdout = &out
	// git chatters on stderr for the not-a-repository case; that is an expected
	// outcome here, not something to surface.
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return strings.TrimSpace(out.String()), true
}

// writeBuildInfo captures and persists an app's build metadata to
// runtime/apps/<name>/build.json. Best-effort: a failure here is noted in the
// build log but never fails the build, because a missing badge is a cosmetic
// loss and a failed build is not.
func (t *task) writeBuildInfo(ctx context.Context) {
	t.buildInfo = captureAppBuild(ctx, t.cfg.SourceDir(t.app))
	if err := os.MkdirAll(t.cfg.AppRuntimeDir(t.app), 0o755); err != nil {
		fmt.Fprintf(t.out, "# build.json: could not create runtime dir: %v\n", err)
		return
	}
	if err := version.WriteAppBuild(t.cfg.AppBuildPath(t.app), t.buildInfo); err != nil {
		fmt.Fprintf(t.out, "# build.json: %v\n", err)
		return
	}
	if !t.buildInfo.Empty() {
		fmt.Fprintf(t.out, "# build.json: %s\n", t.buildInfo.Badge())
	}
}

// writeChangelog renders the app's CHANGELOG.md (if any) to HTML for the
// dashboard's release-notes modal. No CHANGELOG.md means any previously rendered
// one is removed, so deleting a changelog removes the button — the dashboard
// only offers it when there is something to show.
func (t *task) writeChangelog() {
	src := t.cfg.SourceDir(t.app)
	out := t.cfg.AppChangelogPath(t.app)

	md, err := os.ReadFile(filepath.Join(src, "CHANGELOG.md"))
	if err != nil {
		_ = os.Remove(out) // absent (or unreadable): make sure no stale copy lingers
		return
	}
	htmlOut := renderChangelogHTML(string(md))
	if err := os.MkdirAll(t.cfg.AppRuntimeDir(t.app), 0o755); err != nil {
		fmt.Fprintf(t.out, "# changelog.html: %v\n", err)
		return
	}
	if err := os.WriteFile(out, []byte(htmlOut), 0o644); err != nil {
		fmt.Fprintf(t.out, "# changelog.html: %v\n", err)
		return
	}
	t.hasChangelog = true
	fmt.Fprintf(t.out, "# changelog.html: %d bytes from CHANGELOG.md\n", len(htmlOut))
}
