package server

import (
	"net/http/httptest"
	"testing"

	"github.com/scottymacleod/aigateway/internal/config"
)

// P0.14: SourceIP strips IPv6 brackets and only believes X-Forwarded-For
// entries added by trusted proxies, walking right-to-left.
func TestSourceIP(t *testing.T) {
	noProxies, err := config.Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	proxies, err := config.Parse([]byte("settings:\n  trusted_proxies: [\"10.0.0.0/8\", \"fd00::/8\"]\n"))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		cfg    *config.Config
		remote string
		xff    []string
		want   string
	}{
		{"ipv4", noProxies, "203.0.113.9:5555", nil, "203.0.113.9"},
		{"ipv6 loopback", noProxies, "[::1]:8080", nil, "::1"},
		{"ipv6 full", noProxies, "[2001:db8::7]:443", nil, "2001:db8::7"},
		{"xff ignored without trusted proxies", noProxies, "203.0.113.9:1", []string{"6.6.6.6"}, "203.0.113.9"},
		{"xff ignored from untrusted peer", proxies, "203.0.113.9:1", []string{"6.6.6.6"}, "203.0.113.9"},
		{"spoofed leftmost behind trusted proxy", proxies, "10.0.0.5:1", []string{"6.6.6.6, 203.0.113.9"}, "203.0.113.9"},
		{"two trusted hops", proxies, "10.0.0.5:1", []string{"6.6.6.6, 203.0.113.9, 10.0.0.7"}, "203.0.113.9"},
		{"multiple xff headers", proxies, "10.0.0.5:1", []string{"6.6.6.6", "203.0.113.9"}, "203.0.113.9"},
		{"all hops trusted", proxies, "10.0.0.5:1", []string{"10.0.0.8"}, "10.0.0.8"},
		{"garbage entry stops at proxy", proxies, "10.0.0.5:1", []string{"6.6.6.6, not-an-ip"}, "10.0.0.5"},
		{"ipv6 trusted proxy", proxies, "[fd00::1]:443", []string{"6.6.6.6, 2001:db8::5"}, "2001:db8::5"},
		{"ipv4-mapped peer", proxies, "[::ffff:10.0.0.5]:1", []string{"203.0.113.9"}, "203.0.113.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := SourceIP(tc.cfg, r); got != tc.want {
				t.Errorf("SourceIP = %q, want %q", got, tc.want)
			}
		})
	}
}
