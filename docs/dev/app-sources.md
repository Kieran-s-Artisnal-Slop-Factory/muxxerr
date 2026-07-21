# Where apps come from

Each entry in `apps.json` has a `source`, and it can be one of three things.
They differ in one respect that matters more than convenience: **who decides
which version gets built.**

```jsonc
"source": "apps/readerr"                              // submodule  — this repo pins it
"source": "../readerr"                                // local path — you do, right now
"source": "git+https://github.com/you/readerr"        // remote     — the remote's tip does
"source": "git+https://github.com/you/readerr#v1.2.0" // remote     — pinned to a ref
```

## Submodules (the default, and the recommended one)

The two apps ship as submodules under `apps/`:

| Path | Repository | Branch |
|---|---|---|
| `apps/readerr` | `Kieran-s-Artisnal-Slop-Factory/readerr` | `main` |
| `apps/workoutt` | `Kieran-s-Artisnal-Slop-Factory/workoutt` | `master` |

A submodule records an **exact commit** in this repository's history. Two people
who check out the same commit of muxerr build byte-identical apps, and
so does CI, and so does a rebuild six months from now. That is the whole
argument for it.

### Cloning

```bash
git clone --recurse-submodules https://github.com/you/muxerr
```

Already cloned without it? The apps directories will be empty and `muxbuild`
will tell you the backend source does not exist. Fix it with:

```bash
git submodule update --init --recursive
```

### Updating an app to the latest upstream

```bash
# Everything, to the tip of each submodule's configured branch:
git submodule update --remote --merge

# Or just one:
git submodule update --remote --merge apps/readerr
```

Then **commit the result**. This is the step people miss: `--remote` moves the
submodule's checkout, and until you commit, the new commit id is only in your
working tree.

```bash
git add apps/readerr apps/workoutt
git commit -m "Update readerr and workoutt"
go run ./cmd/muxbuild      # rebuild against the new sources
```

`git submodule status` shows what you have; a leading `+` means the checkout has
moved away from the commit this repo records, which is exactly what you see
after `--remote` and before `git commit`.

### Working on an app

Submodules are real checkouts, so you can work in them directly — but a fresh
submodule is in **detached HEAD**, and committing there loses your work when it
next moves. Get on a branch first:

```bash
cd apps/readerr
git checkout main
# ...edit, commit, push as normal...
cd ../..
git add apps/readerr && git commit -m "Bump readerr"
```

If you would rather work in a checkout you already have somewhere else, point
`source` at it — see below — instead of fighting the submodule.

## A local path

```jsonc
"source": "../readerr"
```

Relative to `apps.json`. This is the right answer while you are actively
changing an app: no submodule bookkeeping, no commit dance, `muxbuild` just
builds whatever is in that directory right now.

It is the wrong answer for anything you deploy, because the built artifact
depends on the state of a directory that is not in this repository's history.

## A `git+` URL

```jsonc
"source": "git+https://github.com/you/readerr"        // default branch
"source": "git+https://github.com/you/readerr#main"   // a branch
"source": "git+https://github.com/you/readerr#v1.2.0" // a tag
"source": "git+https://github.com/you/readerr#a1b2c3d"// a commit
```

`muxbuild` clones this into `runtime/sources/<app>/` on first build and updates
it on every build after. The commit it landed on is printed in the build log.

Useful when you want a build server to always take the latest without anyone
committing a submodule bump, or when the app is not something you want in this
repo's history at all.

The tradeoff to be aware of: with no `#ref`, **two builds a week apart can
produce different software from identical inputs**. That is fine for a personal
server that always wants the newest thing, and it is a bad property to discover
while trying to work out which build broke something. Pin a tag if you care.

Notes:

- The checkout under `runtime/sources/` is a **build input, not a working
  copy.** Every build does `git reset --hard` and `git clean -fdx` on it
  (`node_modules` is preserved, because reinstalling it every build is minutes
  for nothing). Edits there are discarded without warning.
- Clones are shallow (`--depth 1`).
- `git` is only required when at least one app actually uses a `git+` source;
  submodules and local paths need nothing.
- A URL containing credentials is **rejected at config-load time**. They would
  end up in build logs and error messages. Use a public URL, or an `ssh://`
  remote with a key the build agent already has.

## Mixing them

Per-app, freely. A common shape while working on one app:

```jsonc
{ "name": "readerr",  "source": "../readerr" },        // the one you're editing
{ "name": "workoutt", "source": "apps/workoutt" }      // everything else pinned
```

Nothing downstream of `config.SourceDir` knows or cares which form was used —
by the time anything is built, all three are just a directory on disk.
