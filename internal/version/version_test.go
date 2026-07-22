package version

import (
	"path/filepath"
	"testing"
)

func TestGatewayVersionFromEmbeddedFile(t *testing.T) {
	// The VERSION file next to this package is the single source of truth. If it
	// is ever emptied, "dev" is the floor rather than "".
	if got := GatewayVersion(); got == "" {
		t.Fatal("GatewayVersion() is empty; it must never be")
	}
}

func TestGatewayLabelIncludesVersion(t *testing.T) {
	label := GatewayLabel()
	if label == "" || label[0] != 'v' {
		t.Fatalf("GatewayLabel() = %q, want a v-prefixed version", label)
	}
}

func TestBannerMentionsName(t *testing.T) {
	b := Banner()
	if !contains(b, "muxxerr") {
		t.Fatalf("Banner() does not name muxxerr:\n%s", b)
	}
}

func TestAppBuildBadge(t *testing.T) {
	cases := []struct {
		name string
		b    AppBuild
		want string
		empt bool
	}{
		{"both", AppBuild{Version: "1.4.0", Commit: "3035d7f2a"}, "1.4.0 · 3035d7f2a", false},
		{"commit only", AppBuild{Commit: "3035d7f2a"}, "3035d7f2a", false},
		{"version only", AppBuild{Version: "1.4.0"}, "1.4.0", false},
		{"verbatim v", AppBuild{Version: "v2.0"}, "v2.0", false},
		{"date version", AppBuild{Version: "2024-06"}, "2024-06", false},
		{"dirty", AppBuild{Commit: "abc1234"}, "abc1234", false},
		{"dirty flag", AppBuild{Version: "1.0", Commit: "abc1234", Dirty: true}, "1.0 · abc1234-dirty", false},
		{"empty", AppBuild{}, "", true},
		{"builtAt only is still empty", AppBuild{BuiltAt: "2026-01-01T00:00:00Z"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.b.Badge(); got != c.want {
				t.Errorf("Badge() = %q, want %q", got, c.want)
			}
			if got := c.b.Empty(); got != c.empt {
				t.Errorf("Empty() = %v, want %v", got, c.empt)
			}
		})
	}
}

func TestAppBuildTooltip(t *testing.T) {
	b := AppBuild{CommitFull: "3035d7f2a6b191fc", Dirty: true, BuiltAt: "2026-07-22T16:07:08Z"}
	got := b.Tooltip()
	for _, want := range []string{"commit 3035d7f2a6b191fc", "uncommitted changes", "built 2026-07-22T16:07:08Z"} {
		if !contains(got, want) {
			t.Errorf("Tooltip() = %q, missing %q", got, want)
		}
	}
}

func TestReadWriteAppBuildRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "build.json")
	in := AppBuild{Version: "1.4.0", Commit: "3035d7f2a", CommitFull: "3035d7f2a6b191fc", Dirty: true, BuiltAt: "2026-07-22T16:07:08Z"}
	if err := WriteAppBuild(path, in); err != nil {
		t.Fatalf("WriteAppBuild: %v", err)
	}
	out := ReadAppBuild(path)
	if out != in {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestReadAppBuildMissingIsZero(t *testing.T) {
	// A missing or malformed file must be the zero value, never an error, so the
	// dashboard simply shows no badge.
	if b := ReadAppBuild(filepath.Join(t.TempDir(), "nope.json")); !b.Empty() {
		t.Fatalf("ReadAppBuild(missing) = %+v, want empty", b)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
