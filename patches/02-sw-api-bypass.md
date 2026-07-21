# 02 — Keep the service worker's hands off the API

**Applies to:** workoutt and readerr (identical change, one line each)
**Status:** not applied
**Apply with:** [01-sync-base.md](01-sync-base.md). Applying 01 without 02 is
worse than applying neither.

## The problem

Both service workers open with the same guard:

`frontend/public/sw.js`, line 27 in both apps:

```js
// Never cache dev-server module URLs — serving them stale breaks the app
// after code changes. (The worker shouldn't be registered in dev at all,
// but belt and braces.)
const UNCACHEABLE = [/\/src\//, /\/@/, /\/node_modules\//];
```

and in the `fetch` listener (readerr line 96, workoutt line 135):

```js
if (UNCACHEABLE.some((re) => re.test(url.pathname))) return;
```

Everything not matched falls through to the handler below, whose non-navigation
branch is **stale-while-revalidate**: serve the cached copy immediately, refresh
behind it.

Today that is harmless. The worker's scope is `/` (root) or `/readerr/` (Pages),
and the API is served root-absolute at `/sync/pull` — either outside the scope
entirely, or *inside* it at the root where the SW registration path
(`<base>sw.js` → scope `/`) does technically cover it. The reason nobody has hit
this is that the pull URL carries `?since=<server_seq>`, which changes on every
successful sync, so each request is a fresh cache key and the stale copy is
never the one you want back.

That is luck, not design, and patch 01 removes it. Once `getSyncUrl()` returns
the build base, the API lives at `/alice/readerr/sync/pull` — squarely inside
the worker's scope — and any request whose URL repeats gets served from cache
first. `/healthz`, `/dbsize`, `/title?url=<same url>` and a re-issued
`/sync/pull?since=<same seq>` (which is exactly what a retry after a failed
apply looks like) all repeat. A stale sync response is silent data loss dressed
up as a successful sync.

## The fix

Add the API paths to `UNCACHEABLE`. It is already the mechanism for "this
request must always hit the network", and the `fetch` listener already returns
early on a match, which hands the request straight to the browser untouched.

`frontend/public/sw.js`, both apps, replacing line 27:

```diff
-// Never cache dev-server module URLs — serving them stale breaks the app
-// after code changes. (The worker shouldn't be registered in dev at all,
-// but belt and braces.)
-const UNCACHEABLE = [/\/src\//, /\/@/, /\/node_modules\//];
+// Requests that must always hit the network, never the cache.
+//
+// Dev-server module URLs: serving them stale breaks the app after code
+// changes. (The worker shouldn't be registered in dev at all, but belt and
+// braces.)
+//
+// API endpoints: these are under the app's own base once the client resolves
+// its sync URL from BASE_URL, which puts them inside this worker's scope. A
+// stale-while-revalidate /sync/pull hands the client a response it has already
+// applied and reports success — silent data loss. Matched on pathname, so the
+// query string (?since=, ?url=) is irrelevant.
+const UNCACHEABLE = [
+  /\/src\//,
+  /\/@/,
+  /\/node_modules\//,
+  /\/sync\//,
+  /\/healthz$/,
+  /\/backup$/,
+  /\/push\//,
+  /\/title$/,
+  /\/dbsize$/,
+];
```

Use the same list in both apps even though neither uses all of it — workoutt has
no `/title` or `/dbsize`, readerr has no `/push/`. Keeping the two files
identical here is worth more than the three regexes it saves: these two service
workers are otherwise line-for-line the same file, and the moment they diverge
cosmetically nobody diffs them again.

Anchoring matters. `/healthz`, `/backup`, `/title` and `/dbsize` are exact
endpoints and are anchored with `$` so a page route like `/backups/` is not
caught. `/sync/` and `/push/` are prefixes with a trailing slash, which is what
distinguishes them from a hypothetical `/synced` page. Neither is anchored at
the start, deliberately — the pathname is `/alice/readerr/sync/pull`, and the
prefix is not known to the worker at authoring time.

## Already done upstream in the generator

`local-sync-template` got this right when its service worker was written.
`src/lib/generator/frontend/emitServiceWorker.ts` emits an `apiExclusion` block
(lines 28-39 of the emitter) directly into the generated `fetch` handler:

```js
  // Never cache the sync/API endpoints — the Go backend serves them at the
  // origin root regardless of base, and they must always hit the server.
  if (
    url.pathname.startsWith('/sync/') ||
    url.pathname === '/healthz' ||
    url.pathname === '/backup'
  ) {
    return;
  }
```

It is emitted only when `ctx.includeBackend` is true, which is correct — an
offline-only generated app has no such endpoints.

So **newly generated apps already carry this fix**; only the hand-written
workoutt and readerr copies lag. Note the generator's version tests exact
root-absolute paths (`startsWith('/sync/')`, `=== '/healthz'`), which stops
being true under a prefix — see [04-generator.md](04-generator.md) for the
follow-up that makes it prefix-agnostic.

## Verify

1. Apply 01 and 02, rebuild through `muxbuild`.
2. Application → Service Workers → **Unregister**, then hard-reload. The
   previously installed worker stays in control of open clients until it is
   replaced; skipping this makes a correct patch look broken.
3. DevTools → Network: the `/alice/readerr/sync/pull` row must have an empty
   "Size" initiator note — no "(ServiceWorker)".
4. Change data on another device, sync twice. The second pull must bring the
   new rows. Two identical response bodies means the worker is still serving
   cache.
5. Offline check, to prove nothing else regressed: go offline and navigate
   around. Pages and assets must still come from cache; only the API should
   fail, and the app should report a sync error rather than a blank page.
