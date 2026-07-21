# Integration tests

`go test ./...` covers the packages. These two cover the thing packages cannot:
the running server, with real HTTP, real cookies, real child processes.

They are separate on purpose, because they fail in different ways.

## `api.sh` — the HTTP contract

51 assertions driving the gateway with curl: sign-up and admin bootstrap,
provisioning, base rewriting, the account guard, sync through both the prefixed
path and the Referer shim, tenant isolation in all three directions, the
navigation-vs-fetch auth challenge, deep-link login redirects, the SSRF guard,
and database export.

```bash
go run ./cmd/mux -config apps.json -addr 127.0.0.1:8099 &
bash test/api.sh
```

Set `B` to point somewhere else. It expects an **empty data directory** — it
signs up `kieran` and `alex` and will fail on a second run against the same
database.

> On Windows, run it from Git Bash. It exports `MSYS_NO_PATHCONV=1` because
> MSYS rewrites any argument that looks like a Unix path — `/kieran/readerr/`
> becomes `C:/Program Files/Git/kieran/readerr/` before curl ever sees it. That
> cost an hour of chasing a redirect bug that did not exist.

## `forms.py` — the UI actually works

26 assertions that parse the HTML the server returns and submit **the forms it
actually rendered**, following its redirects, rather than posting to URLs chosen
by whoever wrote the test.

This exists because `api.sh` was completely green while the Add button, all
three account forms and the post-signup Continue button were dead: the templates
posted to URLs the router did not serve. An API-level test cannot catch that,
because it never reads the page. If you add a form, this is the suite that keeps
it honest — along with `TestEveryFormActionIsRouted` in `internal/web`, which
checks the same thing without a server.

```bash
python test/forms.py
```

Stdlib only, no dependencies. Also expects an empty data directory.

## `console.py` — the dangerous tool stays hard to reach

36 assertions over the live SQL console and the snapshot viewer's plumbing. Most
of them are about *not* being able to do something: that the console always
opens locked, that the phrase is checked exactly (but forgives whitespace), that
an unlock token is single-use, that a non-admin is neither offered nor allowed
another user's database, and that both tools need a session.

The ones that confirm it does work are deliberately blunt — it creates a table,
writes a row, and reads it back through a second execution to prove the change
reached the real file rather than a copy.

```bash
python test/console.py
```

Needs `"sql_console": true` in apps.json, and an empty data directory.

## What none of these cover

The snapshot viewer itself. It is SQLite compiled to WebAssembly running in a
Web Worker, and there is no way to exercise that from Python — `console.py` only
checks that the page, the module, the worker and the `.wasm` are served with the
right content types. The behaviour has to be confirmed in a browser:

1. Open `/tools/sqlite`, wait for the footnote to show a SQLite version.
2. Load an app snapshot; the table rail should fill with row counts.
3. Run `DELETE FROM <table>;` and confirm the app itself still has its data —
   that is the isolation the whole feature rests on.
4. Drop a non-database file on the page and check the error names the problem.
