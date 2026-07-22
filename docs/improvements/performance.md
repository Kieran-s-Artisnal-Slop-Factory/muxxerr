# Performance and robustness gaps

Ranked by how likely you are to hit them. These are about behaviour under load
and at scale rather than attacker capability; several share a root cause with the
[security findings](security.md).

---

## P1 — App SQLite pools aren't pinned to one connection {#p1}

**Severity: Medium. You will hit this with two devices syncing at once.**

Both backends open their database with WAL + a 5s busy timeout but leave
`database/sql`'s connection pool unbounded and use the driver's default
`DEFERRED` transactions (`apps/readerr/backend/db.go:148`,
`apps/workoutt/backend/db.go:25`):

```go
dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
```

Two deferred transactions that both take a read lock and then try to upgrade to a
write lock deadlock, and one gets `SQLITE_BUSY` **immediately** — the busy
timeout cannot rescue it, because neither side can make progress. In practice a
client syncing from two devices, or a `/sync/push` overlapping workoutt's
60-second notification-scheduler write, produces intermittent `database is
locked` 500s and dropped sync batches.

The gateway's *own* store already does the right thing — one connection —
"removes `SQLITE_BUSY` as a category" ([architecture.md](../dev/architecture.md)).
The child databases do not.

**Fix:** [patch 05 §5.2–5.3](../../patches/05-hardening.md) —
`db.SetMaxOpenConns(1)` and `&_txlock=immediate`. Either alone is enough; a
single connection cannot deadlock against itself.

---

## P2 — `always_on` apps are one resident process per user, forever {#p2}

**Severity: Medium (by design). The scaling ceiling.**

Memory scales with the number of *provisioned* `always_on` (user, app) pairs, not
active ones. workoutt is `always_on` because its notification scheduler only
fires while the process lives ([architecture.md](../dev/architecture.md)), so
**every user who has added workoutt has a permanently resident ~12 MB process**,
started at boot by `StartAlwaysOn`. Idle timeouts — the only capacity control —
do not apply to it.

This is a correct trade for a household or a small team and wrong for hundreds of
users, which the [README](../../README.md) states plainly ("obviously wrong for a
thousand users"). It is listed here so the number is explicit: budget
RAM as `~12 MB × (users with an always_on app) + ~12 MB × (peak concurrent
on-demand instances)`.

**Options if it bites:** a shared, gateway-level scheduler that wakes an instance
only to deliver (removing the reason workoutt must stay resident); or a hard cap
on always_on instances with the rest lazily started and accept missed reminders
while stopped.

---

## P3 — Every HTML/asset response is buffered, rewritten and re-gzipped {#p3}

**Severity: Low. Wasteful, not dangerous.**

The sentinel rewrite (`internal/gateway/rewrite.go`) buffers each rewritable
response in full, runs a string replacement (`/__MUX__` → `/alice/readerr`), and
re-gzips, on **every request** — there is no cache of the rewritten-per-user
output, and HTML additionally gets a full-body lowercase copy for the account
guard injection. The work is redundant: for a given (user, app) the rewritten
bytes are identical every time until the build changes.

Fine for a few users; pure CPU/allocation waste as asset traffic grows, and it
defeats the `immutable` caching story that the gateway's *own* content-addressed
assets enjoy.

**Fix:** cache rewritten output keyed by (user, asset-hash) — the build is
immutable and the prefix is per-user, so the cache key is exact and never stale.
Even a small LRU in front of the hot HTML shells would remove most of it.

---

## P4 — Per-instance log ring can pin megabytes in the gateway heap {#p4}

**Severity: Low.**

Each instance keeps a 200-line ring (`LogCapacity`, `internal/supervisor/logbuf.go:29`)
and each line is capped at `maxLogLine = 64 KiB` (`supervisor.go:99`). Worst case
that is ~12 MB *per instance* held in the gateway process, and with many
instances it multiplies. A child that emits large lines rapidly (a stack-trace
loop, verbose error dumps) sits at the ceiling.

**Fix:** cap the ring by total bytes as well as line count, or shrink
`maxLogLine`. The UI only shows 50 lines; the 200-line buffer is generous
already.

---

## P5 — Orphan reaper can signal a recycled PID {#p5}

**Severity: Low.**

The orphan reaper (`internal/supervisor/orphan_other.go:22`) records a PID +
`StartedAt` + binary name in a pid-file so a killed gateway can stop leftover
children on the next boot, but the non-Windows path does not verify `StartedAt`
before signalling. After an ungraceful exit and a reboot, a recycled PID that now
runs an image with the same basename (another `readerr`/`workoutt`, or on
macOS/BSD any process) could be signalled.

**Fix:** compare the recorded `StartedAt` against the live process's start time
(the field is already stored) before sending a signal; skip if they differ.

---

## P6 — The `crashes` map is never garbage-collected {#p6}

**Severity: Low.**

`noteCrash` (`supervisor.go:624`) creates a `crashTracker` per (user, app) and
never deletes it. Individual death timestamps are pruned, but the map entry
itself lingers, so the map grows by one for every distinct pair that has ever
crashed. Negligible on any real deployment; a housekeeping leak on a long-lived
server with much churn.

**Fix:** drop a tracker in the janitor once its `deaths` slice has been empty for
a while.

---

## P7 — The console gate stat-storms every instance DB on each render {#p7}

**Severity: Low.**

Rendering the SQL-console gate lists apps and their sizes by stat-ing instance DB
files (`internal/web/tools.go:136`), including for other users when the caller is
an admin. On a server with many provisioned pairs, each page load — and each
failed-phrase retry — issues a burst of filesystem syscalls proportional to the
total instance count.

**Fix:** cache sizes briefly, or compute them lazily only for the selected app.

---

## P8 — Published images ship without build provenance {#p8}

**Severity: Low (supply chain).**

`.github/workflows/docker.yaml:96` sets `provenance: false` for a smaller
manifest. A self-hoster who `docker compose up -d` against the moving `:latest`
tag has no cryptographic attestation that the image was built from this source by
this pipeline.

**Fix:** enable `provenance: true` (SLSA attestation) for tagged releases, and
recommend pinning `MUX_IMAGE` to a released digest rather than `:latest` (the
compose file already suggests pinning a version).

---

## A note on what is *already* right

Worth stating, so this file is not read as a list of everything being broken:

- The gateway store uses a single SQLite connection and content-addressed,
  `immutable` static assets — the child DBs are the ones that need [P1](#p1).
- Cold start is ~55 ms warm / ~111 ms cold at ~12 MB RSS, which is what makes
  lazy start-and-idle-stop viable in the first place.
- The supervisor collapses concurrent cold starts into one process (single-flight
  on the instance `ready` channel), damps crash loops, and drains child output
  without losing the last lines of a dying process.
- Concurrent-start port races are retried on a fresh port; the export path is
  transactionally consistent (`VACUUM INTO`).

The gaps above are real but bounded, and most are one small change each — the
patch-05 items ship as a ready diff in
[patches/05-hardening.md](../../patches/05-hardening.md).
