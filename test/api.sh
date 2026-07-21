#!/bin/bash
# End-to-end journey against a freshly-initialised muxerr.
# MSYS_NO_PATHCONV stops Git Bash rewriting leading-slash values into
# Windows paths before curl ever sees them.
export MSYS_NO_PATHCONV=1
B=${B:-http://127.0.0.1:8099}
cd "$(dirname "$0")"
mkdir -p .work && cd .work
pass=0; fail=0
ok()  { pass=$((pass+1)); printf "  \033[32mPASS\033[0m %s\n" "$1"; }
no()  { fail=$((fail+1)); printf "  \033[31mFAIL\033[0m %s — %s\n" "$1" "$2"; }
is()  { if [ "$2" = "$3" ]; then ok "$1"; else no "$1" "got '$2' want '$3'"; fi; }
has() { if echo "$2" | grep -q "$3"; then ok "$1"; else no "$1" "missing '$3'"; fi; }
hasnt(){ if echo "$2" | grep -q "$3"; then no "$1" "unexpectedly contains '$3'"; else ok "$1"; fi; }

csrf() { grep mux_csrf "$1" | awk '{print $7}'; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
loc()  { curl -s -o /dev/null -w '%{redirect_url}' "$@"; }

signup() { # jar user pass -> echoes passphrase
  rm -f "$1"; curl -s -c "$1" $B/signup >/dev/null
  curl -s -c "$1" -b "$1" -X POST $B/signup \
    --data-urlencode "csrf_token=$(csrf $1)" --data-urlencode "username=$2" \
    --data-urlencode "password=$3" --data-urlencode "password_confirm=$3" -o ./su.html
  grep -oE '[a-z]+(-[a-z]+){5}' ./su.html | head -1
}
add() { curl -s -c "$1" -b "$1" -X POST "$B/apps/$2/install" \
    --data-urlencode "csrf_token=$(csrf $1)" -o /dev/null -w '%{http_code}'; }
row() { echo "{\"rows\":{\"links\":[{\"id\":\"$1\",\"url\":\"https://ex.com/$1\",\"title\":\"$2\",\"added_at\":\"2026-07-20T10:00:00Z\",\"updated_at\":\"2026-07-20T10:00:00Z\"}]}}"; }

echo "── identity ──────────────────────────────────────────────"
PP1=$(signup j1.txt kieran "correct-horse-battery")
has "first signup issues a 6-word passphrase" "$PP1" '^[a-z]*-[a-z]*-[a-z]*-[a-z]*-[a-z]*-[a-z]*$'
is  "first user became admin" "$(code -b j1.txt $B/admin)" "200"
PP2=$(signup j2.txt alex "another-long-password")
is  "second user is not admin" "$(code -b j2.txt $B/admin)" "403"
is  "short password rejected" "$(rm -f jz.txt; curl -s -c jz.txt $B/signup>/dev/null; curl -s -c jz.txt -b jz.txt -X POST $B/signup --data-urlencode "csrf_token=$(csrf jz.txt)" --data-urlencode 'username=bob' --data-urlencode 'password=short' --data-urlencode 'password_confirm=short' -o /dev/null -w '%{http_code}')" "400"
is  "reserved username rejected" "$(rm -f jz.txt; curl -s -c jz.txt $B/signup>/dev/null; curl -s -c jz.txt -b jz.txt -X POST $B/signup --data-urlencode "csrf_token=$(csrf jz.txt)" --data-urlencode 'username=admin' --data-urlencode 'password=a-long-enough-password' --data-urlencode 'password_confirm=a-long-enough-password' -o /dev/null -w '%{http_code}')" "400"
is  "bad CSRF refused" "$(code -b j1.txt -X POST $B/apps/readerr/install --data-urlencode 'csrf_token=nope')" "403"

echo "── provisioning and the app itself ───────────────────────"
is  "kieran adds readerr" "$(add j1.txt readerr)" "303"
is  "kieran adds workoutt" "$(add j1.txt workoutt)" "303"
is  "alex adds readerr" "$(add j2.txt readerr)" "303"
APP=$(curl -s -b j1.txt -H 'Sec-Fetch-Mode: navigate' $B/kieran/readerr/)
hasnt "sentinel base fully rewritten" "$APP" '__MUX__'
has "links carry the tenant prefix" "$APP" 'href="/kieran/readerr/'
has "owner guard injected" "$APP" '__mux_owner'
has "guard names the right owner" "$APP" 'OWNER="kieran",APP="readerr"'
is  "asset serves" "$(code -b j1.txt $B/kieran/readerr/_astro/Layout.C7_vgLkn.css)" "200"
is  "service worker serves" "$(code -b j1.txt $B/kieran/readerr/sw.js)" "200"
is  "manifest serves" "$(code -b j1.txt $B/kieran/readerr/manifest.webmanifest)" "200"
is  "inner page serves" "$(code -b j1.txt $B/kieran/readerr/settings/)" "200"
is  "workoutt serves too" "$(code -b j1.txt $B/kieran/workoutt/)" "200"
is  "bare app path redirects to slash" "$(code -b j1.txt $B/kieran/readerr)" "301"
is  "unknown app is 404" "$(code -b j1.txt $B/kieran/nosuchapp/)" "404"

echo "── sync, both the prefixed and the compatibility path ────"
has "prefixed push accepted" "$(curl -s -b j1.txt -X POST $B/kieran/readerr/sync/push -H 'Content-Type: application/json' -d "$(row k1 'kieran-prefixed')")" 'server_seq'
has "root push via Referer shim accepted" "$(curl -s -b j1.txt -X POST $B/sync/push -H 'Content-Type: application/json' -H "Referer: $B/kieran/readerr/settings/" -d "$(row k2 'kieran-shim')")" 'server_seq'
PULL=$(curl -s -b j1.txt "$B/kieran/readerr/sync/pull?since=0")
has "prefixed row present" "$PULL" 'kieran-prefixed'
has "shim row present (same instance)" "$PULL" 'kieran-shim'
is  "shim without a referer refuses" "$(code -b j1.txt -X POST $B/sync/push -H 'Content-Type: application/json' -d '{"rows":{}}')" "404"

echo "── tenant isolation ──────────────────────────────────────"
curl -s -b j2.txt -X POST $B/alex/readerr/sync/push -H 'Content-Type: application/json' -d "$(row a1 'ALEX-ONLY')" >/dev/null
APULL=$(curl -s -b j2.txt "$B/alex/readerr/sync/pull?since=0")
has  "alex sees his own row" "$APULL" 'ALEX-ONLY'
hasnt "alex cannot see kieran's data" "$APULL" 'kieran-'
is  "alex blocked from kieran's app" "$(code -b j2.txt $B/kieran/readerr/)" "403"
is  "alex blocked from kieran's API" "$(code -b j2.txt "$B/kieran/readerr/sync/pull?since=0")" "403"
is  "shim cannot cross tenants" "$(code -b j2.txt -H "Referer: $B/kieran/readerr/" "$B/sync/pull?since=0")" "403"

echo "── unauthenticated behaviour ─────────────────────────────"
is  "navigation is redirected to login" "$(code -H 'Sec-Fetch-Mode: navigate' $B/kieran/readerr/)" "303"
is  "fetch gets 401, not an HTML redirect" "$(code -H 'Sec-Fetch-Mode: cors' "$B/kieran/readerr/sync/pull?since=0")" "401"
has "401 body is JSON the client can read" "$(curl -s -H 'Sec-Fetch-Mode: cors' "$B/kieran/readerr/sync/pull?since=0")" '"error":"unauthenticated"'
has "login URL carries next" "$(curl -s -D - -o /dev/null -H 'Sec-Fetch-Mode: navigate' $B/kieran/readerr/settings/ | grep -i '^location')" 'next=%2Fkieran%2Freaderr%2Fsettings%2F'

echo "── deep-link login round trip ────────────────────────────"
relogin() { rm -f jr.txt; curl -s -c jr.txt $B/login >/dev/null
  curl -s -c jr.txt -b jr.txt -X POST $B/login --data-urlencode "csrf_token=$(csrf jr.txt)" \
    --data-urlencode "username=$1" --data-urlencode "password=$2" --data-urlencode "next=$3" \
    -o /dev/null -w '%{redirect_url}'; }
is "gated at an app URL, login returns there" "$(relogin kieran correct-horse-battery /kieran/readerr/settings/)" "$B/kieran/readerr/settings/"
is "wrong user is not sent into that namespace" "$(relogin alex another-long-password /kieran/readerr/)" "$B/"
is "external next refused" "$(relogin kieran correct-horse-battery https://evil.example/)" "$B/"
is "protocol-relative next refused" "$(relogin kieran correct-horse-battery //evil.example/)" "$B/"
is "no next lands on the chooser" "$(relogin kieran correct-horse-battery '')" "$B/"

echo "── SSRF guard on readerr's /title ────────────────────────"
for u in "http://169.254.169.254/" "http://192.168.1.1/" "http://10.0.0.5/" "http://127.0.0.1:8099/admin" "http://localhost/" "file:///etc/passwd" "http://100.64.0.1/"; do
  enc=$(python -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1],safe=''))" "$u")
  is "blocked $u" "$(code -b j1.txt "$B/kieran/readerr/title?url=$enc")" "403"
done
is "blocked through the shim too" "$(code -b j1.txt -H "Referer: $B/kieran/readerr/" "$B/title?url=http%3A%2F%2F169.254.169.254%2F")" "403"

echo "── export ────────────────────────────────────────────────"
curl -s -b j1.txt "$B/admin/instances/alex/readerr/export" -o ex.db -w ''
is "admin export is a real sqlite file" "$(head -c 15 ex.db)" "SQLite format 3"
is "admin export holds only alex's data" "$(python -c "
import sqlite3;c=sqlite3.connect('ex.db');print(','.join(r[0] for r in c.execute('select title from links')))")" "ALEX-ONLY"
is "self-export works" "$(code -b j2.txt $B/apps/readerr/export)" "200"
is "non-admin cannot export another user" "$(code -b j2.txt $B/admin/instances/kieran/readerr/export)" "403"

echo "── PWA assets are readable without a session ─────────────"
# A browser fetches <link rel="manifest"> with credentials OMITTED, so these
# must answer anonymously or every app is permanently un-installable.
for f in manifest.webmanifest icon.svg favicon.svg favicon.ico icon-maskable.svg; do
  is "anonymous $f" "$(code $B/kieran/readerr/$f)" "200"
done
is "manifest declares the right type" "$(curl -s -o /dev/null -w '%{content_type}' $B/kieran/readerr/manifest.webmanifest)" "application/manifest+json"
is "the app shell is still behind auth" "$(code -H 'Sec-Fetch-Mode: navigate' $B/kieran/readerr/)" "303"
is "the API is still behind auth" "$(code -H 'Sec-Fetch-Mode: cors' "$B/kieran/readerr/sync/pull?since=0")" "401"
# net/http normalises the path and redirects; what matters is where you end
# up, which must not be a file outside the app's dist directory.
is "no traversal through the public path" "$(curl -s -L -o /dev/null -w '%{http_code}' --path-as-is "$B/kieran/readerr/../../../go.mod")" "404"

echo "── dashboard metadata ────────────────────────────────────"
DASH=$(curl -s -b j1.txt $B/)
has "app icon rendered" "$DASH" 'app-card__icon'
has "added date rendered" "$DASH" '<dt>Added</dt>'
has "database size rendered" "$DASH" '<dt>Database</dt>'
has "logs link rendered" "$DASH" '/apps/readerr/logs'

echo "── log viewer ────────────────────────────────────────────"
is "owner can read their own logs" "$(code -b j1.txt $B/apps/readerr/logs)" "200"
has "log lines rendered" "$(curl -s -b j1.txt $B/apps/readerr/logs)" 'class="logline'
has "structured level extracted" "$(curl -s -b j1.txt $B/apps/readerr/logs)" 'logline__level'
is "a static app has no log page" "$(code -b j1.txt $B/apps/nosuchapp/logs)" "404"
is "another user cannot read them" "$(code -b j2.txt $B/admin/instances/kieran/readerr/logs)" "403"
is "an admin can" "$(code -b j1.txt $B/admin/instances/alex/readerr/logs)" "200"
is "logs need a session" "$(code $B/apps/readerr/logs)" "303"

echo "── static assets are content-addressed ───────────────────"
VER=$(curl -s $B/login | grep -oE '/_mux/static/[a-f0-9]+/mux.css' | head -1)
has "stylesheet URL carries a version" "$VER" '/_mux/static/[a-f0-9]'
has "versioned assets are immutable" "$(curl -s -D - -o /dev/null $B$VER | grep -i cache-control)" 'immutable'

echo
echo "══════════════════════════════════════════════════════════"
printf "  %d passed, %d failed\n" "$pass" "$fail"
[ "$fail" -eq 0 ]
