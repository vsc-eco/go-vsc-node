package tss

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsTransportErr locks in the deliberately-narrow classification: only
// libp2p's own idle-death signals ("no recent network activity", "keepalive
// timeout") count as a dead connection worth evicting. Ambiguous errors that a
// LIVE-but-busy peer can also emit under load must NOT classify as dead —
// evicting on them would drop a concurrent, un-retried reshare message (the
// regression this narrowing guards against).
func TestIsTransportErr(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want bool
	}{
		// Genuine idle-death — evict.
		{"keepalive timeout", "yamux: keepalive timeout", true},
		{"no recent network activity", "timeout: no recent network activity", true},
		// The real production strings observed in the mainnet stall: the
		// idle-death token is embedded in a longer message, so substring
		// matching must still catch it.
		{"prod stream-reset+keepalive", "msgpack decode error [pos 0]: stream reset: stream reset: connection closed: keepalive timeout", true},
		{"prod no-recent-activity", "msgpack decode error [pos 0]: timeout: no recent network activity", true},
		// Case-insensitivity (errors from different layers vary in casing).
		{"uppercase", "KEEPALIVE TIMEOUT", true},

		// Ambiguous — a live-but-busy peer emits these under resource-manager /
		// stream-limit pressure. Must NOT evict.
		{"bare stream reset", "stream reset", false},
		{"bare connection closed", "connection closed", false},
		{"canceled by remote", "msgpack encode error: stream reset (remote): code: 0x0: transport error: stream 5 canceled by remote with error code 0", false},
		{"context deadline exceeded", "context deadline exceeded", false},
		{"eof", "EOF", false},
		{"connection refused", "dial tcp: connect: connection refused", false},
		{"unrelated app error", "no documents in result", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isTransportErr(errors.New(tc.err)))
		})
	}

	// A nil error is never a transport failure.
	assert.False(t, isTransportErr(nil))
}

// TestParseWitnessAddrs verifies announced multiaddr strings are parsed,
// malformed entries are skipped rather than failing the whole set, and empty
// input yields an empty (non-nil-panicking) slice.
func TestParseWitnessAddrs(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		assert.Len(t, parseWitnessAddrs(nil), 0)
		assert.Len(t, parseWitnessAddrs([]string{}), 0)
	})

	t.Run("valid addrs parsed (real witness announce format)", func(t *testing.T) {
		in := []string{
			"/ip4/121.99.241.161/tcp/10720",
			"/ip4/121.99.241.161/udp/10720/quic-v1",
			"/ip6/2404:4400:4122:4200::1189/tcp/10720",
		}
		got := parseWitnessAddrs(in)
		assert.Len(t, got, 3)
		// Round-trips back to the same canonical strings.
		for i, a := range got {
			assert.Equal(t, in[i], a.String())
		}
	})

	t.Run("malformed entries skipped, valid ones kept", func(t *testing.T) {
		in := []string{
			"not-a-multiaddr",
			"/ip4/10.0.0.1/tcp/4001",
			"",
			"/nonsense/xyz",
			"/ip4/10.0.0.2/udp/4001/quic-v1",
		}
		got := parseWitnessAddrs(in)
		assert.Len(t, got, 2)
		assert.Equal(t, "/ip4/10.0.0.1/tcp/4001", got[0].String())
		assert.Equal(t, "/ip4/10.0.0.2/udp/4001/quic-v1", got[1].String())
	})

	t.Run("all malformed yields empty", func(t *testing.T) {
		assert.Len(t, parseWitnessAddrs([]string{"bad", "worse", ""}), 0)
	})
}
