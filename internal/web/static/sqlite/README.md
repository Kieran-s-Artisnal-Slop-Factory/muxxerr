# Vendored SQLite WASM

`sqlite3.mjs` and `sqlite3.wasm` are the official SQLite WebAssembly build,
copied verbatim from the npm package [`@sqlite.org/sqlite-wasm`][pkg]
**version 3.53.0-build1** (`dist/index.mjs` → `sqlite3.mjs`, `dist/sqlite3.wasm`
unchanged). Apache-2.0; SQLite itself is public domain.

## Why these files are checked in

The gateway has no build step and makes no external requests, and both of those
are load-bearing rather than incidental — it is the login page for somebody's
personal server. Pulling a megabyte of WebAssembly off a CDN at page load, or
requiring node to assemble it, would give up one to get the other. Vendoring is
the only option that keeps both, at the cost of ~1.4 MB in the binary and a
manual step when upgrading.

`sqlite3.mjs` locates `sqlite3.wasm` through `import.meta.url`, so the two must
stay in the same directory and no bundler is involved. The gateway serves them
with the same content-addressed URL as every other asset.

## Upgrading

```bash
npm pack @sqlite.org/sqlite-wasm@<version>
tar -xzf sqlite.org-sqlite-wasm-<version>.tgz
cp package/dist/index.mjs    internal/web/static/sqlite/sqlite3.mjs
cp package/dist/sqlite3.wasm internal/web/static/sqlite/sqlite3.wasm
```

Then update the version above, rebuild, and open `/tools/sqlite` — the page
reports the SQLite version it booted, so a mismatched pair fails loudly rather
than silently serving a stale runtime.

## What is deliberately not here

`sqlite3-opfs-async-proxy.js` and the OPFS VFS. Persistent storage in the
browser needs `SharedArrayBuffer`, which needs COOP/COEP headers, which would
change the gateway's headers for every page including the proxied apps. The
viewer holds its database in memory instead: it is a scratch copy of a snapshot,
and losing it on reload is the correct behaviour for what it is.

[pkg]: https://www.npmjs.com/package/@sqlite.org/sqlite-wasm
