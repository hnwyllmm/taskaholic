package main

import (
	"strings"
	"testing"
)

func TestLocalControlURLRetainsLoopbackDefaultAndSecuresRemote(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, address, apiToken, runtimeToken, want string
		remote                                      bool
		anonymous                                   bool
	}{
		{name: "default", address: "127.0.0.1:17343", want: "http://127.0.0.1:17343"},
		{name: "ipv6 loopback", address: "[::1]:17343", want: "http://[::1]:17343"},
		{name: "remote explicit opt in", address: "0.0.0.0:17343", apiToken: a, runtimeToken: b},
		{name: "missing tokens", address: "0.0.0.0:17343", remote: true},
		{name: "missing api token", address: "0.0.0.0:17343", runtimeToken: b, remote: true},
		{name: "missing runtime token", address: "0.0.0.0:17343", apiToken: a, remote: true},
		{name: "short token", address: "0.0.0.0:17343", apiToken: "short", runtimeToken: b, remote: true},
		{name: "whitespace token", address: "0.0.0.0:17343", apiToken: a + "\n", runtimeToken: b, remote: true},
		{name: "shared token", address: "0.0.0.0:17343", apiToken: a, runtimeToken: a, remote: true},
		{name: "wildcard ipv4", address: "0.0.0.0:17343", apiToken: a, runtimeToken: b, remote: true, want: "http://127.0.0.1:17343"},
		{name: "wildcard ipv6", address: "[::]:17343", apiToken: a, runtimeToken: b, remote: true, want: "http://[::1]:17343"},
		{name: "specific interface", address: "192.0.2.10:17343", apiToken: a, runtimeToken: b, remote: true, want: "http://192.0.2.10:17343"},
		{name: "invalid", address: "not-an-address", remote: true},
		{name: "implicit wildcard", address: ":17343", apiToken: a, runtimeToken: b, remote: true},
		{name: "explicit anonymous API", address: "0.0.0.0:17343", runtimeToken: b, remote: true, anonymous: true, want: "http://127.0.0.1:17343"},
		{name: "anonymous still requires remote opt in", address: "0.0.0.0:17343", runtimeToken: b, anonymous: true},
		{name: "anonymous still requires runtime token", address: "0.0.0.0:17343", remote: true, anonymous: true},
		{name: "anonymous rejects weak runtime token", address: "0.0.0.0:17343", runtimeToken: "weak", remote: true, anonymous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := localControlURL(tc.address, tc.remote, tc.anonymous, tc.apiToken, tc.runtimeToken)
			if tc.want == "" {
				if err == nil || strings.Contains(err.Error(), a) || strings.Contains(err.Error(), b) {
					t.Fatalf("expected rejection without credentials in diagnostics, got %q / %v", got, err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("got %q / %v, want %q", got, err, tc.want)
			}
		})
	}
}
