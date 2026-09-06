// Package localconfig shares the trusted launch policy between the application
// and its supervisor. Neither launch mode may weaken runtime authentication.
package localconfig

import (
	"fmt"
	"net"
)

func ControlURL(listen string, allowRemote, noAPIAuth bool, apiToken, runtimeToken string) (string, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("listen must use an explicit IP address")
	}
	if !ip.IsLoopback() {
		if !allowRemote {
			return "", fmt.Errorf("non-loopback listening requires explicit --allow-remote")
		}
		if !validRemoteToken(runtimeToken) {
			return "", fmt.Errorf("remote listening requires ASSISTANT_RUNTIME_TOKEN with at least 32 printable ASCII characters without whitespace, even with --no-api-auth")
		}
		if !noAPIAuth && (!validRemoteToken(apiToken) || apiToken == runtimeToken) {
			return "", fmt.Errorf("remote listening requires separate ASSISTANT_API_TOKEN and ASSISTANT_RUNTIME_TOKEN values, each at least 32 printable ASCII characters without whitespace")
		}
	}
	return HealthBaseURL(listen)
}

// HealthBaseURL maps wildcard listeners to a local dialable address. It does
// not authorize a listener; callers must also validate with ControlURL.
func HealthBaseURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("listen must use an explicit IP address")
	}
	if ip.IsUnspecified() {
		host = "::1"
		if ip.To4() != nil {
			host = "127.0.0.1"
		}
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func validRemoteToken(token string) bool {
	if len(token) < 32 {
		return false
	}
	for _, c := range token {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}
