# Versioning: the splash and the dashboard badges

Two build identities are surfaced, and both follow the same rule — **show it if
it is available, change nothing if it is not.**

1. **The gateway's own** version and commit, in the startup splash.
2. **Each app's** version and commit, as a badge on its dashboard card.

## The gateway (the startup splash)

`mux` prints a small banner to **stderr** at startup (stdout stays pure JSON for
log shippers), and repeats the same facts in the structured `muxxerr listening`
log line:

```
    *
   /|\    muxxerr  ·  v0.1.0 (8b9f590, dirty)
  * * *   an auth gateway for local-first apps
```

### Version — where to update it

The gateway's version is the contents of one file:

```
internal/version/VERSION
```

**That is the single place to bump it.** It is embedded into the binary at
compile time (`go:embed`), so changing it and rebuilding is the whole procedure.
It shows as `v<contents>`.

### Commit — automatic where it can be

The commit is resolved in this order:

1. **A linker override** — `internal/version.gatewayCommit`, set with
   `-ldflags`. Used where the Go build cannot see git (below).
2. **The Go build's embedded VCS stamp** — automatic for a normal
   `go build ./cmd/mux` inside a git checkout, including the dirty flag. (`go run`
   does not always embed it; a real build always does.)
3. **Nothing** — the splash then shows just the version. No error, no blank
   `()`.

To stamp a commit into a build that has no git context:

```bash
go build -ldflags "-X muxxerr/internal/version.gatewayCommit=$(git rev-parse HEAD)" ./cmd/mux
```

**In the Docker image** this is already wired: `.dockerignore` drops every `.git`,
so the build cannot read the VCS stamp, and CI passes the commit as a build-arg
instead — `MUX_COMMIT=${{ github.sha }}` in
[.github/workflows/docker.yaml](../../.github/workflows/docker.yaml), consumed by
`ARG MUX_COMMIT` in the [Dockerfile](../../Dockerfile). Building the image
yourself without that arg just omits the commit.

## The apps (the dashboard badge)

Each app's card on the dashboard shows a small badge with its version and/or
commit — `1.4.0 · 3035d7f2a`, or just `3035d7f2a`, or just `1.4.0`. **An app with
no version and no commit gets no badge, and its card looks exactly as it did
before.**

### How it works

`muxbuild` captures the information at build time and writes it beside the app's
other build output:

```
runtime/apps/<name>/build.json
```

```json
{
  "commit": "3035d7f2a",
  "commit_full": "3035d7f2a6b191fcab084bdcaeedc504aa85e4f9",
  "version": "1.4.0",
  "built_at": "2026-07-22T16:07:08Z"
}
```

The gateway reads every `build.json` once at startup and badges the dashboard
(`internal/web`, `internal/version.AppBuild`). The `BUILD` column in `muxbuild`'s
own summary table shows the same value, so you can see what was captured without
starting the gateway. The full commit and build time appear in the badge's hover
tooltip; the badge itself stays short.

### Where each field comes from

| Field | Source | Notes |
|---|---|---|
| commit | `git -C <app source> rev-parse HEAD` | the normal from-source path; works for submodules and `git+` sources |
| commit (fallback) | a `COMMIT` file in the app's source root | used only when git cannot run — see Docker below |
| version | a `VERSION` file in the app's source root | optional; shown **verbatim** (write `v1.4.0` if you want the `v`) |

So, to **add a version** to an app, put a `VERSION` file in that app's repository
root:

```bash
echo "1.4.0" > apps/readerr/VERSION
go run ./cmd/muxbuild -config apps.json -only readerr
```

The commit needs nothing — it is read from the app's git checkout automatically.

### Making the badges work in the Docker image

Inside the image, `git` cannot be used against the app sources (`.dockerignore`
drops every `.git`, deliberately — see the [deployment doc](../admin/deployment.md#building-the-image-and-how-the-multi-arch-cross-compile-works)).
Plain files survive into the build context, so:

- **Version:** a `VERSION` file in the app repo works unchanged — it is read from
  disk, not from git.
- **Commit:** the CI workflow writes a `COMMIT` file per submodule before the
  build (`git submodule foreach '… rev-parse HEAD > COMMIT'`), and `muxbuild`
  falls back to it. If you build the image yourself and want commit badges,
  either write those `COMMIT` files first, or build the submodules out and pass
  them in the same way.

`COMMIT` files are build-time artifacts, not something to commit to an app repo
(git already knows the commit); keep them gitignored in the app if you generate
them locally.

## Release notes (a `CHANGELOG.md`)

If an app's source root has a **`CHANGELOG.md`**, its card on the dashboard gets a
**Release notes** button that opens the changelog in a modal (`<dialog>`). No
`CHANGELOG.md` means no button, and the card is unchanged.

### How it works

`muxbuild` renders `CHANGELOG.md` to HTML **at build time** and writes it to
`runtime/apps/<name>/changelog.html`; the gateway reads that and injects it into
a `<dialog>` on the dashboard, opened by a small script (the close button is a
`<form method="dialog">` and needs no script). Removing the `CHANGELOG.md` and
rebuilding removes the rendered file, so the button disappears.

### What "rendered" means here

The converter (`cmd/muxbuild/changelog.go`) is deliberately small — headings,
lists, paragraphs, thematic breaks, fenced and inline code, and inline
bold/italic/links. It is **not** a full markdown engine: nested lists, tables and
the like render flat. Keeping the changelog coherent is the app author's job.

The one hard rule is safety. The changelog is injected into the gateway's own
trusted page, so **every character of the source is HTML-escaped first** and the
converter only emits its own fixed set of tags; link targets are scheme-checked
(`javascript:` and friends are dropped, keeping the visible text). The gateway
additionally refuses any rendered file containing a `<script`, as defence in
depth. A `CHANGELOG.md` cannot inject markup or script into the dashboard.

### Authoring

Put a `CHANGELOG.md` in the app's source root — the same place as `VERSION`:

```markdown
# 1.4.0 — 2026-08-01

- The thing you fixed
- The thing you added, see [#42](https://github.com/you/app/pull/42)

# 1.3.0 — 2026-07-10

- ...
```

Then rebuild (`go run ./cmd/muxbuild -config apps.json -only <name>`); `muxbuild`
logs `changelog.html: <n> bytes` for apps that have one. In the Docker image it
works the same way as a `VERSION` file — a plain file read from the build
context, no git needed.

## Quick reference

| Surface | Version from | Commit from | Absent → |
|---|---|---|---|
| Gateway splash / log | `internal/version/VERSION` (embedded) | Go VCS stamp, or `-ldflags`/`MUX_COMMIT` | version only |
| App dashboard badge | `<app>/VERSION` file | app's git, or a `<app>/COMMIT` file | no badge |
| App "Release notes" modal | — | — | rendered from `<app>/CHANGELOG.md`; no file → no button |
