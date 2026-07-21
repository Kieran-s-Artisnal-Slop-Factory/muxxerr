package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func serverWithProxies(t *testing.T, cidrs ...string) *Server {
	t.Helper()
	tp, err := parseTrustedProxies(cidrs)
	if err != nil {
		t.Fatalf("parseTrustedProxies(%v): %v", cidrs, err)
	}
	return &Server{trusted: tp}
}

func request(remote string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// This is a security boundary in both directions. Believing X-Forwarded-For
// from anyone lets an attacker forge the key the login throttle counts against;
// believing it from nobody collapses every user behind a proxy onto one key, so
// one person's typos lock out the whole server.
func TestClientIPTrustBoundary(t *testing.T) {
	cases := []struct {
		name    string
		proxies []string
		remote  string
		headers map[string]string
		want    string
	}{
		{
			name:   "no proxy, no headers: the socket address",
			remote: "203.0.113.9:5555",
			want:   "203.0.113.9",
		},
		{
			name:    "an untrusted peer cannot forge the header",
			remote:  "203.0.113.9:5555",
			headers: map[string]string{"X-Forwarded-For": "198.51.100.1"},
			want:    "203.0.113.9",
		},
		{
			name:    "loopback is trusted without configuration",
			remote:  "127.0.0.1:5555",
			headers: map[string]string{"X-Forwarded-For": "198.51.100.1"},
			want:    "198.51.100.1",
		},
		{
			name:    "IPv6 loopback too",
			remote:  "[::1]:5555",
			headers: map[string]string{"X-Forwarded-For": "198.51.100.1"},
			want:    "198.51.100.1",
		},
		{
			name:    "a configured proxy is trusted",
			proxies: []string{"172.18.0.0/16"},
			remote:  "172.18.0.5:5555",
			headers: map[string]string{"X-Forwarded-For": "198.51.100.1"},
			want:    "198.51.100.1",
		},
		{
			name:    "a peer outside the configured range is not",
			proxies: []string{"172.18.0.0/16"},
			remote:  "172.19.0.5:5555",
			headers: map[string]string{"X-Forwarded-For": "198.51.100.1"},
			want:    "172.19.0.5",
		},
		{
			name:    "a bare address configures a single host",
			proxies: []string{"10.1.2.3"},
			remote:  "10.1.2.3:5555",
			headers: map[string]string{"X-Forwarded-For": "198.51.100.1"},
			want:    "198.51.100.1",
		},
		{
			name:    "the leftmost entry is the original client",
			remote:  "127.0.0.1:5555",
			headers: map[string]string{"X-Forwarded-For": "198.51.100.1, 10.0.0.1, 10.0.0.2"},
			want:    "198.51.100.1",
		},
		{
			name:    "a garbage forwarded value falls back to the peer",
			remote:  "127.0.0.1:5555",
			headers: map[string]string{"X-Forwarded-For": "not-an-ip"},
			want:    "127.0.0.1",
		},
		{
			name:    "an empty forwarded value falls back to the peer",
			remote:  "127.0.0.1:5555",
			headers: map[string]string{"X-Forwarded-For": "   "},
			want:    "127.0.0.1",
		},
		{
			name:    "X-Real-IP is used when there is no XFF",
			remote:  "127.0.0.1:5555",
			headers: map[string]string{"X-Real-Ip": "198.51.100.7"},
			want:    "198.51.100.7",
		},
		{
			name:    "X-Real-IP is ignored from an untrusted peer",
			remote:  "203.0.113.9:5555",
			headers: map[string]string{"X-Real-Ip": "198.51.100.7"},
			want:    "203.0.113.9",
		},
		{
			name:   "a RemoteAddr without a port still works",
			remote: "203.0.113.9",
			want:   "203.0.113.9",
		},
		{
			name:    "IPv6 CIDR",
			proxies: []string{"fd00::/8"},
			remote:  "[fd00::1]:5555",
			headers: map[string]string{"X-Forwarded-For": "198.51.100.1"},
			want:    "198.51.100.1",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := serverWithProxies(t, c.proxies...)
			if got := s.clientIP(request(c.remote, c.headers)); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseTrustedProxiesRejectsNonsense(t *testing.T) {
	for _, bad := range []string{"not-an-ip", "10.0.0.0/99", "hostname.example.com"} {
		if _, err := parseTrustedProxies([]string{bad}); err == nil {
			t.Errorf("parseTrustedProxies(%q) accepted it; a typo here silently widens who is believed", bad)
		}
	}
	// Blank entries are ignored rather than rejected, so a trailing comma in a
	// hand-edited config is not a startup failure.
	tp, err := parseTrustedProxies([]string{"", "  ", "10.0.0.0/8"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tp) != 1 {
		t.Errorf("got %d networks, want 1", len(tp))
	}
}
