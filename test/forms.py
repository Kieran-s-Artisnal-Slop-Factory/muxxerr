"""Drive muxerr the way a browser would: parse the HTML it actually
serves, submit the forms it actually renders, follow the redirects it actually
sends. The curl suite posts to URLs I chose; this one posts to the URLs the
pages contain, which is the only way to catch a form pointing at a route that
does not exist."""
import html.parser, http.cookiejar, re, sys, urllib.parse, urllib.request

BASE = "http://127.0.0.1:8099"
PASS = FAIL = 0

def ok(msg):
    global PASS; PASS += 1; print(f"  PASS {msg}")

def no(msg, detail=""):
    global FAIL; FAIL += 1; print(f"  FAIL {msg} — {detail}")

def check(cond, msg, detail=""):
    ok(msg) if cond else no(msg, detail)


class Forms(html.parser.HTMLParser):
    """Collect every <form> with its method, action and input/button values."""
    def __init__(self):
        super().__init__(); self.forms = []; self.cur = None

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == "form":
            self.cur = {"action": a.get("action", ""), "method": a.get("method", "get").lower(), "fields": {}}
            self.forms.append(self.cur)
        elif tag in ("input", "button", "select", "textarea") and self.cur is not None:
            name = a.get("name")
            if not name:
                return
            if a.get("type") == "checkbox":
                self.cur["fields"][name] = a.get("value", "on")
            else:
                self.cur["fields"][name] = a.get("value", "")

    def handle_endtag(self, tag):
        if tag == "form":
            self.cur = None


class Browser:
    def __init__(self):
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.last_url = BASE

    def get(self, path):
        url = path if path.startswith("http") else BASE + path
        try:
            r = self.op.open(url)
        except urllib.error.HTTPError as e:
            self.last_url = url; self.body = e.read().decode("utf-8", "replace"); self.status = e.code
            return self
        self.last_url = r.geturl(); self.body = r.read().decode("utf-8", "replace"); self.status = r.status
        return self

    def forms(self):
        p = Forms(); p.feed(self.body); return p.forms

    def form(self, match):
        """Find the form whose action or field set mentions `match`."""
        for f in self.forms():
            if match in f["action"] or match in " ".join(f["fields"]):
                return f
        return None

    def submit(self, form, **overrides):
        fields = dict(form["fields"]); fields.update(overrides)
        action = urllib.parse.urljoin(self.last_url, form["action"])
        data = urllib.parse.urlencode(fields).encode()
        try:
            r = self.op.open(urllib.request.Request(action, data=data, method="POST"))
        except urllib.error.HTTPError as e:
            self.last_url = action; self.body = e.read().decode("utf-8", "replace"); self.status = e.code
            return self
        self.last_url = r.geturl(); self.body = r.read().decode("utf-8", "replace"); self.status = r.status
        return self


print("── signup, entirely through rendered forms ───────────────")
b = Browser()
b.get("/signup")
f = b.form("username")
check(f is not None and f["action"] == "/signup", "signup form found", f and f["action"])
b.submit(f, username="kieran", password="correct-horse-battery", password_confirm="correct-horse-battery")
check(b.status == 200, "signup submitted", b.status)
m = re.search(r"\b([a-z]+(?:-[a-z]+){5})\b", b.body)
check(m is not None, "recovery passphrase rendered")
PP = m.group(1) if m else ""

print("── the post-signup Continue button actually works ────────")
cont = b.form("saved")
check(cont is not None and cont["action"] == "/passphrase", "continue form found", cont and cont["action"])
b.submit(cont, saved="1")
check(b.status == 200, "continue accepted (not a 404)", b.status)
check(b.last_url.rstrip("/") == BASE, "landed on the dashboard", b.last_url)

print("── the Add button actually provisions an app ─────────────")
b.get("/")
add = next((f for f in b.forms() if "install" in f["action"]), None)
check(add is not None, "add form found", [f['action'] for f in b.forms()])
if add:
    check("readerr" in add["action"] or "workoutt" in add["action"], "add form targets an app", add["action"])
    b.submit(add)
    check(b.status == 200, "add submitted", b.status)
    check("/kieran/" in b.last_url, "redirected into the app", b.last_url)

b.get("/")
check("Open" in b.body, "installed app now shows an Open control")
rm = next((f for f in b.forms() if "remove" in f["action"]), None)
check(rm is not None, "remove form present on the card")

print("── account actions, through their rendered forms ─────────")
b.get("/account")
acts = {f["action"] for f in b.forms() if f["method"] == "post"}
for want in ("/account/password", "/account/passphrase", "/account/sessions/revoke"):
    check(want in acts, f"account page posts to {want}", acts)

pw = next((f for f in b.forms() if f["action"] == "/account/password"), None)
check(pw is not None, "password form found")
if pw:
    check(set(pw["fields"]) >= {"current_password", "password", "password_confirm"},
          "password form carries the fields the handler reads", set(pw["fields"]))
    b.submit(pw, current_password="correct-horse-battery",
             password="a-second-good-password", password_confirm="a-second-good-password")
    check(b.status == 200 and "/account" in b.last_url, "password change accepted", (b.status, b.last_url))
    check("signed out" in b.body.lower() or "changed" in b.body.lower(), "confirmation shown")

b.get("/account")
rot = next((f for f in b.forms() if f["action"] == "/account/passphrase"), None)
if rot:
    b.submit(rot, current_password="a-second-good-password")
    check(b.status == 200, "passphrase rotation accepted", b.status)
    m2 = re.search(r"\b([a-z]+(?:-[a-z]+){5})\b", b.body)
    check(m2 is not None and m2.group(1) != PP, "a NEW passphrase was issued")

print("── the old password no longer works, the new one does ────")
c = Browser(); c.get("/login")
lf = c.form("password")
c.submit(lf, username="kieran", password="correct-horse-battery")
check("do not match" in c.body, "old password rejected")
c = Browser(); c.get("/login")
c.submit(c.form("password"), username="kieran", password="a-second-good-password")
check(c.last_url.rstrip("/") == BASE, "new password signs in", c.last_url)

print("── admin page controls resolve ───────────────────────────")
c.get("/admin")
check(c.status == 200, "admin page loads", c.status)
admin_actions = {f["action"] for f in c.forms() if f["method"] == "post"}
check(any("/admin/settings/signups" == a for a in admin_actions), "signups toggle present", admin_actions)
sig = next((f for f in c.forms() if f["action"] == "/admin/settings/signups"), None)
if sig:
    c.submit(sig)
    check(c.status == 200 and "/admin" in c.last_url, "signups toggle works", (c.status, c.last_url))

print()
print("══════════════════════════════════════════════════════════")
print(f"  {PASS} passed, {FAIL} failed")
sys.exit(1 if FAIL else 0)
