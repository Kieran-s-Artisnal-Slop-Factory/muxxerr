# Example apps

## static-notes

A frontend-only app: four files, no backend, no sync, no build step. It exists to
demonstrate the third thing the gateway can host, alongside the two real sync
apps — and to be something to copy when you want to make a plain site available
per user.

Note the two conventions it follows:

- URLs use the sentinel base (`/__MUX__/style.css`), which the gateway rewrites
  to `/<username>/notes/style.css`. Purely relative URLs work too and need no
  sentinel at all; `muxbuild` will report `0/n` placeholder files for those and
  that is fine for a static app.
- There is no `package.json`, so `muxbuild` copies the directory verbatim
  instead of trying to run Astro over it.

To serve it, add this to `apps.json` and run `muxbuild`:

```json
{
  "name": "notes",
  "title": "Notes",
  "description": "A frontend-only demo app with no backend and no sync.",
  "kind": "static",
  "source": "examples",
  "frontend_dir": "static-notes",
  "base_placeholder": "/__MUX__",
  "api_prefixes": [],
  "guarded_routes": []
}
```

A static app never gets a child process: the gateway serves it from
`runtime/apps/notes/dist` directly, applying the same base rewriting and the
same account guard it applies to a proxied app.
