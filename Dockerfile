# muxxerr — one image holding the gateway plus every app declared in
# apps.json, already compiled and already built.
#
# The sibling images (readerr/Dockerfile, workoutt/Dockerfile) can be a `go
# build` and a `npm run build` because each one is a single app. This one
# cannot. `muxbuild` *is* the build: it reads apps.json, compiles each app's Go
# backend, builds each frontend with `--base=/__MUX__`, and then refuses to
# publish a dist in which that sentinel is missing. Reimplementing those steps
# as RUN lines would drop the refusal, and the failure it catches is invisible
# until a user opens the app in a browser and every asset 404s. So this
# Dockerfile runs muxbuild rather than paraphrasing it.
#
# WHAT ACTUALLY CROSS-COMPILES
#
# All of it — but only one part for the obvious reason, so, precisely:
#
#   * The frontends are built by node on the BUILD platform and emit HTML, CSS
#     and JS. There is no target architecture involved; the output is the same
#     bytes either way.
#   * The `mux` gateway binary is a plain `go build` with GOOS/GOARCH set. Pure
#     Go, CGO off, genuinely cross-compiled.
#   * The app backends are compiled by muxbuild, and muxbuild is a *program*,
#     so it has to run natively here. It cross-compiles them only as a side
#     effect: buildBackend (cmd/muxbuild/main.go) launches `go build` with
#     cmd.Env = os.Environ() + CGO_ENABLED=0, so a GOARCH set on the RUN below
#     is inherited by those child builds.
#
# That last one leans on an implementation detail rather than a documented
# flag, which is not something to take on trust — if it ever stops being true
# the image still builds, and then every child process dies with "exec format
# error" on an arm64 host. So the stage asserts the architecture of each
# compiled backend afterwards with `go version -m`, in the same spirit as
# muxbuild's own placeholder check: verify the artifact, do not trust the
# build.
#
# Net effect: `buildx --platform linux/amd64,linux/arm64` works without
# QEMU-emulating node or go.

# --------------------------------------------------------------- build stage
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

# git is for the submodule fallback below; nodejs/npm are what muxbuild shells
# out to for the Astro builds. The version gate is here rather than in a
# comment because Alpine's `nodejs` package tracks the base image, and a silent
# downgrade to Node 18 surfaces as an incomprehensible Vite error 40 lines into
# an unrelated build.
RUN apk add --no-cache git nodejs npm
RUN set -eu; \
    echo "go $(go version), node $(node --version), npm $(npm --version)"; \
    major=$(node -p 'process.versions.node.split(".")[0]'); \
    if [ "$major" -lt 20 ]; then \
        echo "FATAL: muxbuild needs Node 20+ for the Astro builds, got $(node --version)."; \
        echo "Alpine's nodejs package tracks the golang base image; pin a base image"; \
        echo "with a newer Alpine, or install node from the official image instead."; \
        exit 1; \
    fi

# apps.json declares its app sources as "apps/readerr" and "apps/workoutt",
# resolved by config.abs() against the directory apps.json was loaded from —
# i.e. the submodule checkouts, in place. So the repository is copied to one
# directory and apps.json is used verbatim, with no sed rewriting of source
# paths at build time. A config the image was built from that differs from the
# config in the repository is a config nobody can reason about.
WORKDIR /src

# go.mod first so a change to Go source does not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# THE SUBMODULE PROBLEM.
#
# `docker build` receives a build CONTEXT, not a git repository. The app
# sources are submodules at apps/readerr and apps/workoutt, and which of two
# things is in the context depends entirely on how the host cloned:
#
#   * cloned with --recursive (or `git submodule update --init` since) — the
#     submodules are ordinary populated directories and are already here. This
#     is the normal case, it is what CI does (docker.yaml checks out with
#     submodules: recursive), and it is the only case that builds the exact
#     commits this repository pins.
#
#   * cloned without it — apps/readerr and apps/workoutt are empty directories.
#     git leaves them as placeholders. Nothing in the context can rebuild them,
#     and `git submodule update` is not available either, because .dockerignore
#     drops .git (it is large, and none of it is a build input).
#
# So the fallback clones straight from the URLs in .gitmodules, which is the
# one piece of git metadata that IS a build input and is therefore kept. These
# are public repositories over HTTPS: no credentials, no build secrets, no
# tokens baked into a layer.
#
# Be clear about what the fallback costs. It clones the branch TIP, not the
# commit this repository pins, because resolving the pinned SHA needs the .git
# that was just excluded. It is a convenience for "I cloned this and want to
# try it", not a reproducible build. If you need reproducibility, check out the
# submodules on the host first — the line below then reports "from build
# context" for every app and nothing is fetched at all.
RUN set -eu; \
    for key in $(git config -f .gitmodules --name-only --get-regexp '^submodule\..*\.path$'); do \
        sub=${key%.path}; \
        path=$(git config -f .gitmodules --get "$sub.path"); \
        url=$(git config -f .gitmodules --get "$sub.url"); \
        branch=$(git config -f .gitmodules --get "$sub.branch" || true); \
        if [ -n "$(ls -A "$path" 2>/dev/null || true)" ]; then \
            echo "==> $path: using the checkout from the build context"; \
        elif [ -n "$branch" ]; then \
            echo "==> $path: empty in the build context, cloning $url ($branch tip)"; \
            git clone --depth 1 --branch "$branch" "$url" "$path"; \
        else \
            echo "==> $path: empty in the build context, cloning $url (default branch)"; \
            git clone --depth 1 "$url" "$path"; \
        fi; \
    done

ARG TARGETOS TARGETARCH

# Two Go builds with deliberately different targets, which is the whole trick.
# muxbuild is a build tool: it must execute in this stage, so it is compiled
# for the BUILD platform with no override. mux ships in the final image, so it
# is compiled for the TARGET.
RUN set -eu; \
    go build -trimpath -o /usr/local/bin/muxbuild ./cmd/muxbuild; \
    CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-amd64}" \
        go build -trimpath -ldflags="-s -w" -o /out/mux ./cmd/mux

# The build proper, and then the assertion the comment at the top promised.
# GOOS/GOARCH are set on muxbuild's own environment so that the `go build` it
# runs per app inherits them; npm and astro are indifferent to both.
RUN set -eu; \
    GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-amd64}" \
        muxbuild -config apps.json; \
    want="${TARGETARCH:-amd64}"; \
    for dir in runtime/apps/*/; do \
        name=$(basename "$dir"); \
        bin="$dir$name"; \
        [ -f "$bin" ] || continue; \
        got=$(go version -m "$bin" | grep -o 'GOARCH=[a-z0-9]*' | head -n1 | cut -d= -f2); \
        if [ "$got" != "$want" ]; then \
            echo "FATAL: $bin was built for '$got', not '$want'. The gateway spawns"; \
            echo "this binary at runtime, so the image would die with 'exec format"; \
            echo "error' the first time anyone opened $name. muxbuild is no longer"; \
            echo "passing GOARCH through to its child go builds — see the note at"; \
            echo "the top of this Dockerfile."; \
            exit 1; \
        fi; \
        echo "==> $bin: GOARCH=$got ok"; \
    done

# -------------------------------------------------------------- final image
#
# Not FROM scratch, and not for the usual reason. The gateway's job is to spawn
# a child process per active (user, app) pair, so the image has to be something
# those children can run in. The backends are CGO_ENABLED=0 / modernc.org/sqlite
# — genuinely static, so musl vs glibc does not arise — but scratch still has no
# /tmp, no CA bundle and no zoneinfo, and all three are load-bearing here.
FROM alpine:3.20
WORKDIR /srv

# curl        the compose healthcheck; drop it if you do not use one.
# ca-certificates  readerr's child process fetches caller-supplied URLs over
#             HTTPS for GET /title. Without a CA bundle every such fetch fails
#             with x509: certificate signed by unknown authority.
# tzdata      children inherit TZ from the gateway (see internal/supervisor).
#             workoutt builds reminder instants in local time, so a container
#             without zoneinfo silently sends every notification in UTC.
RUN apk add --no-cache curl ca-certificates tzdata

COPY --from=builder /out/mux      /usr/local/bin/mux
COPY --from=builder /src/runtime  /srv/runtime
COPY --from=builder /src/apps.json /srv/apps.json

# The data volume is /srv/data, not the /data the sibling images use, and the
# difference is deliberate.
#
# data_dir in apps.json is the relative string "data", which config.abs()
# resolves against apps.json's own directory — /srv. Getting the volume onto
# /data instead would mean rewriting data_dir to "/data" inside the image, and
# the deployment docs actively tell operators to bind-mount their own apps.json
# (it is how you set secure_cookies behind TLS). The moment they do, the
# rewrite is gone, data_dir is the repository's "data" again, the gateway
# writes mux.db and pepper.key to /srv/data — and /srv/data has no volume on
# it, so the accounts live in the container's writable layer and disappear on
# the next `docker compose pull`.
#
# Mounting the volume where the *unmodified* config already points means the
# image's config and a config copied from the repository behave identically.
VOLUME /srv/data

# PORT overrides site.addr when set. It is declared here so the value the
# healthcheck uses and the value the gateway listens on cannot drift; :8080 in
# apps.json is the same number, so the image behaves identically either way.
ENV PORT=8080 \
    TZ=UTC
EXPOSE 8080

# start-period is generous because first boot creates mux.db, runs migrations
# and starts any always_on instances before the listener is answering.
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
    CMD curl -fsS "http://localhost:${PORT}/healthz" || exit 1

# No -config: WORKDIR is /srv and apps.json sits beside the binary, so the
# default search finds it. Passing it would only be a second place to change.
CMD ["mux"]
