# Authentication

Everything credential-shaped lives in two packages:
[`internal/auth`](../../internal/auth) (hashing, passphrases, tokens — never
touches a database) and [`internal/store`](../../internal/store) (persistence —
never sees a plaintext secret). The split is deliberate: `store.CreateUser`
takes *hashes*, so there is no code path where the database layer could
accidentally log or store a password.

The threat model, stated plainly, is **someone walks off with the SQLite file**.
That is the realistic failure for a box in a closet: a stolen laptop, a backup
copied to the wrong place, a synced folder. Everything below is aimed at making
that file useless on its own.

Not in the threat model: an attacker with code execution on the host (they have
the pepper too, and can read every instance database directly), a malicious
child process (see "What this is not" in the [README](../../README.md)), or a
shared browser profile (covered in [operations.md](../admin/operations.md)).

One thing this front door does **not** cover, worth stating here because it is
easy to assume otherwise: the app backends behind it do no authentication of
their own and currently listen on all interfaces, so from a from-source
deployment they are reachable — unauthenticated — on the LAN, sidestepping every
protection on this page. See
[docs/improvements/security.md §C2](../improvements/security.md#c2).

## Password hashing: Argon2id

[`internal/auth/password.go`](../../internal/auth/password.go):

```go
argonTime    uint32 = 3
argonMemory  uint32 = 64 * 1024 // KiB → 64 MiB
argonThreads uint8  = 4
argonKeyLen  uint32 = 32
saltLen             = 16
```

64 MiB / 3 passes / 4 lanes is OWASP's second recommended configuration, and it
costs roughly 50-100 ms on a modern core. That number is the whole design: slow
enough that offline guessing is expensive per attempt, fast enough that a login
does not feel stalled and a request does not hold a worker for a noticeable
time. Argon2**id** rather than Argon2i or Argon2d because it is the hybrid —
side-channel resistant on the first pass, GPU-hostile thereafter — and it is
what you should pick absent a specific reason.

Memory-hardness is the point. bcrypt and PBKDF2 are compute-hard but cheap in
silicon; a memory-hard function makes a GPU or ASIC attacker pay for 64 MiB per
concurrent guess, which is what actually collapses their parallelism advantage.

Hashes are stored in PHC format, parameters inline:

```
$argon2id$v=19$m=65536,t=3,p=4$<b64 salt>$<b64 hash>
```

That is what makes the parameters changeable. `Verify` decodes the cost from the
stored string, so raising `argonMemory` tomorrow still verifies every existing
hash; `NeedsRehash` then reports that a record used weaker parameters, and a
successful login quietly re-hashes it. No migration, no forced reset, no
stranded accounts.

A 16-byte random salt per record means two users with the same password have
different hashes and a precomputed table is worthless.

### The one password rule

```go
const MinPasswordLength = 10
```

Length, and nothing else. Composition rules ("one uppercase, one digit, one
symbol") reliably produce `Password1!` — they push people toward predictable
mutations of a short base word, which is the pattern every cracking wordlist
already encodes. Length is what actually multiplies an attacker's work.

There is also a 1024-byte ceiling. Argon2 handles long inputs fine, but an
unbounded password is a free way to make the server do arbitrary work, and
nobody's real password is a megabyte.

## The pepper

[`internal/auth/secret.go`](../../internal/auth/secret.go). The pepper is a
32-byte server-wide secret that is mixed into every hash and **stored outside
the database**.

```go
func peppered(pepper Pepper, secret string) []byte {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(secret))
	return mac.Sum(nil)
}
```

HMAC rather than concatenation. Concatenating (`pepper + password`) works but
invites two problems: the pepper's length becomes part of the input space in
ways that interact badly with length-extension reasoning, and `pepper="ab",
pw="c"` collides with `pepper="a", pw="bc"`. HMAC gives a fixed-size,
unambiguous, keyed digest and sidesteps both.

**What the pepper protects against, precisely:** offline attack on a stolen
database. The salt stops precomputation but is stored alongside the hash, so a
thief with the file can still grind guesses. The pepper is not in the file. An
attacker with only `mux.db` cannot verify a single guess — not slowly, not
expensively, not at all. They are testing candidates against an HMAC key they do
not have.

**Where it lives**, in resolution order:

1. `MUX_PEPPER` — base64, at least 16 bytes decoded. Checked first, because the
   whole point is to keep it out of the data volume, and an environment variable
   (or a systemd credential, or a secrets manager) is how you do that.
2. `<data_dir>/pepper.key` — base64, mode 0600, generated on first run. The
   default, because a scheme nobody can start without configuring is a scheme
   nobody uses.

**If it is lost, there is no recovery.** Not "hard", not "requires support" —
every stored password hash and every stored passphrase hash becomes permanently
unverifiable, because every guess is now HMAC'd under a different key. The only
remedy is an admin resetting every account by hand, and if the *admin's*
password is in that database, that means editing the database directly. Back up
`pepper.key`, and back it up **separately from `mux.db`** — a backup containing
both is a backup with no pepper at all. [operations.md](../admin/operations.md)
covers this again because it is the single most consequential operational fact
in the system.

## Recovery passphrases

There is no mail server, so there is no reset link. Instead
[`internal/auth/passphrase.go`](../../internal/auth/passphrase.go) hands the user
a passphrase once, at sign-up:

```
meadow-cobalt-jigsaw-hornet-fresco-lantern
```

Presenting it later proves account ownership well enough to set a new password.
It is hashed exactly like the password — same Argon2id, same pepper — so an
admin reading the database cannot recover it either.

### Entropy

The wordlist has **exactly 256 entries** (asserted by
`TestWordlistIsExactly256`, and by a length check in `GeneratePassphrase`
itself), so one word is exactly 8 bits with no modulo bias. Six words:

```
6 words × 8 bits = 48 bits ≈ 2.8 × 10^14 combinations
```

48 bits is weak by key-material standards and enormous by human-password
standards — far beyond anything a person picks, and this secret never faces an
offline attack: the reset path is throttled and lockout-backed, so an attacker
gets a handful of online guesses before exponential backoff makes the effort
pointless. Against online guessing, 48 bits is overwhelming.

Generation uses `crypto/rand.Int` over the exact list length rather than
`randomByte % 256`. The modulo would be unbiased at exactly 256 entries, but it
would silently *become* biased if anyone ever resized the list, and the
guarantee is worth more than the nanoseconds.

The words themselves are chosen to survive being read off a screen and retyped:
nothing under four letters, no homophones, no plurals of each other.

### Normalisation

`NormalisePassphrase` lowercases, keeps letters and digits, converts spaces,
dashes, underscores and whitespace to a single `-`, collapses runs, and trims
the ends. So `Meadow Cobalt  JIGSAW-hornet_fresco lantern` verifies.

**The normalised form is what gets hashed**, which makes this function
effectively frozen: changing it invalidates every stored passphrase in the
world, with no way to detect or migrate. Treat it as schema.

## Sessions

Schema in [`internal/store/schema.sql`](../../internal/store/schema.sql).

A token is 256 bits from `crypto/rand`, base64url-encoded
(`auth.NewSessionToken`). **Only its SHA-256 is stored** (`store.HashToken`):

```sql
CREATE TABLE IF NOT EXISTS sessions (
    token_hash     TEXT    PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_gen INTEGER NOT NULL,
    ...
);
```

The plaintext token never touches disk, so the stolen-database attacker cannot
replay a live session — the same reasoning as password hashing, one level up.

SHA-256 and not Argon2id, deliberately. This is checked on *every request*, and
the input is 256 bits of uniform randomness: there is nothing to brute-force, so
there is no work factor to add. A slow hash here would only make the server
slow.

### `credential_gen` invalidates sessions

Every user row carries `credential_gen`, and every session records the value it
was minted with. `SessionUser` joins on equality:

```sql
SELECT ... FROM sessions s JOIN users u ON u.id = s.user_id
 WHERE s.token_hash = ?
   AND s.expires_at > ?
   AND s.credential_gen = u.credential_gen
   AND u.is_disabled = 0
```

`SetCredentials` does `credential_gen = credential_gen + 1`, so **changing a
password logs out every other device instantly**, with no `DELETE FROM sessions`
sweep, no cache to invalidate, and no window where a stale session still works.
Disabling an account is the same trick with a different column, in the same
query.

This is why one lookup answers four questions — known token, not expired, not
superseded, not disabled — and why there is no code path that checks three of
them and forgets the fourth.

`PurgeExpiredSessions` runs periodically as housekeeping only; correctness never
depends on it having run.

Default TTL is `session_ttl` in [apps.json](../../apps.json), shipped at `720h`
(30 days). Long, because this is a personal-scale tool where re-authenticating
weekly is friction with no matching benefit, and because a password change ends
every session anyway.

`secure_cookies: false` is the default and is wrong the moment you put this
behind TLS. Set it to `true` then. It ships false so that plain-HTTP LAN use
works out of the box rather than failing in a way that looks like a bug.

## Throttling

[`internal/store/store.go`](../../internal/store/store.go), `throttle` table.
Keys are `"<kind>:<username>"` and `"<kind>:ip:<addr>"` — **both**, so an
attacker cannot lock a victim out by hammering their username from anywhere, and
cannot dodge a lockout by rotating usernames from one address.

```go
if fails > freeAttempts {
    d := base << min(fails-freeAttempts-1, 16)
    if d > max || d <= 0 {
        d = max
    }
    until = time.Now().UTC().Add(d)
}
```

The first few attempts are free — real people mistype passwords, and a system
that punishes the first mistake is a system people learn to resent. After that
the delay doubles per failure, capped at `max`. The `min(…, 16)` guards the
shift itself: without it a determined attacker could overflow `base << n` into a
negative duration, which is why `d <= 0` is also treated as "use the cap".

`ClearThrottle` on success, so a successful login wipes the slate.
`PurgeStaleThrottles` drops counters that have stopped mattering, so the table
does not grow without bound.

The parameters (`freeAttempts`, `base`, `max`) are the caller's, not the
store's — login and password reset want different aggressiveness, and hardcoding
one policy in the persistence layer would prevent that.

## Timing

`auth.FakeVerify` exists for one purpose: when a login names a user that does
not exist, the handler still burns a full Argon2id verification against a dummy
hash. Without it, "no such user" returns in microseconds and "wrong password"
returns in ~80 ms, and that difference is a username oracle — an attacker
enumerates valid accounts for free before guessing anything.

`Verify` uses `subtle.ConstantTimeCompare`, and returns `ErrMismatch` for a
wrong secret and `ErrHashInvalid` for a corrupt record. **Callers must report
both to the user identically.** A distinguishable "your stored hash is corrupt"
message leaks account existence just as effectively.

## Admin

The first account created is an admin. There is no separate bootstrap step, no
default credentials, and no environment variable that creates a superuser — the
common ways to end up with an `admin/admin` on the internet.

`allow_admin_impersonation` defaults to **false**. An admin can already export
any user's database, so impersonation adds no capability they lack; what it adds
is the ability to browse as someone else without an obvious trace, and that is a
bigger power than this system needs by default. Enabling it is a deliberate
config change, and every use of it should be in the audit log.

`store.Audit` records security-relevant actions (logins, resets, admin actions)
with actor, target, detail and IP. It is a trail, not a control: it is in the
same database as everything else, and anyone who can edit that file can edit the
log.

## Deliberately absent

- **TOTP / WebAuthn.** Worth having. Not here yet. `credential_gen` and the
  session table are the pieces a second factor would hook into.
- **Password strength estimation** (zxcvbn and friends). Would need a
  dependency and a wordlist larger than this entire project, to tell users
  something a length minimum mostly already covers.
- **Email anything.** No verification, no reset link, no notification. That is
  the constraint the passphrase scheme exists to work around, not an oversight.
- **Per-app permissions.** A user either has an instance of an app or does not.
  There is no read-only, no sharing, and no role beyond admin.
