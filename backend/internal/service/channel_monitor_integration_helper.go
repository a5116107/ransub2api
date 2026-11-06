//go:build integration

package service

import "net/http"

// SetChannelMonitorHTTPClientForTesting replaces only the isolated integration
// fixture transport, allowing loopback endpoints without changing production SSRF rules.
func SetChannelMonitorHTTPClientForTesting(client *http.Client) func() {
	previous, previousPing := monitorHTTPClient, monitorPingHTTPClient
	monitorHTTPClient, monitorPingHTTPClient = client, client
	return func() { monitorHTTPClient, monitorPingHTTPClient = previous, previousPing }
}
