// Package version is the single source of truth for "what build is this?" —
// for the gateway itself (the startup splash) and for each app muxbuild
// compiles (the per-app badge on the dashboard).
//
// TWO DISTINCT THINGS LIVE HERE, do not confuse them:
//
//   - The gateway's OWN version and commit. The version is the string in the
//     VERSION file beside this source, embedded at compile time — that file is
//     the one place you bump muxxerr's version. The commit is read from the Go
//     build's embedded VCS stamp (automatic for `go build`/`go run` inside a git
//     checkout) or injected with -ldflags for builds where .git is absent, such
//     as the Docker image. See Gateway* below.
//
//   - Each APP's version and commit, captured by muxbuild at build time from the
//     app's own source tree and written to runtime/apps/<name>/build.json. The
//     gateway reads those files to badge the dashboard. See AppBuild below.
package version

import (
	_ "embed"
	"encoding/json"
	"os"
	"runtime/debug"
	"strings"
)

// gatewayVersion is the muxxerr version, embedded from the VERSION file next to
// this source. **This is the one place to update the gateway's version number.**
//
//go:embed VERSION
var gatewayVersion string

// gatewayCommit may be set at link time for builds where the VCS stamp is not
// available — most importantly the Docker image, whose build context has no
// .git. Wire it with:
//
//	go build -ldflags "-X muxxerr/internal/version.gatewayCommit=$(git rev-parse HEAD)"
//
// When it is empty, the commit is read from the Go build info instead.
var gatewayCommit string

// GatewayVersion is muxxerr's own version, e.g. "0.1.0". "dev" if the VERSION
// file was somehow empty.
func GatewayVersion() string {
	v := strings.TrimSpace(gatewayVersion)
	if v == "" {
		return "dev"
	}
	return v
}

// GatewayCommit returns the gateway's build commit (short) and whether the tree
// was dirty. Both empty/false when no VCS information is available — a Docker
// build with no .git and no -ldflags override, for instance. "If available".
func GatewayCommit() (short string, dirty bool) {
	if c := strings.TrimSpace(gatewayCommit); c != "" {
		return shortHash(c), false
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	var full string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			full = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return shortHash(full), dirty
}

// GatewayLabel is the one-line descriptor shown in the splash: "v0.1.0",
// "v0.1.0 (3035d7f)", or "v0.1.0 (3035d7f, dirty)".
func GatewayLabel() string {
	label := "v" + GatewayVersion()
	if short, dirty := GatewayCommit(); short != "" {
		if dirty {
			label += " (" + short + ", dirty)"
		} else {
			label += " (" + short + ")"
		}
	}
	return label
}

// Banner is the startup splash: a small ASCII rendering of muxxerr's mark — one
// node branching into several, which is exactly what the gateway is — the
// wordmark, and the build descriptor. Deliberately plain ASCII so it renders in
// any terminal and in captured container logs.
func Banner() string {
	return "\n" +
		"    *\n" +
		"   /|\\    muxxerr  ·  " + GatewayLabel() + "\n" +
		"  * * *   an auth gateway for local-first apps\n"
}

// ---------------------------------------------------------------- per-app build

// AppBuild is one app's build metadata, written by muxbuild to
// runtime/apps/<name>/build.json and read by the gateway to badge the
// dashboard. Every field is omitempty: a build with no git and no VERSION file
// serialises to "{}", the badge is not shown, and the dashboard is unchanged.
type AppBuild struct {
	// Version is the contents of a VERSION file in the app's source root, if it
	// has one. Free-form ("1.4.0", "v1.4.0", "2024-06"); shown verbatim.
	Version string `json:"version,omitempty"`
	// Commit is the short hash of the app source at build time.
	Commit string `json:"commit,omitempty"`
	// CommitFull is the full hash, surfaced only in the badge's hover tooltip.
	CommitFull string `json:"commit_full,omitempty"`
	// Dirty is true if the app source had uncommitted changes when built.
	Dirty bool `json:"dirty,omitempty"`
	// BuiltAt is an RFC3339 timestamp, for the tooltip. Not shown in the badge.
	BuiltAt string `json:"built_at,omitempty"`
}

// Empty reports whether there is nothing worth showing — no version and no
// commit. The dashboard shows no badge in that case, per the requirement to
// leave the UI unchanged when no build information is available.
func (b AppBuild) Empty() bool {
	return strings.TrimSpace(b.Version) == "" && strings.TrimSpace(b.Commit) == ""
}

// Badge is the short label rendered on the dashboard, e.g. "1.4.0 · 3035d7f",
// "3035d7f" (no VERSION file), or "1.4.0" (no git). The version is shown exactly
// as written in the app's VERSION file — write "v1.4.0" there if you want the v.
// Empty when Empty().
func (b AppBuild) Badge() string {
	var parts []string
	if v := strings.TrimSpace(b.Version); v != "" {
		parts = append(parts, v)
	}
	if c := strings.TrimSpace(b.Commit); c != "" {
		if b.Dirty {
			c += "-dirty"
		}
		parts = append(parts, c)
	}
	return strings.Join(parts, " · ")
}

// Tooltip is the hover text: the full commit and the build time when present.
func (b AppBuild) Tooltip() string {
	var parts []string
	if c := strings.TrimSpace(b.CommitFull); c != "" {
		parts = append(parts, "commit "+c)
	}
	if b.Dirty {
		parts = append(parts, "uncommitted changes")
	}
	if t := strings.TrimSpace(b.BuiltAt); t != "" {
		parts = append(parts, "built "+t)
	}
	return strings.Join(parts, " · ")
}

// ReadAppBuild loads build.json, returning the zero value on any problem —
// missing file, unreadable, malformed. The caller renders no badge for a zero
// value, so a stale or absent file simply means "no build info", never an error.
func ReadAppBuild(path string) AppBuild {
	blob, err := os.ReadFile(path)
	if err != nil {
		return AppBuild{}
	}
	var b AppBuild
	if err := json.Unmarshal(blob, &b); err != nil {
		return AppBuild{}
	}
	return b
}

// WriteAppBuild writes build.json. muxbuild calls this after a successful build.
func WriteAppBuild(path string, b AppBuild) error {
	blob, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(blob, '\n'), 0o644)
}

func shortHash(full string) string {
	full = strings.TrimSpace(full)
	if len(full) > 9 {
		return full[:9]
	}
	return full
}
