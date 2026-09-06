package main

import "work-assistant/internal/localconfig"

// Validate remote exposure before touching the database or opening a socket.
// Binding to all interfaces must not make the local Runtime dial 0.0.0.0/::.
func localControlURL(listen string, allowRemote, noAPIAuth bool, apiToken, runtimeToken string) (string, error) {
	return localconfig.ControlURL(listen, allowRemote, noAPIAuth, apiToken, runtimeToken)
}
