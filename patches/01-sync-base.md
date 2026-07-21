# 01 — Make the sync base default to the build base

**Applies to:** workoutt and readerr (identical change, one line each)
**Status:** not applied
**Apply with:** [02-sw-api-bypass.md](02-sw-api-bypass.md) — see the ordering
note in [README.md](README.md)

## The problem

Both apps resolve their API base through one function:

```ts
export function getSyncUrl(): string {
  return localStorage.getItem(SYNC_URL_KEY) ?? '';
}
```

Every caller then does `fetch(`${base}/sync/pull`)`. With no configured URL,
`base` is `''` and the request is **root-absolute**: `/sync/pull`.

That is exactly right in the deployment the apps were written for — the Go
backend serves the built frontend, so the origin root *is* the backend. It is
wrong under the multiplexer, where the same origin hosts many instances and the
backend for this user's copy lives at `/alice/readerr/sync/pull`.

The gateway compensates with the `Referer` shim (`internal/gateway/shim.go`).
It works. It is also a guess that fails whenever the browser withholds the
header. The fix is to stop discarding information the build already has.

## The fix

Astro's `--base` flag sets `import.meta.env.BASE_URL`, always with a trailing
slash: `/` at the root, `/readerr/` on GitHub Pages, `/__MUX__/` when built by
`muxbuild`. Trimming the trailing slash turns it into exactly the prefix the
`${base}/sync/pull` template expects — and at the root it trims to `''`, which
is byte-for-byte today's behaviour.

The apps already have `lib/paths.ts` doing this for links (`href()`); this is
the same idea for API calls. It is deliberately *not* routed through `href()`,
because `href()` returns a slash-terminated path and every call site here
concatenates a leading-slash path onto the base.

### readerr — `frontend/src/lib/sync.ts`

Currently at lines 36-38:

```diff
 export function getSyncUrl(): string {
-  return localStorage.getItem(SYNC_URL_KEY) ?? '';
+  // No configured URL means same-origin. The base the app was built with is
+  // the correct same-origin prefix: '' at the root (the Go backend serving
+  // its own frontend), '/readerr' on GitHub Pages, '/alice/readerr' behind
+  // the multiplexer, whose sentinel base is rewritten per user at serve time.
+  // BASE_URL always ends in '/', which the templates supply themselves.
+  return localStorage.getItem(SYNC_URL_KEY) ?? import.meta.env.BASE_URL.replace(/\/+$/, '');
 }
```

### workoutt — `frontend/src/lib/sync.ts`

Identical, currently at lines 35-37 (the file is two lines shorter — readerr
imports `recordSyncEvent` from `./services/syncLog`, workoutt has no sync log):

```diff
 export function getSyncUrl(): string {
-  return localStorage.getItem(SYNC_URL_KEY) ?? '';
+  // No configured URL means same-origin. The base the app was built with is
+  // the correct same-origin prefix: '' at the root (the Go backend serving
+  // its own frontend), '/workoutt' on GitHub Pages, '/alice/workoutt' behind
+  // the multiplexer, whose sentinel base is rewritten per user at serve time.
+  // BASE_URL always ends in '/', which the templates supply themselves.
+  return localStorage.getItem(SYNC_URL_KEY) ?? import.meta.env.BASE_URL.replace(/\/+$/, '');
 }
```

## What else this fixes for free

`getSyncUrl()` is the single source of the API base in both apps, so two more
root-absolute endpoints in readerr are corrected by the same edit — **no
further changes needed**, listed here only so you know they were checked:

- `frontend/src/lib/services/capture.ts` — `fetchTitles()` reads
  `const base = getSyncUrl();` (line 391) and passes it into
  `fetchTitleViaBackend()`, whose fetch is
  `fetch(`${base}/title?url=${encodeURIComponent(url)}`)` at **line 346**.
- `frontend/src/lib/services/stats.ts` — `fetch(`${getSyncUrl()}/dbsize`)` at
  **line 172**.

workoutt has no equivalent extra endpoints on the client side; its `/push/*`
calls are the only others, and they go through the same base.

## Why not the alternatives

- **Hardcode the prefix at build time.** Requires a per-user build. The whole
  point of the sentinel-base design is that one build serves every user; see
  [docs/dev/architecture.md](../docs/dev/architecture.md).
- **Rewrite the JS bundle's `fetch` calls in the gateway.** The gateway does
  rewrite `/__MUX__` inside JS, but `'/sync/pull'` contains no sentinel — you
  would be pattern-matching arbitrary string literals in minified output. No.
- **Inject a `<base>` tag or a global `window.__MUX_BASE__`.** Works, but it
  makes the app depend on the multiplexer to run at all, and it does not reach
  code running inside the service worker. `BASE_URL` is already there, already
  build-time-correct, and already the mechanism `paths.ts` uses.
- **Keep relying on the shim.** It stays in the gateway either way, for
  unpatched apps. This patch just means the patched app never needs it.

## Verify

Rebuild through `muxbuild` (a plain `npm run build` has no `--base` and will
show no difference), then in DevTools → Network confirm sync requests are
`/alice/readerr/sync/pull`. The gateway's shim debug line
(`shim: re-attributed root-absolute request`) must stop appearing for this app.

Then confirm the no-op property: build standalone with no `--base`, serve with
the Go backend, and check the requests are still bare `/sync/pull`.
