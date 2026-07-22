# 05 — Backend hardening for supervised children

**Applies to:** `workoutt/backend`, `readerr/backend`, and the generator's
`emitGoMain.ts` / `emitGoDb.ts`
**Status:** not applied. This is the only backend patch that still matters — the
sync-URL and service-worker patches (01–03) are already merged upstream.
**Mostly optional hardening, with one exception:** §5.1 (bind loopback) is a
live security fix, not hygiene — see
[docs/improvements/security.md §C2](../docs/improvements/security.md#c2). The
other five items are robustness; take any subset.

These backends were written for a stated posture: *"Single-user, self-hosted, no
auth"* (`readerr/backend/main.go` line 3), permissive CORS, LAN only. Every
decision below was reasonable under that posture. Running them as supervised
child processes with other tenants next door changes the assumptions enough that
each is worth a second look — but they are six unrelated items, and taking two
of them and leaving four is a perfectly good outcome.

The gateway already compensates for some of this. Where it does, it is noted.

---

## 5.1 — `BIND_ADDR`, so children are not reachable from the network

`readerr/backend/main.go` line 113, `workoutt/backend/main.go` line 99:

```go
addr := ":" + envOr("PORT", "8080")
```

`":" + port` binds **all interfaces**. Under muxxerr, every running
instance is therefore a no-auth, permissive-CORS sync server listening on the
LAN on an ephemeral port. The gateway's authentication is a front door with the
windows open: anyone who can reach the host and guess the port has full
read/write access to that user's database, with no session and no username.

```diff
-	addr := ":" + envOr("PORT", "8080")
+	// Bind loopback by default. This process is normally either the only thing
+	// on the box (standalone) or a child of a gateway that proxies to it
+	// (multiplexed) — in both cases nothing outside the host should reach it
+	// directly, and in the multiplexed case reaching it directly bypasses
+	// authentication entirely. Set BIND_ADDR=0.0.0.0 to opt back in to LAN
+	// access, e.g. when syncing from a phone with no reverse proxy.
+	addr := envOr("BIND_ADDR", "127.0.0.1") + ":" + envOr("PORT", "8080")
```

**Gateway coverage:** partial and unsatisfying. The supervisor already binds the
child to an ephemeral port and only ever dials `127.0.0.1:<port>` itself, and it
sets `BIND_ADDR=127.0.0.1` in the child's environment — which today the child
ignores. Applying this patch is what makes that setting real. Until then the
only defence is a host firewall.

This is the one item on this page worth doing unconditionally, including for
standalone use.

## 5.2 — `db.SetMaxOpenConns(1)`

`readerr/backend/db.go` line 148, `workoutt/backend/db.go` line 25 — identical:

```go
dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
db, err := sql.Open("sqlite", dsn)
```

`database/sql` then keeps an unbounded pool. WAL plus a 5s busy timeout makes
that survivable — readers do not block, and a writer that collides retries for
five seconds — but `SQLITE_BUSY` under concurrent writes is a real failure mode
and `/sync/push` is one long write transaction.

```diff
 	db, err := sql.Open("sqlite", dsn)
 	if err != nil {
 		return nil, err
 	}
+	// One connection. SQLite serialises writers anyway, so a pool buys nothing
+	// but the opportunity for two goroutines to collide and produce
+	// SQLITE_BUSY; a single connection turns that into a queue. This is a
+	// single-user database — the contention is one browser tab against
+	// (in workoutt's case) the notification scheduler, not a workload.
+	// The gateway's own store does exactly the same thing for the same reason.
+	db.SetMaxOpenConns(1)
```

Relevant to workoutt specifically: its 60-second scheduler goroutine
(`srv.startScheduler`, `main.go` line 76) writes to the same database as the
sync handlers, so it genuinely does have two writers.

## 5.3 — `_txlock=immediate`

Same DSN. Go's SQLite drivers begin transactions in `DEFERRED` mode, which takes
a read lock first and upgrades to a write lock on the first write. If two
transactions both hold read locks and both then try to upgrade, one gets
`SQLITE_BUSY` **immediately** — the busy timeout does not apply to a deadlocked
upgrade, because retrying cannot help while the other side holds its read lock.

`BEGIN IMMEDIATE` takes the write lock up front, so the second transaction waits
politely through the busy timeout instead of failing.

```diff
-	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
+	// _txlock=immediate: begin transactions with BEGIN IMMEDIATE rather than
+	// the default DEFERRED. A deferred transaction takes a read lock and
+	// upgrades on first write; two of those upgrading at once is an instant
+	// SQLITE_BUSY that busy_timeout cannot rescue, because neither side can
+	// make progress. Taking the write lock up front turns that into a wait.
+	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate", path)
```

Largely redundant with 5.2 — a single connection cannot deadlock against
itself. Apply one or both; 5.2 alone is sufficient.

## 5.4 — `VACUUM INTO` for `/backup`

This is the item with an actual correctness bug behind it.

`readerr/backend/sync.go` lines 392-400 and `workoutt/backend/sync.go` lines
329-337, identical apart from the filename:

```go
func (s *server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="readerr-backup.db"`)
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	http.ServeFile(w, r, s.dbPath)
}
```

Checkpoint, then stream the file off disk. Between the `Exec` returning and
`ServeFile` finishing there is no lock held, so **any write that lands during
the transfer tears the download**: `ServeFile` sends a `Content-Length` from the
stat it did at the start and then reads pages that may have changed underneath
it. The result is a file that is the right length and internally inconsistent —
the worst possible failure for a backup, because it looks fine until you restore
it. `wal_checkpoint(TRUNCATE)` itself can also return without checkpointing
(it returns a busy result rather than an error) if a reader is active.

Under muxxerr this gets more likely, not less: the admin export path
proxies `GET /backup` while the user's browser may be mid-sync, and workoutt's
scheduler writes on its own timer regardless of what anyone is doing.

`VACUUM INTO` is SQLite's answer — it produces a consistent snapshot of the
whole database into a new file, transactionally, with no WAL to reconcile:

```diff
 func (s *server) handleBackup(w http.ResponseWriter, r *http.Request) {
-	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
-		http.Error(w, err.Error(), http.StatusInternalServerError)
-		return
-	}
+	// VACUUM INTO, not checkpoint-then-ServeFile. The old approach released
+	// every lock before the transfer began, so a write landing mid-download
+	// produced a backup that was the right length and internally inconsistent
+	// — a corrupt file that looks fine until the day you restore it. VACUUM
+	// INTO snapshots the whole database transactionally into a new file, WAL
+	// already folded in, and costs one extra copy on disk.
+	tmp, err := os.MkdirTemp("", "backup")
+	if err != nil {
+		http.Error(w, err.Error(), http.StatusInternalServerError)
+		return
+	}
+	defer os.RemoveAll(tmp)
+	snapshot := filepath.Join(tmp, "snapshot.db")
+	if _, err := s.db.ExecContext(r.Context(), "VACUUM INTO ?", snapshot); err != nil {
+		slog.Error("backup snapshot", "error", err)
+		http.Error(w, err.Error(), http.StatusInternalServerError)
+		return
+	}
 	w.Header().Set("Content-Disposition", `attachment; filename="readerr-backup.db"`)
 	w.Header().Set("Content-Type", "application/vnd.sqlite3")
-	http.ServeFile(w, r, s.dbPath)
+	http.ServeFile(w, r, snapshot)
 }
```

Adds `os` and `path/filepath` to the imports (`sync.go` in both apps currently
has neither; `log/slog` is already imported, used by `writeJSON` below).

Costs: a full copy of the database on disk for the duration of the download, in
the OS temp directory. For these apps that is single-digit megabytes. If that is
not acceptable, put the temp file next to the database instead
(`filepath.Dir(s.dbPath)`) so it lands on the same volume and inside the
instance's own directory — which under muxxerr is the tidier choice
anyway, since each instance directory is already isolated per (user, app).

`VACUUM INTO` also compacts, so the backup is smaller than the live file. That
is a bonus, not the point.

## 5.5 — `MaxBytesReader` on `/sync/push`

`readerr/backend/sync.go` line 156, `workoutt/backend/sync.go` line 173:

```go
func (s *server) handlePush(w http.ResponseWriter, r *http.Request) {
	var req pushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
```

The decoder reads until the body ends. A client that never stops sending makes
the server allocate until it does not. In a single-user LAN app the only client
is you; multiplexed, one user's runaway (or hostile) client is a memory
exhaustion vector against a process that shares a machine with everyone else's.

```diff
 func (s *server) handlePush(w http.ResponseWriter, r *http.Request) {
+	// Cap the request body. The decoder otherwise reads until the client stops
+	// sending, and a push is the one endpoint that accepts unbounded input.
+	// 64 MiB is far above any real push — the client batches by row count, and
+	// a full first sync of a multi-year database is single-digit megabytes of
+	// JSON — while still being a bound. MaxBytesReader makes the decode fail
+	// with a clear error rather than the connection dying mid-parse.
+	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
 	var req pushRequest
 	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
```

The existing error path already returns 400 with the decode error, which is the
right response for an over-large body too.

**Gateway coverage:** the gateway can and should cap request bodies at the proxy
before they ever reach a child. That is the better place for a global limit.
This patch is defence in depth for the standalone case.

## 5.6 — `http.Server` with timeouts

`readerr/backend/main.go` line 115, `workoutt/backend/main.go` line 101:

```go
if err := http.ListenAndServe(addr, withCORS(mux)); err != nil {
```

`ListenAndServe` uses `http.DefaultServeMux`'s zero-valued server: no read
timeout, no write timeout, no idle timeout, no header timeout. A connection that
opens and says nothing is held forever. Same reasoning as 5.5 — irrelevant when
the only client is your own browser, relevant when the process is one of many
and a stuck goroutine is charged to a shared machine.

```diff
-	addr := ":" + envOr("PORT", "8080")
+	addr := envOr("BIND_ADDR", "127.0.0.1") + ":" + envOr("PORT", "8080")
+	// Explicit timeouts. ListenAndServe's zero-valued server has none, so a
+	// connection that opens and never speaks is held until the process exits.
+	// WriteTimeout is generous because /backup streams the whole database and
+	// /title waits on a remote host (its own client already caps at 10s).
+	srvHTTP := &http.Server{
+		Addr:              addr,
+		Handler:           withCORS(mux),
+		ReadHeaderTimeout: 10 * time.Second,
+		ReadTimeout:       2 * time.Minute,
+		WriteTimeout:      5 * time.Minute,
+		IdleTimeout:       2 * time.Minute,
+	}
 	slog.Info("readerr backend listening", "addr", addr, "db", dbPath)
-	if err := http.ListenAndServe(addr, withCORS(mux)); err != nil {
+	if err := srvHTTP.ListenAndServe(); err != nil {
 		slog.Error("server exited", "error", err)
 		os.Exit(1)
 	}
```

readerr's `main.go` does not currently import `time`; workoutt's does.

A note on what this does **not** add: graceful shutdown on `SIGTERM`. It is the
obvious next line, and it is deliberately absent — muxxerr's supervisor
already sends `SIGTERM` and waits before escalating, and SQLite in WAL mode
recovers cleanly from an abrupt exit, so the added complexity buys very little.
If you add it anyway, `srvHTTP.Shutdown(ctx)` on a signal is the whole change.

---

## Which of these to actually take

If you take one: **5.1**. An unauthenticated sync server on `0.0.0.0` is the
only item here that is a security problem rather than a robustness one.

If you take two: **5.1 and 5.4**. A backup that silently corrupts is the only
item that loses data.

The rest are hygiene. They matter more the further you get from "one person, one
laptop, one app", which is exactly the direction muxxerr points.

## Verify

- **5.1:** `ss -ltnp | grep <child pid>` (or `netstat -ano` on Windows) must show
  `127.0.0.1:<port>`, not `0.0.0.0:<port>`. From another machine on the LAN,
  `curl http://<host>:<port>/healthz` must fail to connect while the same call
  through the gateway (`/alice/readerr/healthz`, authenticated) succeeds.
- **5.2 / 5.3:** run a push loop against workoutt with the scheduler interval
  set low (`NOTIFY_INTERVAL_SECONDS=1`) and confirm no `SQLITE_BUSY` in the
  logs.
- **5.4:** start a large `/backup` download, write during it, then
  `sqlite3 downloaded.db "PRAGMA integrity_check;"` — must print `ok`. Doing the
  same before the patch is how you reproduce the bug.
- **5.5:** `curl -X POST --data-binary @100mb.json .../sync/push` must return
  400 promptly rather than growing the process RSS.
- **5.6:** open a raw socket, send nothing, and confirm it is closed within
  `ReadHeaderTimeout` rather than held open.
