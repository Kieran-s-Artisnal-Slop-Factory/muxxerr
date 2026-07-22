# Deployment

Running muxxerr as a container, and putting HTTPS in front of it.

This page is about getting it up and keeping it reachable. What to do once it is
running — bootstrapping the admin, users, exports, backups, the security caveats
— is [operations.md](operations.md).

## Quick start

```bash
git clone --recursive https://github.com/Kieran-s-Artisnal-Slop-Factory/muxxerr
cd muxxerr
cp .env.example .env        # optional: port, timezone, pepper, image
docker compose up -d
```

That pulls a prebuilt image from GHCR containing the gateway plus every app in
`apps.json`, already compiled and already built. There is no build step on your
machine and no Go or Node toolchain required.

`--recursive` is not strictly required for the compose path — you are pulling an
image, not building one — but the clone is where `docker-compose.yml`,
`apps.json` and `.env.example` come from, so you need the repository either way.

If the image name in `docker-compose.yml` does not match wherever this
repository actually lives, set `MUX_IMAGE` in `.env`. GHCR names are lowercase
regardless of how the GitHub owner is capitalised, and that mismatch is the most
common reason `docker compose up` reports a manifest that does not exist.

Then open <http://localhost:8080> and **sign up**.

Three things to expect on that first visit:

1. **The first account created becomes the administrator.** There is no default
   password and no bootstrap credential printed to the log. The tradeoff is a
   race — between `up -d` and your sign-up, whoever gets there first is the
   admin — so if the machine is reachable from anywhere untrusted, create your
   account before you tell anyone the address. ([operations.md](operations.md)
   explains why it works this way.)
2. **Write down the recovery passphrase it shows you once.** There is no mail
   server here. That passphrase is the entire account-recovery story.
3. `./data` appears on the host, containing `mux.db` and `pepper.key`. Read the
   `MUX_PEPPER` section of [.env.example](../../.env.example) now rather than
   after your first restore.

Apps are opt-in per user: nothing is created at sign-up, and an instance
directory and database appear the first time you add an app from the chooser.

To check it is alive without a browser:

```bash
curl -fsS http://localhost:8080/healthz     # {"status":"ok"}
docker compose ps                           # STATUS should read (healthy)
```

### Building from source instead

```bash
git submodule update --init --recursive
docker compose -f docker-compose.build.yml up --build
```

The submodule line is optional — the Dockerfile clones any app source that
arrives empty — but without it you get each app's branch tip rather than the
commits this repository pins. The first build is slow: `npm ci` plus a full
Astro build per app, plus a Go compile per app, plus the gateway.

## Changing the port

`MUX_PORT` in `.env` is the host side of the port mapping. The container always
listens on 8080.

```ini
MUX_PORT=9000
```

```bash
docker compose up -d        # recreates the container with the new mapping
```

Do not change `PORT` in the compose file. It is the gateway's own listen port
inside the container, and the healthcheck is written against 8080.

Running without compose, both knobs the gateway itself understands still work:

```bash
mux -config apps.json -addr :9000
PORT=9000 mux -config apps.json         # PORT overrides site.addr
```

## HTTPS

The gateway does not terminate TLS and is not going to. Put a reverse proxy in
front of it.

### Which proxy

**Use Caddy.** For this shape of deployment — one Go service, one hostname, a
single small host, an operator who would rather not think about certificate
renewal — it is the obvious answer, and not only because the config is short:

- **ACME is automatic and it is the default.** No certbot cron job, no renewal
  hook that silently stopped working in March, no `--nginx` plugin rewriting
  your config. Caddy obtains and renews on its own and reloads itself.
- **Its defaults are already right for this app**, which is the part that
  actually matters here. Caddy's `reverse_proxy` preserves the original `Host`
  header, imposes no request body size limit, and applies no read timeout to
  the upstream response. Every one of those is a default the other options get
  wrong, and each of the three breaks something specific below.
- **HTTP→HTTPS redirect is implied** by writing a hostname in the site block.

When something else is the better call:

| Instead use | When |
|---|---|
| **Traefik** | You are already running several compose stacks behind one entrypoint and want new services to register themselves by label. Traefik's value is dynamic discovery across many services; with exactly one upstream it is a lot of moving parts for a certificate. |
| **nginx + certbot** | You already run nginx for other things on this host, or your organisation's runbooks assume it. It is the most familiar and the most manual — and, of the four, the one most likely to break this app out of the box, because three of its defaults are wrong here. |
| **Cloudflare Tunnel** | The host has no inbound ports and cannot get any: CGNAT, a residential ISP that blocks 443, a locked-down corporate network. It is the only option that works with zero open ports. Read the caveats below before choosing it for convenience rather than necessity. |

There is a fifth option worth naming: **do not expose it at all.** Put the host
on a Tailscale or WireGuard network and skip TLS entirely. That is the posture
this software was written for, it removes the entire attack surface this
section is about, and if it fits your situation it is a better answer than any
row in that table. (You would leave `secure_cookies: false` in that case — see
below.)

### Caddy, working config

Two files. Assume `mux.example.com` already resolves to this host and that
ports 80 and 443 reach it.

**`Caddyfile`**

```caddy
mux.example.com {
	reverse_proxy mux:8080
}
```

That is the whole thing. It obtains a certificate, redirects HTTP to HTTPS,
proxies everything, preserves `Host`, sets `X-Forwarded-For` and
`X-Forwarded-Proto`, applies no body limit and no upstream read timeout.

**Compose additions** — add to `docker-compose.yml`:

```yaml
services:
  caddy:
    image: caddy:2-alpine
    # Share the mux container's network namespace. This is not a stylistic
    # choice: it makes Caddy reach the gateway over 127.0.0.1, which is the
    # only way X-Forwarded-For is honoured. See "Client IP" below.
    network_mode: "service:mux"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data          # certificates and ACME account keys — a real
      - caddy_config:/config      # volume, or you re-issue on every restart
    depends_on:
      - mux
    restart: unless-stopped

volumes:
  caddy_data:
  caddy_config:
```

And in the `mux` service, publish 80 and 443 instead of 8080 — with
`network_mode: "service:mux"` the ports for both containers are declared on
`mux`, and the gateway itself should no longer be reachable from outside:

```yaml
  mux:
    ports:
      - "80:80"
      - "443:443"
      - "443:443/udp"    # HTTP/3
```

Then finish the job in `apps.json`, which the next section is about.

### The four things the proxy must get right

These are specific to this application. Generic "reverse proxy a Go app" advice
gets three of them wrong.

#### 1. `secure_cookies` must be flipped to true

The session cookie's `Secure` flag comes from `site.secure_cookies` in
`apps.json` and from nothing else. The gateway does not look at
`X-Forwarded-Proto`, does not sniff `r.TLS`, and has no way to know that TLS
terminated one hop upstream. Ship it as-is behind HTTPS and every session cookie
goes out without `Secure`, meaning a browser will happily send it over plain
HTTP to the same host — which is exactly what the flag exists to prevent.

```json
{
  "site": {
    "secure_cookies": true
  }
}
```

Mount your edited config over the one in the image (the line is already in
`docker-compose.yml`, commented out):

```yaml
    volumes:
      - ./data:/srv/data
      - ./apps.json:/srv/apps.json:ro
```

Keep `data_dir` as the relative string `"data"`. It resolves against the
directory `apps.json` sits in, which is `/srv`, which is where the volume is
mounted. An absolute `/data` in a mounted config sends `mux.db` and
`pepper.key` somewhere with no volume behind it, and that failure presents as
"all my accounts vanished after `docker compose pull`".

The default is `false` on purpose and should stay `false` on a LAN or a
Tailscale network: a `Secure` cookie over plain HTTP is simply not stored, and
the symptom is a login form that accepts your password and returns you to the
login form.

#### 2. Client IP, and why the proxy topology matters

`clientIP` in [internal/web/clientip.go](../../internal/web/clientip.go) reads
`X-Forwarded-For` **only from a peer you have said to believe**. Loopback is
always believed; anything else has to be named in `site.trusted_proxies`.

That is a real fork in behaviour, because the login throttle keys on two things
independently ([internal/web/auth_handlers.go](../../internal/web/auth_handlers.go)):

```go
keys := []string{"login:" + username, "login:ip:" + s.clientIP(r)}
```

Three failed attempts, then a doubling lockout to five minutes. So the topology
you pick decides whether that second key means anything:

- **Proxy on the host, gateway published to loopback** (`127.0.0.1:8080:8080`).
  Peer is `127.0.0.1`, the header is honoured with no configuration, per-IP
  throttling works as designed. The simplest correct arrangement.
- **Proxy sharing the gateway's network namespace** (`network_mode:
  "service:mux"`, as in the Caddy config above). Peer is `127.0.0.1` again,
  because it genuinely is the same network stack. Also needs no configuration,
  which is why the compose snippet uses it.
- **Proxy in a container on a shared bridge network.** Peer is the proxy's
  address on that network — `172.18.0.3` or similar. Without configuration the
  header is ignored and **every login attempt from every user collapses onto
  the single key `login:ip:172.18.0.3`**: nothing is spoofable, but three
  fat-fingered passwords from one person lock out everyone. Name the proxy and
  it behaves correctly:

  ```json
  {
    "site": {
      "trusted_proxies": ["172.18.0.0/16"]
    }
  }
  ```

  Accepts CIDR ranges or bare addresses. `X-Real-IP` is honoured from the same
  peers, for proxies that send only that.

The direction of the risk is worth being explicit about. **Only list proxies
you run.** A trusted peer's `X-Forwarded-For` is taken at face value, so naming
something you do not control — or a range wider than the proxy actually
occupies — hands whoever is behind it the ability to forge the key the throttle
counts against, and per-IP rate limiting stops meaning anything. Loopback is
trusted unconditionally because a process on the same machine can already reach
the gateway directly, so refusing to believe it would buy nothing.


#### 3. Long-running requests: the first sync

A first `/sync/pull` with `since=0` is a request for the entire database. The
apps build the whole response in memory before writing a byte, so the client
gets nothing at all — not headers, not a first chunk — until the server is
ready to send all of it. The gateway is built around this;
[internal/gateway/gateway.go](../../internal/gateway/gateway.go) says so
explicitly:

```go
// Deliberately no ResponseHeaderTimeout: a first /sync/pull with
// since=0 materialises the whole database into one response, and the
// apps build it fully in memory before writing a byte. A timeout here
// would turn a slow first sync into a hard failure.
```

`cmd/mux` makes the same choice: `ReadHeaderTimeout` 20s, `IdleTimeout` 120s,
and deliberately **no** `WriteTimeout`.

A proxy that sets a 30- or 60-second read timeout re-introduces exactly the
failure both of those comments were avoiding, and it fails on the *first* sync
of the *largest* library — the user with the most data, the one time it matters,
looking like a 504 with no explanation. Give the upstream read timeout minutes,
not seconds, or remove it.

The same applies to response buffering. A proxy that buffers the full response
before forwarding will spool a multi-hundred-megabyte body to disk, or hit a
buffer cap and error out. Turn buffering off; there is nothing to gain from it
when the upstream is a local socket.

#### 4. Request body size: `/sync/push`

The gateway caps a proxied request body at 256 MiB
(`maxAPIBody`, [internal/gateway/gateway.go](../../internal/gateway/gateway.go))
because the apps decode `/sync/push` with an unbounded `json.Decoder`. The limit
is deliberately generous — a first push of a large library is legitimate.

**nginx's default `client_max_body_size` is 1 MB.** That is not a theoretical
problem: a user's first push of any substantial library gets a 413, the app's
sync error handling reports something unhelpful, and it looks like the app is
broken rather than the proxy. Set it to at least the gateway's own limit and let
the gateway be the thing enforcing a maximum:

```nginx
client_max_body_size 256m;
```

Caddy and Traefik have no default body limit. Cloudflare's is 100 MB and is not
configurable below Enterprise — see below.

#### And one that does not apply: WebSockets

None of the apps use WebSockets. Sync is ordinary request/response over HTTP,
there is no server-sent-event stream, and the gateway does no protocol
upgrading. You do not need `Upgrade`/`Connection` header plumbing, and any
tutorial config you copy that includes it is carrying dead weight. If you add an
app that does use them, revisit this — nothing here is arranged to prevent it,
it simply is not needed today.

### nginx + certbot, sketch

If you must. Every non-obvious line here is one of the problems above.

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name mux.example.com;

    ssl_certificate     /etc/letsencrypt/live/mux.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/mux.example.com/privkey.pem;

    # The gateway caps bodies at 256 MiB itself. nginx's default is 1m, which
    # silently breaks a large first /sync/push.
    client_max_body_size 256m;

    location / {
        proxy_pass http://127.0.0.1:8080;

        # $host, NOT the default $proxy_host. The gateway's root-absolute
        # compatibility shim compares the Referer's host against r.Host to
        # decide which tenant a request like /sync/pull belongs to (see
        # internal/gateway/shim.go). Rewrite Host and the two never match, the
        # shim declines every request, and the apps' sync calls stop resolving
        # — with no error anywhere that names the cause.
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # A first /sync/pull returns an entire database and sends nothing until
        # it is ready. nginx's 60s default turns that into a 504.
        proxy_read_timeout    600s;
        proxy_send_timeout    600s;

        # Do not spool a whole database through nginx's buffers in either
        # direction. The upstream is a local socket; there is nothing to gain.
        proxy_buffering         off;
        proxy_request_buffering off;
    }
}
```

`proxy_pass` to `127.0.0.1` matters as much as any of the above: it is what
makes the gateway's peer loopback and therefore what makes
`X-Forwarded-For` count. Publish the container on `127.0.0.1:8080:8080` so
nothing else can reach it directly.

Then `certbot --nginx -d mux.example.com`, and check in ninety days that
renewal actually ran.

### Traefik, sketch

Labels on the `mux` service, with a `traefik` service holding the entrypoints
and an ACME resolver. Traefik preserves `Host` by default and has no body
limit by default, so the only thing to say explicitly is the timeout:

```yaml
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.mux.rule=Host(`mux.example.com`)"
      - "traefik.http.routers.mux.entrypoints=websecure"
      - "traefik.http.routers.mux.tls.certresolver=le"
      - "traefik.http.services.mux.loadbalancer.server.port=8080"
      # First /sync/pull sends no bytes until the whole database is built.
      - "traefik.http.services.mux.loadbalancer.responseforwarding.flushinterval=100ms"
```

and on the Traefik entrypoint itself, `--entrypoints.websecure.transport.
respondingTimeouts.readTimeout=0` so a large upload is not cut off.

Traefik in its own container on a shared network puts you squarely in the
middle case of §2 — real client IPs will not reach the gateway. Either accept
the shared throttle key or give Traefik the gateway's network namespace the way
the Caddy config does.

### Cloudflare Tunnel, and why it is a last resort here

`cloudflared` dials out, so there is nothing to open and nothing to port-forward.
That is genuinely the only reason to choose it, and when you need it, you need
it. Run `cloudflared` **on the host**, pointed at `http://127.0.0.1:8080`, so the
gateway's peer is loopback and `X-Forwarded-For` still counts.

Three things to weigh:

- **TLS terminates at a third party.** Cloudflare decrypts every request,
  including every login POST. For a personal deployment that may be an
  acceptable trade; it should be a decision, not a side effect.
- **The request body limit is 100 MB on Free and Pro** (200 MB Business, 500 MB
  Enterprise), and it is not adjustable on the lower plans. That is well below
  the gateway's own 256 MiB, so it is the effective ceiling on `/sync/push` —
  the exact failure mode described in §4, with no config knob to fix it.
- **Origin responses that send nothing for around 100 seconds are cut off with
  a 524**, raisable only on Enterprise. A first `/sync/pull` on a very large
  database is precisely a request that sends nothing for a long time.

For a small library none of this will ever fire. For a heavy user it will fire
on the one request they most need to succeed.

Sources for those two limits:
[Error 524](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-5xx-errors/error-524/),
[upload size limits](https://community.cloudflare.com/t/max-upload-size/630925).

### After you change the proxy

```bash
curl -fsSI https://mux.example.com/healthz          # 200, and check the scheme
```

Then log in through the proxy and confirm in devtools that the session cookie
has **Secure** set. If it does not, `secure_cookies` is still `false` — and the
login will appear to work, which is what makes it easy to miss.

## Backups

Two things, stored **separately**: the `data/` directory, and the pepper.

With the compose setup, `data/` is a bind mount and is right there on the host:

```
./data/mux.db                              identity
./data/pepper.key                          the key that makes mux.db useful
./data/instances/<user>/<app>/<app>.db     every user's app data
```

Stop the container, copy the directory, start it again. Do not copy a `.db`
while an instance is running — WAL mode means the main file alone can be missing
the last few minutes, or internally inconsistent.

`runtime/` is not in the volume at all; it is baked into the image and
regenerated by `muxbuild`. Do not back it up.

**If `pepper.key` is lost, every account is permanently unrecoverable.** Not
difficult — impossible. And a backup archive containing both `mux.db` and
`pepper.key` is a backup with no pepper in it, which is the whole reason
`MUX_PEPPER` exists: set it in `.env` and the key never lands in the volume at
all. Read the `MUX_PEPPER` section of [.env.example](../../.env.example) before
you set it, because changing it later does the same thing as losing it.

The full treatment — what a consistent copy actually requires, the caveat on the
apps' `/backup` endpoint, and how to verify a restore before you need one — is
[operations.md § Backups](operations.md#backups). It is not repeated here.

## Upgrading

```bash
docker compose pull
docker compose up -d
```

The image carries the apps, so pulling a new image is also how you get new app
versions. Running children are not hot-swapped — they are replaced when the
container restarts, which `up -d` does.

`./data` is untouched by an upgrade. `signups_enabled` and other runtime
settings live in `mux.db`, not in `apps.json`, so they survive too — see
[operations.md § Signups](operations.md#signups) for why editing `apps.json`
will not change them after first boot.
