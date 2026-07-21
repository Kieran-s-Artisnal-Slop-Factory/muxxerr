// Base rewriting: how one build of an app serves every user.
//
// The frontends are built once with `astro build --base=/__MUX__`, so every
// internal URL they emit — page links, hashed asset paths, the manifest, the
// service worker's own location — contains that sentinel. On the way out the
// gateway replaces it with the real mount prefix for whoever is asking, so
// /__MUX__/_astro/app.js becomes /kieran/readerr/_astro/app.js for Kieran and
// /alex/readerr/_astro/app.js for Alex, from the same bytes on disk.
//
// This is cheap in practice: of readerr's 275 built files only 21 contain the
// sentinel at all (the ~250 hashed chunks reference each other relatively),
// and those are the small ones. The alternative — one Astro build per (user,
// app) — costs a full minute of node and ~5 MB per pair, and would have to be
// re-run whenever a user is added.
//
// API responses are deliberately NOT rewritten. They are the user's own data,
// and a note that happened to contain the sentinel string must not be quietly
// edited in transit.
package gateway

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// maxRewriteBody bounds what we will buffer to rewrite. Everything that
// legitimately carries the sentinel is HTML, JS, CSS or a manifest — all
// small. Anything larger is passed through untouched rather than pulled into
// memory.
const maxRewriteBody = 16 << 20 // 16 MiB

// rewritableType reports whether a Content-Type is text we may safely
// string-replace inside. Binary types are excluded not for correctness (the
// sentinel is unlikely to appear) but because rewriting them means buffering
// them, and images and fonts are the large files.
func rewritableType(ct string) bool {
	ct = strings.ToLower(ct)
	for _, p := range []string{
		"text/html", "text/css", "text/plain",
		"javascript", "application/json", "manifest+json",
		"image/svg+xml", "application/xml", "text/xml",
	} {
		if strings.Contains(ct, p) {
			return true
		}
	}
	return false
}

func (g *Gateway) modifyResponse(res *http.Response) error {
	rc := requestCtx(res.Request.Context())
	if rc.app == nil {
		return nil
	}

	// A redirect from the child is expressed in its own root-relative terms —
	// http.FileServer's /settings -> /settings/ being the common case. Left
	// alone it would throw the browser out of the app's namespace entirely.
	rewriteLocation(res, rc.prefix)
	rewriteCookiePaths(res, rc.prefix)

	// The apps set Access-Control-Allow-Origin: * on everything. Behind a
	// single-origin gateway that header is at best meaningless and at worst
	// an invitation; and if the gateway ever adds its own, a browser seeing
	// two of them rejects the response outright.
	res.Header.Del("Access-Control-Allow-Origin")
	res.Header.Del("Access-Control-Allow-Methods")
	res.Header.Del("Access-Control-Allow-Headers")

	if rc.isAPI {
		// Never let the service worker or an intermediate cache hold on to
		// sync traffic: a stale /sync/pull silently desynchronises the client.
		res.Header.Set("Cache-Control", "no-store")
		if res.Header.Get("Content-Encoding") != "" {
			res.Header.Add("Vary", "Accept-Encoding")
		}
		return nil
	}

	if res.StatusCode == http.StatusNotModified || res.StatusCode == http.StatusNoContent {
		return nil
	}
	// A HEAD response carries the headers of the body that a GET would return,
	// but no body. Rewriting would read zero bytes and then confidently
	// announce Content-Length: 0, which is a lie about the resource.
	if res.Request != nil && res.Request.Method == http.MethodHead {
		return nil
	}
	if !rewritableType(res.Header.Get("Content-Type")) {
		return nil
	}
	if res.ContentLength > maxRewriteBody {
		slog.Warn("response too large to rewrite; serving as-is",
			"app", rc.app.Name, "path", rc.upstreamPath, "bytes", res.ContentLength)
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxRewriteBody+1))
	res.Body.Close()
	if err != nil {
		return err
	}
	if len(body) > maxRewriteBody {
		return fmt.Errorf("response body exceeds %d bytes", maxRewriteBody)
	}

	body = rewriteBody(body, rc, res.Header.Get("Content-Type"))
	setBody(res, body, rc.acceptsGzip)
	return nil
}

// rewriteBody applies the sentinel substitution and, for HTML, injects the
// account guard.
func rewriteBody(body []byte, rc *reqCtx, contentType string) []byte {
	if ph := rc.app.BasePlaceholder; ph != "" {
		body = bytes.ReplaceAll(body, []byte(ph), []byte(rc.prefix))
	}
	if strings.Contains(strings.ToLower(contentType), "text/html") {
		body = injectGuard(body, rc)
	}
	return body
}

// setBody replaces a response body, re-compressing if the original client
// asked for compression (we stripped Accept-Encoding on the way upstream so
// that we had plain text to rewrite, so it is on us to put it back).
func setBody(res *http.Response, body []byte, acceptsGzip bool) {
	res.Header.Del("Content-Encoding")
	// Whatever the child computed no longer describes these bytes.
	res.Header.Del("Etag")
	res.Header.Del("Content-Range")
	res.Header.Del("Accept-Ranges")

	if acceptsGzip && len(body) > 1024 {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(body); err == nil && zw.Close() == nil {
			body = buf.Bytes()
			res.Header.Set("Content-Encoding", "gzip")
			res.Header.Add("Vary", "Accept-Encoding")
		}
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	res.ContentLength = int64(len(body))
	res.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

func rewriteLocation(res *http.Response, prefix string) {
	loc := res.Header.Get("Location")
	if loc == "" || !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") {
		return
	}
	res.Header.Set("Location", prefix+loc)
}

// rewriteCookiePaths keeps any cookie the child sets inside the tenant's
// namespace. None of the current apps set cookies, but an app that starts to
// would otherwise scope them at "/" and collide with every other tenant on
// the origin — and with the gateway's own session cookie.
func rewriteCookiePaths(res *http.Response, prefix string) {
	cookies := res.Header.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	out := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts := strings.Split(c, ";")
		found := false
		for i, p := range parts {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(k, "Path") {
				parts[i] = " Path=" + prefix + strings.TrimSuffix(v, "/")
				found = true
			}
		}
		if !found {
			parts = append(parts, " Path="+prefix+"/")
		}
		out = append(out, strings.Join(parts, ";"))
	}
	res.Header.Del("Set-Cookie")
	for _, c := range out {
		res.Header.Add("Set-Cookie", c)
	}
}

// injectGuard inserts the account guard immediately after <head>, before any
// of the app's own scripts.
//
// Why this exists: IndexedDB, localStorage and Cache Storage are scoped to an
// ORIGIN, never to a path. /kieran/readerr/ and /alex/readerr/ are the same
// origin, so they share one database. On a shared browser that is not merely
// untidy — Alex's app would load Kieran's local-first data and then push it
// into Alex's server database. The guard detects the change of owner and
// clears local state before the app boots, after which the app re-syncs from
// its own backend, which is exactly the local-first recovery path.
//
// The real fix is an origin per user (a subdomain); this is the honest
// mitigation available inside the /<user>/<app>/ URL scheme that was asked
// for. See docs/dev/architecture.md.
func injectGuard(body []byte, rc *reqCtx) []byte {
	lower := bytes.ToLower(body)
	idx := bytes.Index(lower, []byte("<head>"))
	if idx < 0 {
		return body
	}
	at := idx + len("<head>")
	script := guardScript(rc.username, rc.app.Name)
	out := make([]byte, 0, len(body)+len(script))
	out = append(out, body[:at]...)
	out = append(out, script...)
	out = append(out, body[at:]...)
	return out
}

func guardScript(username, app string) []byte {
	// window.stop() aborts parsing and any in-flight fetches, which is what
	// keeps the app's deferred module scripts from running against the wrong
	// user's data while the asynchronous wipe is still in progress.
	const tmpl = `<script>(function(){
var OWNER=%q,APP=%q,KEY="__mux_owner";
function wipe(){
 var jobs=[];
 try{jobs.push(caches.keys().then(function(k){return Promise.all(k.map(function(n){return caches.delete(n)}))}))}catch(e){}
 try{jobs.push(navigator.serviceWorker.getRegistrations().then(function(r){return Promise.all(r.map(function(x){return x.unregister()}))}))}catch(e){}
 try{
  var names=[APP];
  if(indexedDB.databases){jobs.push(indexedDB.databases().then(function(d){
    return Promise.all(d.map(function(x){return new Promise(function(res){var q=indexedDB.deleteDatabase(x.name);q.onsuccess=q.onerror=q.onblocked=function(){res()}})}))}))}
  names.forEach(function(n){jobs.push(new Promise(function(res){var q=indexedDB.deleteDatabase(n);q.onsuccess=q.onerror=q.onblocked=function(){res()}}))});
 }catch(e){}
 try{localStorage.clear();sessionStorage.clear()}catch(e){}
 return Promise.all(jobs).catch(function(){});
}
try{
 var prev=localStorage.getItem(KEY);
 if(prev&&prev!==OWNER){
  try{window.stop()}catch(e){}
  document.documentElement.innerHTML='<head><meta charset="utf-8"><title>Switching account</title></head><body style="font:16px/1.6 system-ui,sans-serif;padding:15vh 1.5rem;text-align:center">Switching account, clearing local data...</body>';
  wipe().then(function(){try{localStorage.setItem(KEY,OWNER)}catch(e){}location.reload()});
  return;
 }
 localStorage.setItem(KEY,OWNER);
}catch(e){}
})();</script>`
	return []byte(fmt.Sprintf(tmpl, username, app))
}

// ---------------------------------------------------------------- static

// serveStatic handles apps with no backend at all: a frontend-only app the
// admin has made available per user. Same rewriting, no child process.
func (g *Gateway) serveStatic(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	fs, ok := g.static[rc.app.Name]
	if !ok {
		writeGatewayError(w, r, http.StatusNotFound, "That app has no built frontend.")
		return
	}
	rr := &rewriteRecorder{ResponseWriter: w, rc: rc, req: r}
	sub := r.Clone(r.Context())
	sub.URL.Path = rc.upstreamPath
	sub.URL.RawPath = ""
	// A byte range is measured against the file on disk, which is not the body
	// we are about to send. http.FileServer would answer 206 with offsets from
	// the pre-rewrite file; dropping the header makes it serve the whole thing
	// and lets the rewrite stay honest. Nothing an app serves from static
	// assets is large enough for ranges to matter.
	sub.Header.Del("Range")
	sub.Header.Del("If-Range")
	fs.ServeHTTP(rr, sub)
	rr.finish()
}

// rewriteRecorder buffers a static file response so the same substitution can
// be applied to it as to a proxied one. Non-rewritable types stream straight
// through untouched, so images and fonts are never buffered.
type rewriteRecorder struct {
	http.ResponseWriter
	rc       *reqCtx
	req      *http.Request
	buf      bytes.Buffer
	code     int
	decide   bool // header written, passthrough decision made
	pass     bool
	done     bool
	overflow bool // body exceeded maxRewriteBody; see finish()
}

func (w *rewriteRecorder) WriteHeader(code int) {
	if w.decide {
		return
	}
	w.decide = true
	w.code = code
	ct := w.Header().Get("Content-Type")
	w.pass = code == http.StatusNotModified || !rewritableType(ct)
	if w.pass {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	// Length and validators will be wrong once the body changes; they are
	// re-set in finish().
	w.Header().Del("Content-Length")
	w.Header().Del("Etag")
	w.Header().Del("Accept-Ranges")
}

func (w *rewriteRecorder) Write(b []byte) (int, error) {
	if !w.decide {
		w.WriteHeader(http.StatusOK)
	}
	if w.pass {
		return w.ResponseWriter.Write(b)
	}
	if w.buf.Len()+len(b) > maxRewriteBody {
		// Returning an error here is not enough on its own: http.FileServer
		// ignores write errors, so without this flag the client would receive
		// a 200 with a body silently cut off at the limit — the worst possible
		// outcome, because it looks like success.
		w.overflow = true
		return 0, fmt.Errorf("static response exceeds %d bytes", maxRewriteBody)
	}
	return w.buf.Write(b)
}

func (w *rewriteRecorder) finish() {
	if w.done || w.pass || !w.decide {
		return
	}
	w.done = true
	if w.overflow {
		slog.Error("static file too large to rewrite; refusing rather than truncating",
			"app", w.rc.app.Name, "path", w.rc.upstreamPath, "limit", maxRewriteBody)
		w.Header().Del("Content-Length")
		writeGatewayError(w.ResponseWriter, w.req, http.StatusInternalServerError,
			"That file is too large for this server to rewrite.")
		return
	}
	body := rewriteBody(w.buf.Bytes(), w.rc, w.Header().Get("Content-Type"))
	if w.rc.acceptsGzip && len(body) > 1024 {
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		if _, err := zw.Write(body); err == nil && zw.Close() == nil {
			body = gz.Bytes()
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Add("Vary", "Accept-Encoding")
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.ResponseWriter.WriteHeader(w.code)
	if w.req.Method != http.MethodHead {
		w.ResponseWriter.Write(body)
	}
}
