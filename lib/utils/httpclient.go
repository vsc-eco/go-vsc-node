package utils

import (
	"net/http"
	"time"
)

// HTTP2PingAfter and HTTP2PingTimeout are the bots' HTTP/2 connection health
// checks: a connection with no frame for HTTP2PingAfter is pinged, and closed
// if the ping is not answered within HTTP2PingTimeout.
var (
	HTTP2PingAfter   = 15 * time.Second
	HTTP2PingTimeout = 10 * time.Second
)

// NewHTTPTransport is http.DefaultTransport's settings plus HTTP/2 health
// checks. I-35 (testnet 11-05 ~01:22-01:38 CET): an established HTTP/2
// connection to the L1 RPC went silent (no RST, no FIN; new connections to the
// same address worked). Go's default transport runs no health check, so every
// request was sent on the dead connection and waited out its timeout for ~16
// minutes, until TCP gave up. With pings it is closed and replaced within
// HTTP2PingAfter+HTTP2PingTimeout.
func NewHTTPTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: HTTP2PingAfter, PingTimeout: HTTP2PingTimeout}
	return t
}

// NewHTTPClient is an http.Client with timeout and NewHTTPTransport.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: NewHTTPTransport()}
}
