# Known gaps — security and performance

This directory is an honest, evidence-backed list of the places muxxerr is
weaker than its own documentation implies, ranked by how much they should worry
you. It exists because "what this is not" in the [README](../../README.md) is
deliberately high-level, and an operator deciding whether to expose this thing
deserves the specifics with file-and-line citations.

Two documents:

- [security.md](security.md) — things an attacker can *do*.
- [performance.md](performance.md) — things that fall over under load or scale.

Every finding names the file and line it is in, says what it costs in concrete
terms, and — importantly — says whether something already mitigates it. A
finding marked *already documented* means the shipped docs disclose it honestly;
it is listed here anyway so the whole picture is in one place.

## Read this first: posture decides almost everything

muxxerr was written for one posture — **a LAN or a Tailscale/WireGuard network**,
plain HTTP, one small trusted group. Most of what follows is only reachable when
you leave that posture (put it on the public internet, run it on a shared
network, enable open sign-ups for strangers). The project says as much, and that
is a legitimate design choice, not a bug.

Two findings are the exception, because they bite **inside the intended posture,
triggered by an ordinary account holder rather than an outside attacker**:

- **[C1 — SQL-console `ATTACH` escape](security.md#c1).** With the shipped
  defaults (`sql_console: true` *and* `signups_enabled: true` in
  [apps.json](../../apps.json)), any user who can sign up can escalate to admin
  and read every other tenant's database. This is the one to fix before you do
  anything else.
- **[H1 — the container runs as root](security.md#h1).** It turns every other
  weakness into a root-level one.

## Priority table

| # | Finding | Severity | Only if exposed? | Fix lives in |
|---|---|---|---|---|
| [C1](security.md#c1) | SQL console `ATTACH` reaches `mux.db` and other tenants' DBs → admin escalation | **Critical** | No — any signup | ship `sql_console:false`; deny `ATTACH`; make it admin-only |
| [C2](security.md#c2) | App backends bind `0.0.0.0`, no auth — every instance is an open sync server | **Critical** \| Medium in default Docker | Source / host-net / shared bridge | [patch 05 §5.1](../../patches/05-hardening.md) + supervisor sets `BIND_ADDR` |
| [H1](security.md#h1) | Container image runs as **root** | High | No | `USER` in Dockerfile |
| [H2](security.md#h2) | Sign-up / login Argon2id has no cap → unauthenticated CPU/RAM DoS | High | Public | throttle sign-up; global Argon2 semaphore |
| [H3](security.md#h3) | workoutt DB stores the VAPID private key + push secrets → push spoofing | High | Via C1/C2 | keep keys out of the tenant DB, or accept under C2 fix |
| [H4](security.md#h4) | No per-child rlimits and no instance ceiling → one tenant starves the host | High | Multi-user | cgroups/rlimits; a concurrent-instance cap |
| [H5](security.md#h5) | App's own `/backup` streams a live file → torn/corrupt backups | High | Anyone using `/backup` | [patch 05 §5.4](../../patches/05-hardening.md) (`VACUUM INTO`) |
| [M1](security.md#m1) | `MUX_PEPPER` and the whole gateway env are inherited by every child | Medium | Malicious/leaky app | strip secrets in `childVars` |
| [M2](security.md#m2) | `/title` SSRF guard is bypassable (redirect, DNS rebinding) and path-exact | Medium | Public + an account | *already documented*; harden or accept |
| [M3](security.md#m3) | App backends decode request bodies with no size cap | Medium | Direct/LAN | [patch 05 §5.5](../../patches/05-hardening.md) |
| [M4](security.md#m4) | App backends run with no HTTP timeouts (slowloris) | Medium | Direct/LAN | [patch 05 §5.6](../../patches/05-hardening.md) |
| [M5](security.md#m5) | Client IP taken from leftmost `X-Forwarded-For` (throttle-key evasion) | Medium | Public | *partly documented*; trust the right hop |
| [M6](security.md#m6) | Login/reset throttle fails **open** on a database error | Medium | Public | fail closed |
| [P1](performance.md#p1) | App SQLite pool not pinned to 1 / deferred txlock → `SQLITE_BUSY` | Medium | Concurrent writes | [patch 05 §5.2–5.3](../../patches/05-hardening.md) |
| [P2](performance.md#p2) | `always_on` apps are one resident process **per user, forever** | Medium | Scale | by design; the scaling ceiling |
| [P3](performance.md#p3) | Every HTML/asset response is buffered, rewritten and re-gzipped per request | Low | Traffic | cache rewritten output |
| [P4](performance.md#p4)–[P8](performance.md#p8) | Log-ring heap, orphan PID-reuse, unbounded `crashes` map, stat-storms, no image provenance | Low | — | see [performance.md](performance.md) |

## How this was produced

A fan-out audit read each subsystem (`internal/auth`, `internal/gateway`,
`internal/supervisor`, `internal/web`, the vendored app backends, and the
Docker/CI surface) and every claim was then re-checked against the source by
hand. The two findings above the fold (C1, H1) and the patch-05 class (C2, H5,
M3, M4, P1) were verified directly against the code and the vendored submodules.

Where a finding corresponds to one of the still-unapplied hardening items, it
links to [patches/05-hardening.md](../../patches/05-hardening.md), which carries
the concrete diff. **Patches 01–03 are already applied upstream** (so the
service-worker and sync-URL classes of problem are gone); 05 is the one that
still matters, and §5.1 is a security fix, not the "hygiene" its own header once
called it.
