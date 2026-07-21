"""Drive the live SQL console the way a browser does: parse the served HTML,
submit the rendered forms, follow redirects. The console is the one place in
the gateway that writes to an app's database, so the interesting assertions are
the ones about NOT being able to reach it."""
import html.parser, http.cookiejar, json, re, sys, urllib.parse, urllib.request

BASE = "http://127.0.0.1:8099"
PHRASE = ("I understand that using this may permanently and irrevocably break "
          "my database with no way to retrieve a backup")
PASS = FAIL = 0


def ok(m):
    global PASS; PASS += 1; print(f"  PASS {m}")


def no(m, d=""):
    global FAIL; FAIL += 1; print(f"  FAIL {m} — {d}")


def check(c, m, d=""):
    ok(m) if c else no(m, d)


class Forms(html.parser.HTMLParser):
    def __init__(self):
        super().__init__(); self.forms = []; self.cur = None

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == "form":
            self.cur = {"action": a.get("action", ""), "fields": {}}
            self.forms.append(self.cur)
        elif tag in ("input", "button", "select", "textarea") and self.cur is not None:
            if a.get("name"):
                self.cur["fields"][a["name"]] = a.get("value", "")

    def handle_endtag(self, tag):
        if tag == "form":
            self.cur = None


class B:
    def __init__(self):
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.url = BASE; self.body = ""; self.status = 0

    def go(self, path, data=None):
        u = path if path.startswith("http") else BASE + path
        body = urllib.parse.urlencode(data).encode() if data else None
        req = urllib.request.Request(u, data=body, method="POST" if data else "GET")
        try:
            r = self.op.open(req)
            self.url, self.status = r.geturl(), r.status
            self.body = r.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as e:
            self.url, self.status = u, e.code
            self.body = e.read().decode("utf-8", "replace")
        return self

    def forms(self):
        p = Forms(); p.feed(self.body); return p.forms

    def form(self, action):
        return next((f for f in self.forms() if f["action"] == action), None)


def signup(b, user, pw):
    b.go("/signup")
    f = b.form("/signup")
    b.go("/signup", {**f["fields"], "username": user, "password": pw, "password_confirm": pw})
    cont = b.form("/passphrase")
    if cont:
        b.go("/passphrase", {**cont["fields"], "saved": "1"})


def install(b, app):
    b.go("/")
    f = b.form(f"/apps/{app}/install")
    if f:
        b.go(f"/apps/{app}/install", f["fields"])


print("── setup ─────────────────────────────────────────────────")
admin = B(); signup(admin, "kieran", "correct-horse-battery")
install(admin, "readerr")
admin.go("/kieran/readerr/")   # boot the instance so it is running
user = B(); signup(user, "alex", "another-long-password")
install(user, "readerr")
check(admin.status == 200, "setup complete")

print("── the Tools menu ────────────────────────────────────────")
admin.go("/")
check('href="/tools/sqlite"' in admin.body, "SQLite viewer is in the nav")
check('href="/tools/sql"' in admin.body, "SQL console is in the nav (enabled in apps.json)")
check("/tools/sqlite?app=readerr" in admin.body, "dashboard has a per-app Explore link")

print("── the console always opens locked ───────────────────────")
admin.go("/tools/sql")
check(admin.status == 200, "console page loads", admin.status)
check("Read this before you continue" in admin.body, "warning is shown")
check(PHRASE in admin.body, "the exact phrase is shown to copy")
check(admin.form("/tools/sql/execute") is None, "no execute form before unlocking")

print("── the phrase is enforced ────────────────────────────────")
f = admin.form("/tools/sql/unlock")
admin.go("/tools/sql/unlock", {**f["fields"], "phrase": "yes"})
check(admin.status == 400, "a short answer is rejected", admin.status)
check(admin.form("/tools/sql/execute") is None, "still locked")

admin.go("/tools/sql")
f = admin.form("/tools/sql/unlock")
admin.go("/tools/sql/unlock", {**f["fields"], "phrase": PHRASE.upper()})
check(admin.status == 400, "wrong capitalisation is rejected", admin.status)

admin.go("/tools/sql")
f = admin.form("/tools/sql/unlock")
admin.go("/tools/sql/unlock", {**f["fields"], "phrase": "  " + PHRASE.replace(" ", "  ") + "\n"})
check(admin.status == 200, "extra whitespace is forgiven", admin.status)
exec_form = admin.form("/tools/sql/execute")
check(exec_form is not None, "console unlocked")

print("── executing against the live database ───────────────────")
token = exec_form["fields"].get("token", "")
check(len(token) > 20, "an unlock token was issued")

admin.go("/tools/sql/execute", {**exec_form["fields"], "app": "readerr", "token": token,
                                "sql": "SELECT count(*) AS n FROM links;"})
check(admin.status == 200, "SELECT ran", admin.status)
check("Result" in admin.body, "a result section rendered")

# The instance was running; the console must have stopped it.
check("was stopped to run this" in admin.body, "the app was stopped before running")

f2 = admin.form("/tools/sql/execute")
new_token = f2["fields"].get("token", "")
check(new_token and new_token != token, "the token rotates on every execution")

print("── a write really writes ─────────────────────────────────")
admin.go("/tools/sql/execute", {**f2["fields"], "app": "readerr", "token": new_token,
                                "sql": "CREATE TABLE console_probe (id INTEGER PRIMARY KEY, note TEXT);"
                                       "INSERT INTO console_probe (note) VALUES ('written by the console');"
                                       "SELECT note FROM console_probe;"})
check("written by the console" in admin.body, "DDL + DML + SELECT all applied")

f3 = admin.form("/tools/sql/execute")
t3 = f3["fields"].get("token", "")
admin.go("/tools/sql/execute", {**f3["fields"], "app": "readerr", "token": t3,
                                "sql": "SELECT name FROM sqlite_master WHERE name='console_probe';"})
check("console_probe" in admin.body, "the change persisted to the real file")

print("── replay and expiry ─────────────────────────────────────")
admin.go("/tools/sql/execute", {**f3["fields"], "app": "readerr", "token": t3,
                                "sql": "SELECT 1;"})
check("expired" in admin.body.lower() or "Read this before" in admin.body,
      "a replayed token is refused")

print("── authorisation ─────────────────────────────────────────")
# alex is not an admin and must not reach kieran's database.
user.go("/tools/sql")
uf = user.form("/tools/sql/unlock")
user.go("/tools/sql/unlock", {**uf["fields"], "phrase": PHRASE})
uex = user.form("/tools/sql/execute")
check(uex is not None, "a non-admin can unlock their own console")
check("kieran/readerr" not in user.body, "another user's database is not offered")
ut = uex["fields"].get("token", "")
user.go("/tools/sql/execute", {**uex["fields"], "app": "kieran/readerr", "token": ut,
                               "sql": "SELECT 1;"})
check("another user" in user.body, "cross-user execution is refused", user.status)

anon = B()
anon.go("/tools/sql")
check("/login" in anon.url, "the console needs a session", anon.url)
anon.go("/tools/sqlite")
check("/login" in anon.url, "the viewer needs a session", anon.url)

print("── the viewer's plumbing ─────────────────────────────────")
admin.go("/tools/sqlite")
check(admin.status == 200, "viewer page loads", admin.status)
page = admin.body
check("sqliteviewer.js" in page, "viewer module is referenced")
check("/apps/readerr/export" in page, "the user's own snapshot source is offered")
check('type="application/json" id="sv-config"' in page, "the app list crosses as JSON")
cfg = re.search(r'id="sv-config">(.*?)</script>', page, re.S)
try:
    parsed = json.loads(cfg.group(1)) if cfg else None
    check(isinstance(parsed, dict) and parsed.get("apps"), "that JSON parses and lists apps",
          cfg.group(1)[:80] if cfg else "")
except Exception as e:
    no("that JSON parses and lists apps", str(e))

ver = re.search(r'/_mux/static/[a-f0-9]+', page).group(0)
for asset, ctype in (("sqlite/sqlite3.mjs", "javascript"), ("sqlite/sqlite3.wasm", "application/wasm"),
                     ("sqliteviewer.js", "javascript"), ("sqliteworker.js", "javascript"),
                     ("tools.css", "text/css")):
    req = urllib.request.Request(BASE + ver + "/" + asset)
    try:
        r = admin.op.open(req)
        got = r.headers.get("Content-Type", "")
        n = len(r.read())
        check(r.status == 200 and ctype in got and n > 0,
              f"{asset} serves as {ctype}", f"{r.status} {got} {n}b")
    except Exception as e:
        no(f"{asset} serves", str(e))

print()
print("══════════════════════════════════════════════════════════")
print(f"  {PASS} passed, {FAIL} failed")
sys.exit(1 if FAIL else 0)
