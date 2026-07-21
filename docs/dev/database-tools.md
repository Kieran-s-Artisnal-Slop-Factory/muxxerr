# The database tools

Two pages sit under **Tools** in the nav. They look adjacent and are not: one
cannot touch your data and the other can destroy it. That difference is the
entire design, so it is worth being explicit about.

| | SQLite viewer | SQL console |
|---|---|---|
| Path | `/tools/sqlite` | `/tools/sql` |
| Runs where | Your browser | The server |
| Touches the live database | **Never** | **Always** |
| Available | Always | Only if `site.sql_console` is true |
| Gate | None | Type the warning sentence, every visit |

## Why they are separate pages

The obvious design is one page with a "write mode" checkbox. It is worse, for a
reason that only shows up later: the safe operation then carries the dangerous
one's warnings. Someone browsing their reading list to work out why a sync
failed would have to read "this may destroy your data" every time, and after
the fourth time they stop reading it — including on the day it matters.

Splitting them means the viewer can be genuinely relaxed (it is a copy; nothing
you type there can escape the tab) and the console can be genuinely alarming.

## The viewer

The frontend loads SQLite compiled to WebAssembly and runs every query
client-side. The database it operates on is one of:

- **a snapshot of one of your apps**, fetched from the same `/apps/<app>/export`
  endpoint the dashboard's "Download data" link uses — `VACUUM INTO`, so it is a
  consistent point-in-time copy taken without blocking the running app;
- **a `.db` file you upload**, which never leaves your machine; or
- **an empty database**, for scratch work.

Once loaded it is bytes in a tab. `UPDATE`, `DROP`, anything — it applies to
the copy, the server never hears about it, and a reload throws it away. That is
what "should not modify data" means here: the isolation is structural rather
than a list of forbidden keywords, which is the only version of it that cannot
be worked around.

Exports come back out of the browser as a `.db` file or as a SQL dump.

### Implementation notes

- `internal/web/static/sqlite/` holds the vendored `sqlite3.mjs` and
  `sqlite3.wasm`. See the README there for provenance and how to upgrade.
- `sqliteworker.js` runs SQLite in a **Web Worker**, so a query with a cartesian
  join does not freeze the page it is running on.
- The database is **in-memory, not OPFS**. Persistent browser storage needs
  `SharedArrayBuffer`, which needs COOP/COEP headers, which would change the
  headers on every page including the proxied apps. Losing a scratch copy on
  reload is the right trade.
- `.wasm` is served as `application/wasm` explicitly, because
  `WebAssembly.instantiateStreaming` refuses anything else and reports it as
  something that reads like a network error.
- This page is the one place in the gateway that **requires JavaScript**. It
  runs a database engine in your browser; there is no server-rendered version of
  that. The `<noscript>` block says so and points at the download link, which is
  the same file by other means.

## The console

Off unless `apps.json` says so:

```json
{ "site": { "sql_console": true } }
```

Deliberately not a toggle in the admin UI. On a server with open sign-ups, an
admin checkbox is one misclick away from handing every account a tool that can
drop a table; putting it in a file the operator edits makes it a decision rather
than an accident.

Three things guard it, each for a different failure:

1. **The config flag** — stops it existing at all on servers that should not
   have it.
2. **The typed phrase, on every visit** — the token that unlocks the page lives
   only in the page you just unlocked, so a bookmark, the back button, or a tab
   left open overnight all put the warning back in front of you. The risk being
   defended against is not mistyping; it is forgetting which database this tab
   was pointed at.
3. **Stopping the instance first** — two processes writing one SQLite file is
   survivable right up until one of them changes the schema the other has
   cached. The child is stopped before anything runs and starts again on the
   owner's next request.

Every unlock, failed unlock, and execution is written to the audit log with the
first line of the statement.

### What it does not do

- **No automatic backup.** It would be easy to `VACUUM INTO` first and it is
  deliberately not done: a backup nobody asked for creates a second copy of the
  data on disk, and implying safety that has not been arranged for is worse than
  stating plainly that there is none. The warning says to take one, and
  "Download data" is one click away.
- **No transaction wrapper.** The caller may be running their own `BEGIN` and
  `COMMIT`, or a `PRAGMA` that cannot run inside a transaction. Silently
  wrapping would make the console lie about what it did.
- **No statement filtering.** There is no allowlist of "safe" verbs. A tool
  whose stated purpose is arbitrary SQL should not pretend to have guardrails it
  cannot enforce — `PRAGMA writable_schema` alone makes any keyword blocklist
  decorative.

### Known limitation

`splitStatements` finds statement boundaries without being a SQL parser. It
handles semicolons inside string literals, quoted identifiers, and both comment
styles. It does **not** understand `BEGIN … END` in a trigger body, so a
`CREATE TRIGGER` has to be run on its own. This is tested in
`internal/web/sqlconsole_test.go` and called out in the page's help text.
