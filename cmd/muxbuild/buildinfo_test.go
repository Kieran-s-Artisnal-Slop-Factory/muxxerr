package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A source directory with no git but with VERSION and COMMIT files is exactly
// the Docker case: .dockerignore drops .git, and CI writes the commit to a file.
// captureAppBuild must fall back to those files.
func TestCaptureAppBuildFileFallback(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "VERSION"), "2.3.1\n")
	writeFile(t, filepath.Join(dir, "COMMIT"), "abcdef1234567890\n")

	b := captureAppBuild(context.Background(), dir)
	if b.Version != "2.3.1" {
		t.Errorf("Version = %q, want 2.3.1", b.Version)
	}
	if b.Commit != "abcdef123" {
		t.Errorf("Commit = %q, want abcdef123 (short)", b.Commit)
	}
	if b.CommitFull != "abcdef1234567890" {
		t.Errorf("CommitFull = %q, want the full file contents", b.CommitFull)
	}
	if b.BuiltAt == "" {
		t.Error("BuiltAt is empty; it should always be stamped")
	}
}

// No version and no commit anywhere: an empty build, which the dashboard renders
// as no badge at all.
func TestCaptureAppBuildNothingAvailable(t *testing.T) {
	b := captureAppBuild(context.Background(), t.TempDir())
	if !b.Empty() {
		t.Fatalf("expected empty build, got %+v", b)
	}
}

func TestReadShortFileCaps(t *testing.T) {
	dir := t.TempDir()
	// Multi-line: only the first line, trimmed.
	writeFile(t, filepath.Join(dir, "VERSION"), "  1.0.0  \nignored second line\n")
	if got := readShortFile(dir, "VERSION"); got != "1.0.0" {
		t.Errorf("readShortFile = %q, want 1.0.0", got)
	}
	// Absurdly long first line is rejected rather than shown.
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'x'
	}
	writeFile(t, filepath.Join(dir, "COMMIT"), string(long))
	if got := readShortFile(dir, "COMMIT"); got != "" {
		t.Errorf("readShortFile(overlong) = %q, want empty", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
