# Security gaps

Ranked most-serious first. Each finding cites the code, states the concrete
attacker capability, and notes any existing mitigation. See the
[posture note](README.md#read-this-first-posture-decides-almost-everything) —
most of these require you to have left the intended LAN/Tailscale posture, and
the two that do not are flagged.

---

## C1 — The SQL console can `ATTACH` any database on disk {#c1}

**Severity: Critical. Reachable inside the intended posture by any account holder.**

`internal/web/sqlconsole.go:259` (`executeLiveSQL`) opens the caller's own
instance database read-write and runs arbitrary SQL with no statement
restrictions:

```go
dsn := "file:" + dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(ON)"
db, err := sql.Open("sqlite", dsn)
```

`resolveConsoleTarget` (`sqlconsole.go:226`) correctly stops a non-admin from
*naming* another user's app. But SQLite's `ATTACH DATABASE` is not blocked, and
the connection runs as the gateway's OS user, so the moment the console is open
the caller can reach **any file the process can read or write** — including
`mux.db` (users, `is_admin`, session generations) and every other tenant's
instance database, all at documented paths under `data/`.

The `data/` layout is public: [operations.md](../admin/operations.md#backups)
lists `data/mux.db` and `data/instances/<user>/<app>/<app>.db`, and the Docker
image fixes them at `/srv/data/...`.

**Exploit (any user, no admin):** sign up → add any sync app (to get an instance
DB) → **Tools → SQL console** → type the unlock sentence → run

```sql
ATTACH DATABASE '/srv/data/mux.db' AS m;
UPDATE m.users SET is_admin = 1 WHERE username = 'attacker';
```

They are now an admin. `ATTACH '/srv/data/instances/<victim>/readerr/readerr.db'`
then reads or rewrites anyone's data. This defeats the entire per-tenant
isolation story, from a plain account, using two **shipped defaults**:
`"sql_console": true` and `"signups_enabled": true` in the shipped
[apps.json](../../apps.json).

**Why the docs miss it:** [database-tools.md](../dev/database-tools.md) frames
"no statement filtering" as a virtue ("a tool whose stated purpose is arbitrary
SQL should not pretend to have guardrails") — true *for the caller's own
database*, but `ATTACH` crosses the isolation boundary the whole system is built
on, and nothing there says so.

**Fix (in priority order):**

1. **Ship `sql_console: false`.** It is a repair tool; it should not be on by
   default on a server that also has open sign-ups. This is the one-line change
   that closes the exposure immediately.
2. **Deny `ATTACH` on the console connection.** Register a SQLite authorizer
   that rejects `SQLITE_ATTACH` (or set the attached-database limit to 0) so the
   connection is confined to the one file it opened. A text blocklist is not
   enough — `database-tools.md` is right that those are decorative.
3. **Make the console admin-only.** `HandleSQLConsole*` gate on `requireUser`
   (any user); gating on `IsAdmin` matches the tool's actual audience and the
   `resolveConsoleTarget` model, which already treats cross-user access as
   admin-only.

Do (1) now; (2) is the real fix; (3) is defence in depth. All three are small.

---

## C2 — App backends bind all interfaces with no authentication {#c2}

**Severity: Critical for source / host-network / shared-bridge deployments;
Medium (latent) for the default single-container Docker deployment.**

Both vendored backends listen on every interface:

```go
addr := ":" + envOr("PORT", "8080")   // apps/readerr/backend/main.go:113, apps/workoutt/backend/main.go:99
http.ListenAndServe(addr, withCORS(mux))
```

`":" + port` binds `0.0.0.0`. These are the original single-user, **no-auth**,
permissive-CORS sync servers — anyone who can reach one has full read/write/wipe
of that tenant's database with no session and no username. The supervisor picks
a *loopback* free port and only ever dials `127.0.0.1` itself
(`supervisor.go:963,805`), but it does **not** constrain the child's bind:
`childVars` (`supervisor.go:904`) sets `DB_PATH`, `PORT`, `STATIC_DIR` and
`VAPID_*` and nothing else — there is no `BIND_ADDR` anywhere in the gateway.

This directly contradicts two docs, which this change set corrects:

- the [README](../../README.md) said "the child processes still bind loopback
  only" — false;
- [operations.md](../admin/operations.md) said "the supervisor sets
  `BIND_ADDR=127.0.0.1` in the child's environment" — it does not.

**Reachability depends on how you run it:**

- **From source / `go run ./cmd/mux` on a host:** children bind
  `0.0.0.0:<ephemeral>` on the machine → **reachable from the whole LAN.** Any
  device on the network reads and overwrites any user's database. Critical.
- **Default `docker compose up`:** children bind `0.0.0.0:<ephemeral>` *inside
  the container's network namespace*; only `8080` is published, so the ephemeral
  ports are not reachable from the host or LAN. Contained — **but** any sibling
  container on the same Docker network can reach them, and the Caddy snippet's
  `network_mode: "service:mux"` and any `network_mode: host` remove the
  containment. Latent, one compose edit away from Critical.
- Additionally, a browser on the user's own machine can be used for **DNS
  rebinding** onto the host's LAN IP to reach a source-run child.

**Fix:** apply [patch 05 §5.1](../../patches/05-hardening.md) so the child
defaults to `BIND_ADDR=127.0.0.1`, **and** have the supervisor actually set
`BIND_ADDR=127.0.0.1` in `childVars` (today it sets nothing, so even a patched
app has to be told). Until then, a host firewall on the ephemeral range is the
only thing enforcing isolation for a source deployment.

---

## H1 — The container image runs as root {#h1}

**Severity: High. Applies to the default deployment.**

[Dockerfile:158](../../Dockerfile) has no `USER` directive, so the gateway and
every child app process run as **root** inside the container. This magnifies
everything else: the C1 `ATTACH` escape reads and writes any file as root and
leaves root-owned files on the bind-mounted `./data`; a compromise of any child
(the apps are explicitly "not a boundary against the apps themselves") is a root
compromise of the container.

**Fix:** add a non-root `USER` to the final stage and `chown` `/srv/data`
(and the runtime dir) to it. The backends are static `CGO_ENABLED=0` binaries and
need no root capability; the only thing to check is that the bind-mounted volume
is writable by the chosen UID.

---

## H2 — Unauthenticated Argon2id resource exhaustion {#h2}

**Severity: High when reachable by the public.**

Password hashing is deliberately expensive — 64 MiB, `t=3`, `p=4`
([auth.md](../dev/auth.md), `internal/auth/password.go:24`). Two paths let an
unauthenticated caller spend that budget without limit:

- **Sign-up has no throttle.** `HandleSignup` (`internal/web/auth_handlers.go`)
  hashes a password (and generates + hashes a passphrase) on every POST, with no
  per-IP rate limit — unlike login, which is throttled. Whenever sign-ups are
  open (they must be at least once, to create the first admin, and are commonly
  left on), each POST pins ~64 MiB × 4 threads.
- **Login amplifies via `FakeVerify`.** By design a login for a non-existent
  user still runs a full Argon2id verification (`auth.FakeVerify`) to avoid a
  timing oracle — correct, but it means unauthenticated login attempts also cost
  a full hash each, and there is no *global* concurrency cap, only the per-key
  throttle that kicks in after a few attempts.

A few dozen concurrent requests can saturate CPU and RAM on a small box and make
the gateway unresponsive.

**Fix:** throttle sign-up on the same per-IP keys login uses, and add a global
semaphore bounding concurrent Argon2id operations (e.g. to `GOMAXPROCS`), so
verification queues instead of thrashing.

---

## H3 — workoutt stores the VAPID private key and push secrets in its DB {#h3}

**Severity: High, conditional on C1 or C2.**

With the shipped config the VAPID env keys are empty
([apps.json](../../apps.json): `VAPID_PRIVATE_KEY: ""`), so workoutt **generates
a keypair and persists it in its own SQLite database**
(`apps/workoutt/backend/push.go:73`, `INSERT INTO push_config`). The same
database holds every device's `push_subscriptions` (endpoint, `p256dh`, `auth`).

Anyone who can read that database — via the C1 `ATTACH` escape, via a
LAN-reachable backend (C2), or from a stolen/mislaid backup — gets the private
key **and** the subscriptions, and can then send arbitrary Web Push messages to
the user's devices (phishing links in a notification the OS renders as trusted).

**Fix:** this is mostly closed by fixing C1 and C2. To reduce blast radius
independently, supply stable `VAPID_PRIVATE_KEY`/`VAPID_PUBLIC_KEY` via the
environment (kept out of the tenant DB), which `ensurePush` already prefers when
set — though note the gateway currently shares one VAPID pair across all tenants
by design (`supervisor.go` copies `VAPID_*` from the host), so an env key is
server-wide, not per-user.

---

## H4 — No resource limits on children, no ceiling on instances {#h4}

**Severity: High on any multi-user deployment.**

Children are ordinary `exec.Command` processes (`supervisor.go:449`) with **no
cgroup, no `rlimit`, no memory/CPU/fd cap, and no process-group containment**. A
single misbehaving child (a bad migration on a huge import, a leak, an fd storm)
can consume unbounded host RAM/CPU/descriptors and starve every other tenant and
the gateway. WAL recovery protects the *data*, not the *host*.

Separately, `Ensure` (`supervisor.go:265`) has **no ceiling on the number of
concurrent instances**. `StartAlwaysOn` bounds *boot* parallelism to 4, but
on-demand starts do not: with open sign-ups, a script can register N accounts,
provision the apps, and touch each once, forcing N × (#sync apps) resident
processes. Memory is bounded only by idle timeouts, and `always_on` apps opt out
of even those (see [P2](performance.md#p2)).

**Fix:** run children in their own process group with `rlimit`s (`RLIMIT_AS`,
`RLIMIT_NOFILE`) or a cgroup; cap total live instances and shed with a 503 +
`Retry-After` past the cap (the crash-loop path already models this).

---

## H5 — The app's own `/backup` produces torn/corrupt backups {#h5}

**Severity: High for anyone relying on `/backup`.**

`handleBackup` in both apps
(`apps/readerr/backend/sync.go:392`, `apps/workoutt/backend/sync.go:329`) does
`PRAGMA wal_checkpoint(TRUNCATE)` then `http.ServeFile` on the **live**
database. Every lock is released before the transfer starts, so a write landing
mid-download yields a file that is the right length and internally inconsistent —
the worst kind of backup, because it looks fine until you restore it.
`ServeFile` also advertises byte ranges, so a resumed download tears on purpose.

**Mitigation already present:** the gateway's *own* export path — "Download
data" and admin **Export**, i.e. `/apps/<app>/export` — does **not** use the
app's `/backup`. It runs `VACUUM INTO` directly on the file
(`internal/gateway/export.go:83`), read-only, transactionally consistent, whether
or not the instance is running. So the paths most users take are safe. The torn
path is the app's own `/backup`, reachable through the proxy at
`/<user>/<app>/backup` and used by the app's in-frontend backup button.

**Fix:** apply [patch 05 §5.4](../../patches/05-hardening.md) so the app's
`/backup` also uses `VACUUM INTO`. Verify any backup with
`sqlite3 file.db "PRAGMA integrity_check;"`.

---

## M1 — Every child inherits `MUX_PEPPER` and the whole gateway environment {#m1}

**Severity: Medium.**

`spawn` builds the child environment from `os.Environ()`
(`supervisor.go:448`) and `buildEnv` only removes keys that `childVars`
explicitly replaces. `MUX_PEPPER` is neither reserved nor replaced, so when the
pepper is supplied **via the environment** (the `MUX_PEPPER` mode the docs
recommend for keeping it out of the data volume), **every app backend inherits
the server-wide password pepper** in its own environment, readable from inside
the child via `/proc/self/environ`.

The pepper is the one secret that keeps a stolen `mux.db` uncrackable
([auth.md](../dev/auth.md)). The threat model says muxxerr is "not a boundary
against the apps themselves", so a *hostile* app already wins (in the default
file mode it can read `data/pepper.key` off the shared filesystem too). But
handing the pepper to every child in its environment is gratuitous: it means a
read-only information disclosure in any app — a log of its own env, a debug
endpoint that echoes it — leaks the one secret the whole hashing scheme depends
on, and it undercuts the entire point of `MUX_PEPPER` mode, which is to keep the
pepper *out* of easy reach.

**Fix:** in `childVars`, start the child environment from a minimal allowlist,
or explicitly strip `MUX_PEPPER` (and any other gateway secret) before spawning.
Children need `DB_PATH`, `PORT`, `STATIC_DIR`, `TZ`, `VAPID_*` and their own
`env` block — not the gateway's secrets.

---

## M2 — The `/title` SSRF guard is bypassable and path-exact {#m2}

**Severity: Medium. *Already documented.***

`guard.go` blocks `/title?url=` targets that resolve to loopback/RFC1918/
link-local/unique-local at check time (`block-private`). Two gaps, both of which
[operations.md](../admin/operations.md#the-title-ssrf-guard) already discloses:

- **Redirects and DNS rebinding** defeat a pre-flight check — a name that
  resolves public at check time and private at fetch time, or a public URL that
  302s to `http://169.254.169.254/`, reaches internal targets. The guard checks
  the *supplied* URL, not the *fetched* one.
- **Path matching is exact** (`guard.go:45`): the guard keys on the configured
  path string, so `/title/` vs `/title`, or a future app whose `guarded_routes`
  entry does not exactly match its real route, gets **no** protection and fails
  open.

Combined with [C2](#c2), an unauthenticated LAN caller reaches this directly.

**Fix:** re-validate the target after DNS resolution and on every redirect hop
(deny redirects to private space), and match guarded routes by prefix with an
explicit boundary rather than exact string. Or accept the residual risk — the
guard "raises the bar substantially; it does not make the endpoint safe to expose
to the public internet", which is stated honestly today.

---

## M3 — App backends decode request bodies with no size cap {#m3}

**Severity: Medium.**

`handlePush` decodes with an unbounded `json.Decoder`
(`apps/readerr/backend/sync.go:157`, `apps/workoutt/backend/sync.go:173`). A
streamed multi-hundred-MB body inflates to gigabytes of heap in a process the
supervisor expects to run at ~12 MB, OOM-killing it.

**Mitigation:** the gateway caps proxied bodies at 256 MiB
(`maxAPIBody`, `internal/gateway/gateway.go:44,276`), so requests *through the
proxy* are bounded. This finding is defence-in-depth for the standalone case and
for [C2](#c2) direct access, where the gateway cap does not apply.

**Fix:** [patch 05 §5.5](../../patches/05-hardening.md) —
`http.MaxBytesReader(w, r.Body, 64<<20)` on `/sync/push`.

---

## M4 — App backends run with no HTTP timeouts {#m4}

**Severity: Medium.**

`http.ListenAndServe` uses a zero-valued server: no read, write, idle, or header
timeout (`apps/*/backend/main.go`). A connection that opens and says nothing is
held forever; a pool of slow clients exhausts goroutines and fds without ever
crashing the child, so the crash-loop damper never fires.

**Mitigation:** the gateway itself sets `ReadHeaderTimeout` and `IdleTimeout`
(`cmd/mux/main.go:133`), and deliberately no `WriteTimeout` (a first `/sync/pull`
streams a whole DB). So slowloris against the *gateway* is bounded; against a
directly-reachable child ([C2](#c2)) it is not.

**Fix:** [patch 05 §5.6](../../patches/05-hardening.md) — an `http.Server` with
explicit read/idle/header timeouts.

---

## M5 — Client IP is the leftmost `X-Forwarded-For`, which is spoofable {#m5}

**Severity: Medium. *Partly documented.***

`clientIP` (`internal/web/clientip.go`) honours `X-Forwarded-For` only from a
peer named in `site.trusted_proxies` (loopback always trusted), which is correct.
But from a *trusted* peer it takes the header at face value, and the login/reset
throttle keys on `login:ip:<clientIP>` (`auth_handlers.go`). If you trust a
range wider than the proxy actually occupies — or trust something you do not run
— whoever is behind it can forge a new IP per request and the per-IP throttle
stops meaning anything.

[deployment.md §2](../admin/deployment.md#2-client-ip-and-why-the-proxy-topology-matters)
covers the trust configuration well; what it does not spell out is that the value
used is the **leftmost** XFF entry, which an untrusted upstream can pre-populate.

**Fix:** parse XFF right-to-left, trusting one hop at a time, and stop at the
first untrusted address. Keep the deployment doc's guidance to name only proxies
you run.

---

## M6 — The login/reset throttle fails open on a database error {#m6}

**Severity: Medium.**

The throttle check (`auth_handlers.go`) treats a store error as "not throttled"
and proceeds to verify. Under DB contention — which an attacker can *induce*,
e.g. by driving the SQL console or a heavy sync — the per-username and per-IP
lockouts silently disable themselves, permitting unlimited online guessing during
the window.

**Fix:** fail closed — on a throttle-store error, reject with a generic "try
again shortly" rather than falling through to verification.

---

See [performance.md](performance.md) for the robustness and scaling findings
(P1–P8), several of which share root causes with the above.
