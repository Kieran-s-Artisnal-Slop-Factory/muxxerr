// SSRF policy for endpoints that fetch a URL the caller supplied.
//
// readerr's GET /title?url=... is the motivating case: it exists so the app
// can read a page title that the browser cannot fetch itself because of CORS,
// and it is genuinely needed for link capture to work. Standalone on one
// person's laptop that is unremarkable. Behind this gateway it becomes
// reachable by anyone who can sign up, on a machine that probably sits on a
// home or office network — which turns it into a probe for router admin
// pages, other containers, and cloud instance-metadata endpoints, with the
// fetched page's <title> echoed back as the read channel.
//
// So: resolve the target before the child ever sees it, and refuse anything
// that lands on a private, loopback or link-local address.
//
// The honest limitation is that this is a check-then-use gap. We resolve the
// name here and the app resolves it again a moment later; a DNS record with a
// one-second TTL can differ between the two, and a public URL can redirect to
// a private one after we have stopped looking. Closing that properly needs a
// dialer-level control inside the app (patches/05-hardening.md describes it).
// This blocks the direct attempt, which is the one that actually gets tried.
package gateway

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"

	"local-multiplexer/internal/config"
)

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// resolveTimeout bounds the lookup a guarded request costs us. A name that
// will not resolve quickly is not one we are going to allow anyway.
const resolveTimeout = 3 * time.Second

// checkGuards applies any guarded-route policy for this path. It returns a
// message and true when the request must be refused.
func checkGuards(app *config.App, path string, query url.Values) (string, bool) {
	for _, g := range app.GuardedRoutes {
		if g.Path != path || g.Policy != "block-private" {
			continue
		}
		raw := query.Get(g.Param)
		if raw == "" {
			return "", false // nothing to check; let the app reject it
		}
		if msg, bad := targetIsForbidden(raw); bad {
			return msg, true
		}
	}
	return "", false
}

// targetIsForbidden reports whether a caller-supplied URL points somewhere
// this server must not fetch on their behalf.
func targetIsForbidden(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "That URL could not be parsed.", true
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		// file:, gopher:, ftp: and friends have no business here.
		return "Only http and https URLs can be fetched.", true
	}
	host := u.Hostname()
	if host == "" {
		return "That URL has no host.", true
	}

	// A literal IP needs no lookup, and skipping the lookup means a literal
	// cannot be smuggled past us by a resolver quirk.
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return "That address is on a private network, so it will not be fetched.", true
		}
		return "", false
	}

	// "localhost" and anything under .localhost, .internal or .local resolve
	// inward by definition; reject them by name rather than trusting the
	// resolver to agree.
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || hasSuffixAny(lower, ".localhost", ".internal", ".local", ".home.arpa") {
		return "That hostname refers to this machine or the local network.", true
	}

	resolver := &net.Resolver{}
	ctx, cancel := contextWithTimeout(resolveTimeout)
	defer cancel()
	ips, err := resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return "That hostname could not be resolved.", true
	}
	if len(ips) == 0 {
		return "That hostname did not resolve to any address.", true
	}
	// Every answer must be public. One private record is enough to refuse:
	// which one the app would have picked is not ours to predict.
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return "That hostname resolves to a private network address, so it will not be fetched.", true
		}
	}
	return "", false
}

func hasSuffixAny(s string, suffixes ...string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// isPrivateIP covers the ranges that should never be reachable through a
// user-supplied URL: loopback, RFC1918, carrier-grade NAT, link-local
// (including 169.254.169.254, the cloud metadata address), unique-local IPv6,
// and the unspecified address.
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// IsPrivate does not cover 100.64.0.0/10 (carrier-grade NAT, and what
	// Tailscale hands out) or the IPv4 reserved blocks.
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10
			return true
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
			return true
		case v4[0] >= 240: // 240.0.0.0/4 reserved, includes broadcast
			return true
		}
		return false
	}
	// IPv4-mapped IPv6 was handled above via To4. Reject IPv6 unique-local
	// (fc00::/7), which IsPrivate covers, plus 6to4/Teredo wrappers that can
	// carry a private v4 payload.
	if len(ip) == net.IPv6len {
		if ip[0] == 0x20 && ip[1] == 0x02 { // 2002::/16, 6to4
			return isPrivateIP(net.IPv4(ip[2], ip[3], ip[4], ip[5]))
		}
		if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x00 && ip[3] == 0x00 { // 2001::/32, Teredo
			return true
		}
	}
	return false
}
