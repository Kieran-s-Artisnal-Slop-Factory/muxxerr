# Upstream patches

**None of these are applied. Muxerr works with workoutt and readerr
exactly as they are shipped today.** This directory is a set of small, optional
changes to the *apps* that make muxerr's job cleaner, cheaper and less
surprising. They are written as prose + diff hunks rather than `.patch` files
on purpose: they are one-line-ish edits in files that change often upstream, and
a rejected hunk is a worse experience than three seconds of manual editing.

## Why there is anything to patch at all

The apps were built as single-tenant, single-origin, root-mounted local-first
apps. Two of those assumptions bite when you mount many copies of them under
`/<user>/<app>/` on one origin:

1. **The API calls are root-absolute.** `getSyncUrl()` returns `''` when no
   sync URL is configured, so the client fetches `/sync/pull`, not
   `/alice/readerr/sync/pull`. Correct when the Go backend is the origin;
   ambiguous when it isn't.
2. **The service worker owns the whole origin's cache namespace.** Its
   `activate` handler deletes every cache key that isn't its own, so two apps
   installed from one origin evict each other on every activation.

The gateway already handles (1) with a **Referer-based compatibility shim**
(`internal/gateway/shim.go`): a request for a root-absolute API path whose
`Referer` points into `/<user>/<app>/` is re-attributed to that instance. It
works, it is tested, and it is the reason you can point muxerr at an
unmodified checkout and have it just run.

It is also a heuristic. `Referer` is a request header the browser controls:
it is stripped under `Referrer-Policy: no-referrer`, absent on some
service-worker-originated fetches, absent on `fetch()` from a worker context in
some browsers, and absent entirely if a privacy extension removes it. When it
is missing the shim has nothing to key on and the request 404s. Patch 01 removes
the need for the guess. That is the whole argument: **the shim recovers the
prefix from a hint; the patch never loses it.**

Patch 03 is not covered by any shim. Cache eviction happens inside the browser,
where the gateway has no reach at all. If you run two of these apps under one
origin without patch 03 you get correct behaviour with pathological offline
performance — each app re-downloads its shell every time you switch.

## The patches

| Patch | Applies to | Fixes | Shim covers it? |
|---|---|---|---|
| [01-sync-base.md](01-sync-base.md) | workoutt, readerr | root-absolute API calls | yes, via `Referer` |
| [02-sw-api-bypass.md](02-sw-api-bypass.md) | workoutt, readerr | SW serving stale `/sync/pull` | no |
| [03-sw-cache-isolation.md](03-sw-cache-isolation.md) | workoutt, readerr | apps evicting each other's caches | no |
| [04-generator.md](04-generator.md) | local-sync-template | future apps born multiplex-ready | n/a |
| [05-hardening.md](05-hardening.md) | any sync backend | bind scope, torn backups, unbounded bodies | partially |

Order matters between 01 and 02: patch 01 moves the API under the app prefix,
which moves it *into* the service worker's scope. Applying 01 without 02 makes
things worse than either — a cached, stale `/sync/pull` is a data bug, not a
performance one. **Apply 01 and 02 together or neither.**

Patch 05 is independent of the rest and independent of muxerr; it is
the pile of "you are now running this as a supervised child process with other
tenants next door" hardening that was never worth doing for a single-user LAN
app.

## How to apply

There is no tooling. Each patch file gives the exact file, the current text,
and the replacement. Edit by hand:

```bash
cd ../readerr          # or ../workoutt
git checkout -b mux-compat
# make the edits from 01 and 02 (and 03, and 05 if you want it)
git diff               # sanity check: 01+02+03 should be under 30 changed lines
```

Then rebuild through muxerr, which is the only build that matters
here — it is what injects the sentinel base:

```bash
cd ../muxerr
go run ./cmd/muxbuild -config apps.json -app readerr
```

`muxbuild` runs `astro build --base=/__MUX__` in the app's `frontend/`, so
`import.meta.env.BASE_URL` inside the built bundle is `/__MUX__/`, and the
gateway rewrites that sentinel to `/alice/readerr/` on the way out. That is why
patch 01's fallback is `import.meta.env.BASE_URL` and not a hardcoded string:
the same source builds correctly for GitHub Pages (`--base=/readerr`), for the
Go backend at the origin root (no `--base`), and for muxerr.

## How to verify

After applying 01 + 02 and rebuilding, with the gateway running and logged in
as `alice`:

1. **Sync is prefixed.** Open `http://localhost:8080/alice/readerr/`, DevTools →
   Network, hit Sync. Every request must be
   `/alice/readerr/sync/push` and `/alice/readerr/sync/pull`. Seeing bare
   `/sync/push` means the build did not pick up the change — check that you
   rebuilt through `muxbuild`, not `npm run build`.
2. **The shim is now dead code for this app.** Watch the gateway log while
   syncing. `internal/gateway/shim.go` logs at `slog.Debug` with
   `msg="shim: re-attributed root-absolute request"`. After the patch that line
   should never appear for the patched app. That is the acceptance test.
3. **The SW is not caching the API.** DevTools → Network, the `/sync/pull`
   request must *not* be marked "(ServiceWorker)". Sync twice with a change made
   on another device in between; the second pull must return the new rows. If it
   returns the same body twice, patch 02 did not take — unregister the old
   worker (Application → Service Workers → Unregister) and hard-reload, because
   the previous worker stays in control until it is replaced.
4. **Caches are isolated (patch 03).** Use both apps, then Application → Cache
   Storage. You must see `readerr-v2` **and** `workoutt-v5` at the same time.
   Before the patch, whichever app activated last is the only key left.
5. **Nothing regressed at the root.** Run the app standalone the old way
   (`DB_PATH=... STATIC_DIR=... go run ./backend`, frontend built with no
   `--base`). `BASE_URL` is `/`, the fallback trims to `''`, and every URL is
   byte-identical to today's. This is the property that makes the patches safe
   to upstream: at the root mount they are a no-op.

## Honest caveats

- These are **not** required. If you only ever run one app per browser profile
  and never lose the `Referer` header, today's shipped code plus the shim is
  fine.
- Patch 01 changes behaviour for anyone who currently relies on the empty
  fallback *while* serving the frontend under a sub-path from a *different*
  origin than the backend. That combination has no configuration in either app
  today (`setSyncUrl` exists precisely for it, and setting it wins over the
  fallback), so this is theoretical — but it is the one behaviour change.
- Patch 03 leaves stale caches from *other* app versions on the origin until
  their own worker next activates. That is the price of not owning the
  namespace; it is bounded by the number of apps, not by time.
- Patch 05 is a grab bag. Take the `BIND_ADDR` and `MaxBytesReader` items and
  leave the rest if you like; they are unrelated to each other.
