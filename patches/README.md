# Upstream patches

Small, optional changes to the *apps* muxxerr serves, kept as prose + diff hunks
rather than `.patch` files because they are one-line-ish edits in files that
change often upstream, and a rejected hunk is a worse experience than three
seconds of manual editing.

## Status at a glance

Most of what this directory once described **has been merged upstream** and is no
longer needed. What remains is one convenience for the generator and one
hardening set for the app backends.

| Was | Applies to | Status now |
|---|---|---|
| 01 — sync base from `BASE_URL` | readerr, workoutt | ✅ **applied upstream** — the vendored submodules already return `import.meta.env.BASE_URL...`; patch file removed |
| 02 — service worker skips the API cache | readerr, workoutt | ✅ **applied upstream** — `UNCACHEABLE` now lists `/sync/`, `/healthz`, `/backup`, `/push/`, `/title`, `/dbsize`; patch file removed |
| 03 — service worker cache isolation | readerr, workoutt | ✅ **applied upstream** — `CACHE_PREFIX` filter in `sw.js` and `Layout.astro` (caches are now `readerr-v3` / `workoutt-v6`); patch file removed |
| [04](04-generator.md) — generator emits its own `apps.json` entry | `local-sync-template` | ◑ **mostly upstream** — 4.1–4.3 merged; only the `emitMuxEntry` convenience (§4.4) is outstanding |
| [05](05-hardening.md) — backend hardening | readerr, workoutt, generator | ⚠️ **not applied** — still needed; **§5.1 is a security fix**, not hygiene |

Because 01–03 are in the shipped apps, the gateway's `Referer` shim
(`internal/gateway/shim.go`) is now a **fallback for unpatched forks**, not the
load-bearing mechanism it once was: a patched app derives its API base from the
build base and sends `/alice/readerr/sync/pull` directly.

## The one that still matters: 05

[05-hardening.md](05-hardening.md) is the set of "you are now a supervised child
process with other tenants next door" changes that were never worth doing for a
single-user LAN app. It is six independent items; take any subset.

**Do not read it as all-optional hygiene.** One item is a live security gap:

- **§5.1 — bind loopback.** The shipped backends do `addr := ":" + envOr("PORT",
  "8080")`, which binds **all interfaces**. Under muxxerr every running instance
  is therefore an unauthenticated, permissive-CORS sync server that anything on
  the LAN can read and overwrite. The gateway does *not* currently set
  `BIND_ADDR` either, so this patch (plus a one-line supervisor change to set
  `BIND_ADDR=127.0.0.1`) is what makes loopback real. Until then a host firewall
  is the only thing enforcing isolation for a from-source deployment. See
  [docs/improvements/security.md §C2](../docs/improvements/security.md#c2).

The other five (`§5.2/5.3` connection pinning, `§5.4` `VACUUM INTO` backups,
`§5.5` body cap, `§5.6` timeouts) are robustness; they map to findings
[P1](../docs/improvements/performance.md#p1),
[H5](../docs/improvements/security.md#h5),
[M3](../docs/improvements/security.md#m3) and
[M4](../docs/improvements/security.md#m4).

## How to apply (05, and 04.4)

There is no tooling. Each file gives the exact file, the current text and the
replacement. Edit the app source in place — the vendored apps are submodules
under `apps/`:

```bash
cd apps/readerr          # or apps/workoutt
git checkout main        # readerr; workoutt's branch is master — commit here loses to a detached HEAD otherwise
# make the edits from 05
git diff                 # sanity check
cd ../..
```

Then rebuild through muxbuild, which is the only build that injects the sentinel
base and is therefore the one that matters here:

```bash
go run ./cmd/muxbuild -config apps.json -only readerr
```

Commit the submodule bump (`git add apps/readerr && git commit`) if you want the
change to travel with this repo — see
[docs/dev/app-sources.md](../docs/dev/app-sources.md).

## How to verify 05

- **§5.1:** `ss -ltnp` (or `netstat -ano` on Windows) on the child's port must
  show `127.0.0.1:<port>`, not `0.0.0.0:<port>`. From another LAN machine,
  `curl http://<host>:<port>/healthz` must fail to connect while the same call
  through the gateway (`/alice/readerr/healthz`, authenticated) succeeds.
- **§5.4:** start a large `/backup`, write to the DB during it, then
  `sqlite3 downloaded.db "PRAGMA integrity_check;"` — must print `ok`.
- **§5.5:** `curl -X POST --data-binary @100mb.json .../sync/push` must return
  400 promptly rather than growing RSS.
- **§5.2/5.3:** run a push loop against workoutt with `NOTIFY_INTERVAL_SECONDS=1`
  and confirm no `SQLITE_BUSY` in the logs.
- **§5.6:** open a raw socket, send nothing, confirm it is closed within the
  header timeout.
