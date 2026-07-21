# 03 — Stop the apps deleting each other's caches

**Applies to:** workoutt and readerr
**Status:** not applied
**Independent** of patches 01 and 02. Nothing in the gateway can substitute for
this one: Cache Storage lives in the browser, where the server has no reach.

## The problem

Cache Storage is keyed by **origin**, not by scope. Under muxerr both
apps share `http://localhost:8080`, so they share one cache namespace — and
both service workers were written on the assumption that they own it.

`frontend/public/sw.js`, lines 83-90, identical in both apps:

```js
self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE_VERSION).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});
```

`keys.filter((k) => k !== CACHE_VERSION)` means *"delete every cache on this
origin that is not mine"*. The cache names are `readerr-v2` (readerr sw.js line
18) and `workoutt-v5` (workoutt sw.js line 18). So:

- readerr's worker activates → deletes `workoutt-v5`
- you open workoutt → its worker activates → deletes `readerr-v2`
- back to readerr → shell re-downloaded, whole asset cache cold

This is a correctness-preserving bug: nothing breaks, the apps just permanently
thrash their caches, and neither is reliably usable offline while the other is
installed. It is also invisible in every environment the apps were developed in,
because there each app had the origin to itself.

The version suffix is load-bearing and must stay: bumping `readerr-v2` to
`readerr-v3` is how a breaking change invalidates the old cache. The filter has
to keep doing that job while ignoring caches that belong to someone else.

## The fix

Delete only keys that carry this app's own prefix. Same version-bump behaviour,
scoped to one app's namespace.

`frontend/public/sw.js` — **readerr**, replacing lines 83-90:

```diff
+// Everything this app is allowed to delete. Cache Storage is keyed by origin,
+// not by service-worker scope, so under a muxerr (or any deployment that
+// puts two of these apps on one host) a bare "delete every key that isn't
+// mine" makes the two apps evict each other on every activation. Deleting only
+// our own prefix keeps the version bump working — CACHE_VERSION is
+// `<CACHE_PREFIX>vN` — without touching the neighbours.
+const CACHE_PREFIX = 'readerr-';
+
 self.addEventListener('activate', (event) => {
   event.waitUntil(
     caches
       .keys()
-      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE_VERSION).map((k) => caches.delete(k))))
+      .then((keys) =>
+        Promise.all(
+          keys
+            .filter((k) => k.startsWith(CACHE_PREFIX) && k !== CACHE_VERSION)
+            .map((k) => caches.delete(k))
+        )
+      )
       .then(() => self.clients.claim())
   );
 });
```

**workoutt** — identical, with `const CACHE_PREFIX = 'workoutt-';`.

Both files already declare `const CACHE_VERSION = 'readerr-v2'` / `'workoutt-v5'`
at line 18. You can derive the prefix there instead if you prefer one fewer
constant (`const CACHE_PREFIX = CACHE_VERSION.replace(/v\d+$/, '')`), but the
literal is clearer and the regex is one more thing that can be wrong when
someone renames a cache.

### One instance per user, not per user *and* app

Worth being explicit: this isolates *apps* from each other, not *users*. Two
users of the same app on the same browser profile still share `readerr-v2`.
That is not a new problem introduced by muxerr — it is the general
"shared browser" caveat covered in
[docs/admin/operations.md](../docs/admin/operations.md), and it is why the
gateway sets `Cache-Control: no-store` on API responses and why signing out
does not make a shared machine safe. Cache Storage holds only the static shell
and fingerprinted assets, which are identical for every user, so there is no
data leak here — just a shared cache of public files.

## The same bug in the dev branch of Layout.astro

`frontend/src/layouts/Layout.astro` — readerr **line 108**, workoutt **line 98**,
inside the non-production `<script is:inline>` that tears down a leftover worker:

```js
// Dev: the service worker must NOT run — it caches Vite's module
// URLs and serves stale code after edits (blank pages). Unregister
// any leftover registration and drop its caches.
if ('serviceWorker' in navigator) {
  navigator.serviceWorker.getRegistrations().then((regs) => {
    for (const reg of regs) reg.unregister();
  });
  if (window.caches) {
    caches.keys().then((keys) => keys.forEach((k) => caches.delete(k)));
  }
}
```

`caches.delete(k)` for every `k` — this one does not even spare its own. In dev
that is the intent (nuke everything, the worker must not run), and if your dev
server is a dedicated port serving one app it is harmless. It stops being
harmless the moment you run a dev build on the same origin as anything else, or
point a dev frontend at muxerr's port.

Same shape of fix, readerr shown:

```diff
   if (window.caches) {
-    caches.keys().then((keys) => keys.forEach((k) => caches.delete(k)));
+    // Only ours: this page may share an origin with other apps (the
+    // muxerr serves several under one host), and dropping their caches
+    // is not this script's business.
+    caches.keys().then((keys) => keys.filter((k) => k.startsWith('readerr-')).forEach((k) => caches.delete(k)));
   }
```

workoutt: `k.startsWith('workoutt-')`.

`reg.unregister()` above it has the same over-reach in principle —
`getRegistrations()` returns every registration the page can see — but in
practice a page under `/alice/readerr/` only sees registrations at or below its
own scope, so it cannot reach `/alice/workoutt/`'s worker. Left alone.

## Verify

1. Apply to both apps and rebuild both through `muxbuild`.
2. Open `/alice/readerr/`, let the worker install, then open `/alice/workoutt/`
   and let its worker install.
3. Application → Cache Storage. **Both** `readerr-v2` and `workoutt-v5` must be
   listed. Before the patch you see exactly one, whichever activated last.
4. Go offline and switch between the two apps. Both shells must load.
5. Regression check on the version bump: change `CACHE_VERSION` to
   `'readerr-v3'`, reload, and confirm `readerr-v2` is gone and `workoutt-v5`
   survived.
