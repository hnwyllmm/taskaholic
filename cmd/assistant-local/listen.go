package main

import (
	"fmt"
	"net"
)

// Validate remote exposure before touching the database or opening a socket.
// Binding to all interfaces must not make the local Runtime dial 0.0.0.0/::.
func localControlURL(listen string, allowRemote bool, apiToken, runtimeToken string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
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
		if !validRemoteToken(apiToken) || !validRemoteToken(runtimeToken) || apiToken == runtimeToken {
			return "", fmt.Errorf("remote listening requires separate ASSISTANT_API_TOKEN and ASSISTANT_RUNTIME_TOKEN values, each at least 32 printable ASCII characters without whitespace")
		}
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
