// Working out who is asking, when something else is asking on their behalf.
//
// The login throttle counts failures against two keys: the username, and the
// caller's IP. That second one is only useful if it is actually the caller's.
//
// Two ways to get it wrong, and they fail in opposite directions:
//
//   - Trust X-Forwarded-For unconditionally, and it stops being a fact about
//     the caller and becomes a field the caller chose. Anyone can spray guesses
//     from a new "address" per attempt and the per-IP throttle never fires.
//
//   - Trust it from nobody, and every request behind a reverse proxy reports
//     the proxy's address. On a container network that is one bridge IP shared
//     by everyone, so three fat-fingered passwords from one person lock out
//     every user on the server. This is the failure that actually happens,
//     because the deployment that causes it is the recommended one.
//
// So: honour the header from peers you have named, and from nobody else.
// Loopback is trusted by default because a proxy on the same host is the
// common case and the alternative is that the out-of-the-box experience is
// subtly broken.
package web

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// trustedProxies is the parsed form of site.trusted_proxies, plus loopback.
type trustedProxies []*net.IPNet

// parseTrustedProxies turns CIDR strings into networks. A bare address is
// accepted and treated as a single host, because writing /32 for one proxy is
// a detail nobody should have to remember.
func parseTrustedProxies(cidrs []string) (trustedProxies, error) {
	var out trustedProxies
	for _, raw := range cidrs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(raw); err == nil {
			out = append(out, n)
			continue
		}
		ip := net.ParseIP(raw)
		if ip == nil {
			return nil, fmt.Errorf("trusted_proxies: %q is not an IP address or CIDR range", raw)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

// trusts reports whether a peer address may set X-Forwarded-For.
//
// Loopback is always trusted and is not configurable: a process on the same
// machine can already reach the gateway directly, so refusing to believe it
// would buy nothing and would break the single-host proxy setup that the
// deployment docs recommend.
func (t trustedProxies) trusts(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range t {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP resolves the address the throttle should count against.
//
// The leftmost X-Forwarded-For entry is used, which is the original client as
// each hop appends itself. That value is only as trustworthy as the nearest
// proxy — a trusted proxy that forwards a header it received from the internet
// without replacing it is passing on a lie — which is why this is opt-in per
// deployment rather than a default.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !s.trusted.trusts(host) {
		return host
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		first, _, _ := strings.Cut(fwd, ",")
		if first = strings.TrimSpace(first); first != "" {
			// A proxy that sends a malformed value should not be able to turn
			// the throttle key into arbitrary text.
			if net.ParseIP(first) != nil {
				return first
			}
		}
	}
	// Caddy and Traefik send X-Real-IP too, and some setups send only it.
	if real := strings.TrimSpace(r.Header.Get("X-Real-Ip")); real != "" && net.ParseIP(real) != nil {
		return real
	}
	return host
}
