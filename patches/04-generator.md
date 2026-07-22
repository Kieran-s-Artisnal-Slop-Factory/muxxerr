# 04 — Make generated apps multiplex-ready out of the box

**Applies to:** `local-sync-template` (the generator, not any generated app)
**Status:** not applied

Patches 01-03 fix two apps that already exist. This one fixes every app that
does not exist yet, which is the more valuable of the two — a generated app that
needs hand-patching before it can be mounted is a generated app nobody will
bother mounting.

The generator is a set of pure emitters (`EmitContext -> GeneratedFile[]`,
`src/lib/generator/types.ts`) wired together in
`src/lib/generator/index.ts`. Adding or changing behaviour means editing one
emitter, or adding one to the `EMITTERS` / `BACKEND_EMITTERS` arrays.

## 4.1 — The sync base fix, in `emitSyncTs.ts`

This is patch 01, applied at the source. `src/lib/generator/frontend/emitSyncTs.ts`
emits the generated `frontend/src/lib/sync.ts`; the emitted text currently
contains (emitter lines 46-48, which land at lines 35-37 of the generated file):

```ts
export function getSyncUrl(): string {
  return localStorage.getItem(SYNC_URL_KEY) ?? '';
}
```

Inside the emitter this sits in a template literal, so nothing needs escaping —
there is no backtick or `${` in the replacement:

```diff
 export function getSyncUrl(): string {
-  return localStorage.getItem(SYNC_URL_KEY) ?? '';
+  // No configured URL means same-origin, and the base the app was built with
+  // is the correct same-origin prefix: '' at the root (the Go backend serving
+  // its own frontend), '/${app}' under a sub-path like GitHub Pages, and
+  // '/<user>/${app}' behind a muxxerr that mounts many instances on one
+  // host. BASE_URL always ends in '/'; every call site supplies its own.
+  return localStorage.getItem(SYNC_URL_KEY) ?? import.meta.env.BASE_URL.replace(/\\/+$/, '');
 }
```

Two things to get right, both of which the emitter's own header comment warns
about ("every backtick/dollar below is escaped so it lands as literal source
text"):

- The regex `/\/+$/` must be written `/\\/+$/` in the emitter source, or the
  `\/` is consumed by the template literal.
- `${app}` in the comment interpolates the app name at generation time, which
  is what you want. If you would rather the comment be generic, write `\${app}`
  or drop the interpolation.

The emitter already returns `[]` when `ctx.includeBackend` is false, so
offline-only apps are unaffected.

## 4.2 — Make the service worker's API exclusion prefix-agnostic

**Verified fact, and the reason this section is small: the generator already
excludes the API from the cache.** `src/lib/generator/frontend/emitServiceWorker.ts`
builds an `apiExclusion` string (emitter lines 28-39) that it splices into the
generated `fetch` handler, emitted only when `ctx.includeBackend`:

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

Generated apps therefore do **not** need patch 02. What they do need is for that
test to survive 4.1, because the comment's premise ("the Go backend serves them
at the origin root regardless of base") stops being true the moment the client
resolves its base from `BASE_URL`. `startsWith('/sync/')` does not match
`/alice/notes/sync/pull`.

```diff
-  // Never cache the sync/API endpoints — the Go backend serves them at the
-  // origin root regardless of base, and they must always hit the server.
+  // Never cache the sync/API endpoints — they must always hit the server. The
+  // client resolves its API base from BASE_URL, so these live under whatever
+  // prefix the app is mounted at ('/' standalone, '/<user>/<app>/' behind a
+  // muxxerr) — match anywhere in the path, not just at the root. A
+  // stale-while-revalidate /sync/pull returns rows the client has already
+  // applied and reports success, which is silent data loss.
   if (
-    url.pathname.startsWith('/sync/') ||
-    url.pathname === '/healthz' ||
-    url.pathname === '/backup'
+    url.pathname.includes('/sync/') ||
+    url.pathname.endsWith('/healthz') ||
+    url.pathname.endsWith('/backup')
   ) {
     return;
   }
```

`includes`/`endsWith` rather than a regex keeps it readable inside a nested
template literal, where a regex literal's slashes and `$` anchors are one more
escaping hazard. The generated app has no `/title`, `/dbsize` or `/push/`
endpoints — those are readerr and workoutt extensions — so the list stays at
three.

## 4.3 — Cache isolation in the generated worker

The generated worker has the same origin-wide sweep as the hand-written ones.
`emitServiceWorker.ts` emits `const CACHE_NAME = '${ctx.appName}-cache-v2';`
(emitter line 67) and, in the `activate` handler (emitter lines 225-235):

```js
self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE_NAME).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
      // Warm in the background. waitUntil keeps the worker alive for the
      // crawl; clients are already claimed, so nothing is blocked on it.
      .then(() => warmOnce())
  );
});
```

Same fix as [03-sw-cache-isolation.md](03-sw-cache-isolation.md), and it matters
more here because this worker also runs a background crawl of the whole
fingerprinted asset graph — evicting that cache is not a shell re-download, it
is a full re-crawl:

```diff
+const CACHE_PREFIX = '${ctx.appName}-cache-';
+
 self.addEventListener('activate', (event) => {
   event.waitUntil(
     caches
       .keys()
-      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE_NAME).map((k) => caches.delete(k))))
+      // Cache Storage is keyed by origin, not by worker scope. Deleting every
+      // key that isn't ours makes co-hosted apps evict each other on every
+      // activation — and this worker's background asset crawl then runs again
+      // from scratch. Only retire our own older versions.
+      .then((keys) =>
+        Promise.all(
+          keys
+            .filter((k) => k.startsWith(CACHE_PREFIX) && k !== CACHE_NAME)
+            .map((k) => caches.delete(k))
+        )
+      )
       .then(() => self.clients.claim())
       .then(() => warmOnce())
   );
 });
```

The emitter's existing note above `CACHE_NAME` ("`activate` only purges caches
whose name differs, so a constant name makes that step dead code") stays true —
the prefix filter does not change the version-bump semantics.

## 4.4 — A "mux" entry emitter

The last manual step in mounting a generated app is writing its `apps.json`
block by hand and getting `api_prefixes`, `db_file` and `idle_timeout` right
from memory. The generator knows all of it. Have it emit the fragment.

New file, `src/lib/generator/project/emitMuxEntry.ts`:

```ts
/**
 * mux.json — this app's apps.json entry for the muxxerr, ready to
 * paste into that file's "apps" array.
 *
 * Emitted as its own file rather than as a section of the README because it is
 * machine-readable: `jq '.apps += [input]' apps.json mux.json` is the whole
 * install step. Offline-only apps get a "static" entry with no backend, no
 * database and no idle timeout, because there is no process to time out.
 */
import type { EmitContext, GeneratedFile } from '../types';
import { titleCase } from '../naming';

export function emitMuxEntry(ctx: EmitContext): GeneratedFile {
  const entry = ctx.includeBackend
    ? {
        name: ctx.appName,
        title: titleCase(ctx.appName),
        description: `Generated by local-sync-template.`,
        kind: 'sync',
        source: `../${ctx.appName}`,
        backend_dir: 'backend',
        frontend_dir: 'frontend',
        base_placeholder: '/__MUX__',
        health_path: '/healthz',
        backup_path: '/backup',
        db_file: `${ctx.appName}.db`,
        // Exactly the routes backend/main.go registers. Anything not listed
        // here is treated as frontend: HTML-rewritten and cacheable.
        api_prefixes: ['/sync/', '/healthz', '/backup'],
        env: {},
        // The generated backend does no background work, so it is safe to stop
        // when idle. Cold start is ~100ms on a fresh database.
        idle_timeout: '30m',
        always_on: false,
        guarded_routes: [],
      }
    : {
        name: ctx.appName,
        title: titleCase(ctx.appName),
        description: `Generated by local-sync-template (offline-only).`,
        kind: 'static',
        source: `../${ctx.appName}`,
        frontend_dir: 'frontend',
        base_placeholder: '/__MUX__',
      };

  return { path: 'mux.json', contents: JSON.stringify(entry, null, 2) + '\n' };
}
```

Wire it into `src/lib/generator/index.ts` — the `EMITTERS` array, not
`BACKEND_EMITTERS`, since it handles both cases itself:

```diff
 import { emitDockerfile } from './project/emitDockerfile';
 import { emitReadme } from './project/emitReadme';
+import { emitMuxEntry } from './project/emitMuxEntry';
@@
   emitSettingsPage,
   emitOnboarding,
   emitReadme,
+  emitMuxEntry,
 ];
```

And add a line to the generated README (`project/emitReadme.ts`) pointing at it,
because a file nobody knows exists is not a feature.

### Why a separate file rather than editing `apps.json` directly

The generator produces a zip. It has no access to muxxerr's checkout,
does not know where it lives, and must not assume there is one. Emitting a
fragment the user pastes (or `jq`s) in keeps the generator a pure function of
its input, which is the property that makes the whole emitter design testable.

## What a generated app still needs by hand

Even with all of 4.1-4.4 applied, mounting a generated app requires:

- a build through `muxbuild`, so the frontend gets `--base=/__MUX__` — the
  generator's own `npm run build` has no base and produces a root-mounted build;
- `source` in the emitted entry corrected if the checkout is not a sibling
  directory of `apps.json`;
- nothing else. `DB_PATH`, `PORT` and `STATIC_DIR` are set by the supervisor and
  are exactly the three variables the generated `backend/main.go` reads.

See [../docs/dev/adding-an-app.md](../docs/dev/adding-an-app.md) for the full
walkthrough.
