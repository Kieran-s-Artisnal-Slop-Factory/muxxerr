# Operations

Running the multiplexer: first boot, day-to-day administration, backups, and the
security caveats you should know before pointing anyone at it.

The short version, if you read nothing else: **back up `data/` and
`data/pepper.key`, and store the pepper somewhere other than where you store the
database.** Everything else on this page is recoverable.

## First run

```bash
go run ./cmd/muxbuild -config apps.json     # build every configured app
go run ./cmd/mux -config apps.json          # start the gateway
```

On first start the gateway creates, under `data/`:

- `mux.db` — users, sessions, instances, audit log, settings, throttle counters
- `pepper.key` — 32 random bytes, base64, mode 0600 (see [Backups](#backups))
- `instances/` — one directory per (user, app), created on demand

### Bootstrapping the admin

There is no default account and no bootstrap password. **The first account
created through the normal sign-up form becomes the admin.** So the first-run
sequence is: start the gateway, immediately open it, sign up, and you are the
admin.

This is on purpose. Seeding an `admin`/`admin` or an `MUX_ADMIN_PASSWORD`
environment variable is how self-hosted software ends up with a default
credential still live two years later. The tradeoff is a race: between starting
the gateway and creating your account, whoever reaches it first is the admin. On
a LAN that window is seconds and the risk is theoretical — but if the gateway is
reachable from anywhere untrusted, create your account before you tell anyone
the address.

Confirm afterwards: `/admin` should load for you, and the audit log should show
your account creation as the first entry.

### Making someone else an admin

`/admin` → the user → **Make admin**. There is no way to promote a user from the
command line, deliberately: every privilege change goes through the audit log.

## Signups

`signups_enabled` in [apps.json](../../apps.json) is only the **initial
default**, seeded into the settings table on first boot. After that the runtime
value in the database wins, and editing `apps.json` will not change it.

Toggle it at `/admin`. The usual pattern for a small deployment is: leave it on
for the afternoon you are onboarding people, then turn it off and create
accounts by hand.

With signups off, the sign-up form is gone entirely (not shown-and-rejected), so
nobody wastes time on it.

## Users

At `/admin`:

- **Disable** — the account stays, sessions stop working immediately (the
  `is_disabled` check is part of the same query that validates the session, so
  there is no window), and data is untouched. This is the reversible option and
  is almost always what you want.
- **Delete** — removes the user, their sessions and their instance *rows*. It
  does **not** delete their app databases: `data/instances/<user>/` survives.
  That asymmetry is deliberate — dropping a database is a separate, louder
  decision, and it means a deletion done in haste is recoverable. Remove the
  directory by hand once you are sure.
- **Reset an account** — for a user who lost both password and passphrase. This
  sets new credentials and bumps `credential_gen`, which signs them out
  everywhere. Their app data is unaffected; only the login changes.

Anything a user can do to their own account, they should do themselves — see
[../user/getting-started.md](../user/getting-started.md).

## Exporting a user's database

`/admin` → the user → **Export** on an app. The gateway starts the instance if
it is stopped, proxies its `backup_path` (`GET /backup`), and streams back a
SQLite file.

`/backup` is the app's own endpoint, so the file is exactly what that app would
hand its own user — importable back into a standalone install, readable with
`sqlite3`, and containing everything the server knows about that user for that
app.

You can also just take the file:

```
data/instances/alice/readerr/readerr.db
```

**Do not copy that file while the instance is running.** SQLite in WAL mode
keeps recent writes in `readerr.db-wal`; a copy of the main file alone can be
missing the last few minutes or, worse, be internally inconsistent. Either stop
the instance first (`/admin` shows what is running), or copy `.db`, `.db-wal`
and `.db-shm` together, or use the `/backup` endpoint, which is what it is for.

Be aware of a real caveat on the endpoint itself: today's `/backup` in both apps
is `PRAGMA wal_checkpoint(TRUNCATE)` followed by `http.ServeFile`, which
releases every lock before the transfer begins. A write landing mid-download
produces a file that is the right length and internally torn. In practice this
needs concurrent activity to trigger — but "in practice it usually works" is not
what you want from a backup.
[patches/05-hardening.md](../../patches/05-hardening.md) §5.4 replaces it with
`VACUUM INTO`, which snapshots transactionally. If you are relying on `/backup`
for anything you care about, apply that patch.

Verify any backup you intend to trust:

```bash
sqlite3 exported.db "PRAGMA integrity_check;"   # must print: ok
```

## Backups

Two things, and they must be stored **separately**:

### 1. `data/` — everything

```
data/mux.db                              identity
data/instances/<user>/<app>/<app>.db     every user's app data
data/pepper.key                          the key that makes mux.db useful
```

Stop the gateway, copy the directory, restart. If you cannot stop it, snapshot
the filesystem, or back up `mux.db` via `sqlite3 .backup` and each instance
database via `/backup`.

### 2. `data/pepper.key` — again, somewhere else

Every password and passphrase hash in `mux.db` is computed with this 32-byte
key mixed in (HMAC, before Argon2id). It exists so that a stolen `mux.db` is
useless on its own: without the pepper, an attacker cannot verify a single
password guess — not slowly, not expensively, **at all**.

That property is worth exactly as much as your discipline about where you put
it. A backup archive containing both `mux.db` and `pepper.key` is a backup with
no pepper.

**If the pepper is lost, there is no recovery.** Not difficult — impossible.
Every stored hash becomes permanently unverifiable, and the only remedy is
resetting every account by hand. If your own admin password is among them, that
means editing the database directly. This is the single most consequential
operational fact about the system.

Reasonable arrangements:

- pepper in your password manager (it is base64 text, ~44 characters), database
  backed up normally
- `MUX_PEPPER` supplied by systemd credentials or a secrets manager, so
  `pepper.key` never exists on disk at all — this is the case the env var was
  added for, and it is checked before the file
- printed on paper in a different building from the disk, if you are that sort
  of person

Test a restore before you need one: restore `data/` plus the pepper to a scratch
directory, start the gateway against it, and log in. A backup you have never
restored is a hypothesis.

### What you do not need to back up

`runtime/` is entirely reproducible — `muxbuild` regenerates it from the app
sources. Do not include it; it is the large directory.

## Routine maintenance

- **Rebuild after updating an app**: `go run ./cmd/muxbuild -config apps.json
  -only readerr`, then restart the gateway so running children are replaced with
  the new binary. Children are not hot-swapped.
- **Watch the logs.** They are structured (`slog`), so grep by field. The ones
  worth alerting on: repeated instance start failures, guard blocks
  (`guard: blocked private target`) which mean someone is probing `/title`, and
  throttle lockouts on a single username.
- **Disk.** Each instance is a SQLite file that only grows. `VACUUM` per
  instance if one gets large; `/backup` under the `VACUUM INTO` patch compacts
  as a side effect.
- **Memory.** One process per *active* (user, app) pair at ~12 MB each. Idle
  timeouts are the only capacity control, and `always_on` apps opt out of them —
  every user with workoutt has a permanently resident process, by design, because
  its notification scheduler only runs while the process does.

## Reading an instance's logs

Every child process's output is kept in a small in-memory ring — a couple of
hundred lines per instance — and both the owner and an admin can read the tail
of it. Users get a **Logs** link on each app card; admins get one beside every
running instance on the admin page.

The lines are the app's own `slog` output, unpacked from JSON so the message
reads first and the structured fields sit underneath. Anything that is not JSON
— a panic, most importantly — is shown verbatim.

Two things to know:

- **It is in memory only.** A gateway restart clears every buffer. This is
  deliberate: persisting them would mean rotation, retention and disk budgets,
  and a new place for an app to leak something, in exchange for answering a
  question that is nearly always about the last minute. The gateway's own
  stdout still has everything, tagged with user and app, if you ship it
  somewhere.
- **An admin reading someone else's logs is audited**, the same as exporting
  their database, and the page says so on the way in.

## Serving the apps' PWA assets

`manifest.webmanifest`, `favicon.ico`, `icon.svg` and their siblings are served
**without a session**, straight from the app's build directory, without starting
a child process.

That is not an oversight. A browser fetches `<link rel="manifest">` with
credentials omitted — no cookies — so requiring a session there means the fetch
401s and the app can never be installed as a PWA. The alternative fix is adding
`crossorigin="use-credentials"` to every app's layout, which is an upstream
change to every app including ones that do not exist yet, for a file that
contains nothing but the app's name and colours and is byte-identical for every
user.

The list is deliberately short and exact: only a bare filename at the root of
the app's build, only names on the list, no nesting, no traversal. The app shell
and the sync API stay behind auth. The gateway also answers identically for a
username that does not exist, so this cannot be used to enumerate accounts.

## Security caveats

These are the things you should say out loud before giving anyone the address.

### Shared browsers are the weak point

This is the most likely way data actually leaks here, and it is not something
the gateway can fix.

The apps are local-first: the authoritative copy of a user's data lives in
**IndexedDB in the browser**, not on the server. IndexedDB is scoped per origin,
not per logged-in user. So on one browser profile:

- signing out of the gateway does **not** clear the app's stored data;
- the next person on that profile can open the app's storage directly through
  devtools, without any session;
- two users of the same app on one profile can see each other's data.

The gateway sets `Cache-Control: no-store` on API responses and its session
cookie is `HttpOnly`, which stops the *transport* leaking. Neither touches
IndexedDB.

Tell users: on a shared computer, use a private window or a separate browser
profile. It is in
[../user/getting-started.md](../user/getting-started.md), and it is worth
repeating.

Related, smaller: the service-worker cache is also per origin, so co-hosted apps
share it. That cache holds only the static shell and fingerprinted assets —
identical for every user, no data — so it is a performance problem rather than a
privacy one. See
[patches/03-sw-cache-isolation.md](../../patches/03-sw-cache-isolation.md).

### The `/title` SSRF guard

readerr's `GET /title?url=` fetches a caller-supplied URL server-side, because a
browser cannot do it cross-origin. Its own source is candid about the posture:
*"No SSRF hardening beyond the scheme check — consistent with the single-user
LAN posture … of the rest of the server."*

That was a fair call for a single-user app. It stops being fair when anyone with
an account can reach the endpoint — a fetch originating from the server can
reach your router's admin page, a cloud metadata endpoint, or anything else on
the LAN that trusts local traffic.

So the gateway guards it, declared in `apps.json`:

```json
"guarded_routes": [
  { "path": "/title", "param": "url", "policy": "block-private" }
]
```

`block-private` rejects targets resolving to loopback, RFC1918, link-local and
unique-local addresses before the request ever reaches the child.

Two things to know. First, this is **per configured route** — if you add another
app with a URL-fetching endpoint and do not declare it, it is unguarded. Second,
it is not a complete SSRF defence: DNS rebinding (a name that resolves to a
public address at check time and a private one at fetch time) and open
redirectors to internal hosts are outside what a pre-flight check on the
supplied URL can catch. It raises the bar substantially; it does not make the
endpoint safe to expose to the public internet.

### Children bind loopback only — with a caveat

Each child listens on `127.0.0.1:<ephemeral>` and the gateway dials it directly.
Nothing outside the host can reach a child, which matters a great deal because
**the children have no authentication at all** — they are the original
single-user, no-auth, permissive-CORS servers. Anyone who could reach one
directly would have full read/write on that user's database with no session and
no username.

The caveat: today's app binaries do `addr := ":" + envOr("PORT", "8080")`, which
binds *all* interfaces. The supervisor sets `BIND_ADDR=127.0.0.1` in the child's
environment, but the shipped apps do not read it —
[patches/05-hardening.md](../../patches/05-hardening.md) §5.1 is the one-line
change that makes them. Until that patch is applied, **a host firewall is the
thing actually enforcing this**, so make sure the ephemeral port range is not
open on the machine's external interfaces.

### Not a boundary against the apps themselves

Every child runs as the same OS user, with the same filesystem access, as the
gateway. The isolation is one process and one database per tenant, which is
plenty against accidental cross-tenant reads and worth nothing against an app
that is actively hostile. Only mount code you would run yourself.

### Transport

Ships as plain HTTP with `secure_cookies: false`, because the expected
deployment is a LAN or a Tailscale network and a `Secure` cookie over HTTP
simply fails to work, in a way that looks like a bug.

If you put it behind TLS — and you should, if it is reachable from anywhere you
do not control — set `"secure_cookies": true`. Terminate TLS in a reverse proxy;
the gateway does not do it.

### Session lifetime

`session_ttl` ships at `720h` (30 days). Long, because this is a personal-scale
tool and weekly re-authentication is friction without a matching benefit — and
because a password change bumps `credential_gen`, which invalidates every
existing session immediately. Shorten it if the deployment warrants; the cost is
purely user annoyance.

### Admin impersonation

`allow_admin_impersonation` defaults to **false**, and leaving it there is the
right call. An admin can already export any user's database, so impersonation
grants no new access — what it grants is the ability to browse as someone else,
which is a different and more uncomfortable power. If you enable it, tell your
users, and check the audit log.

## When something is wrong

| Symptom | Where to look |
|---|---|
| Gateway will not start, error names a config field | `apps.json` — parsed with `DisallowUnknownFields`; the error names the key |
| "instance failed to become ready" | the child's stderr in the gateway log; then `health_path` in `apps.json` |
| One user's app 404s on sync | `api_prefixes` for that app, and the `Referer` shim — see [adding-an-app.md](../dev/adding-an-app.md) |
| Everyone locked out after a restore | you restored `mux.db` without the matching `pepper.key`. Restore the right pepper; there is no other fix |
| workoutt reminders stopped | `always_on: true` **and** `idle_timeout: "0s"` must both be set |
| Memory climbing | count running instances; check whether an app that should idle-stop is marked `always_on` |
