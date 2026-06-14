package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// discoverBootstrapPeers queries the magi L2 GraphQL endpoint for the
// active witness fleet's announced libp2p multiaddrs, returning them as a
// flat list suitable for libp2pBroadcasterConfig.BootstrapPeers.
//
// The IS service uses this when -p2pBootstrapPeers is empty but the L2
// trio is configured — operators get a one-flag deployment instead of
// hand-maintaining a CSV of witness multiaddrs alongside the witness
// fleet on Hive. Witnesses re-announce every ~24h, so the per-startup
// snapshot is fresh enough; periodic re-discovery is future work
// (witness moves servers between two IS-service restarts → stale; one
// IS-service restart fixes it).
//
// Filtering:
//   - enabled = true (skips paused / decommissioned witnesses)
//   - peer_addrs non-empty (skips witnesses that announced without
//     publishing multiaddrs — observed in older binaries before
//     announcements.go started writing peer_addrs)
//
// Failure mode: returns an error if the GQL query fails (network /
// schema / 0 results). main.go fails loud on error — refusing to start
// in degraded mode is the safer default than silently degrading to an
// empty mesh (which would otherwise look identical to "is-service is
// running but rejecting every attestation").
//
// The height parameter selects which announcement era to read. The
// witnesses resolver does GetWitnessesAtBlockHeight, which is a
// "latest announcement per account at or before height" query. Passing
// a far-future height gets the latest known fleet snapshot — the same
// view a fresh L1 block streamer would have.
func discoverBootstrapPeers(ctx context.Context, gqlURL string, atHeight uint64) ([]string, error) {
	query := `query Discover($h: Uint64!) {
  witnessNodes(height: $h) {
    account
    enabled
    peer_addrs
  }
}`
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]any{"h": fmt.Sprintf("%d", atHeight)},
	})
	if err != nil {
		return nil, fmt.Errorf("marshalling discovery query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", gqlURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building discovery request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting discovery query: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("discovery query returned http %d", resp.StatusCode)
	}

	var out struct {
		Data struct {
			WitnessNodes []struct {
				Account   string   `json:"account"`
				Enabled   *bool    `json:"enabled"`
				PeerAddrs []string `json:"peer_addrs"`
			} `json:"witnessNodes"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding discovery response: %w", err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("discovery query GraphQL errors: %s", out.Errors[0].Message)
	}

	peers := make([]string, 0, len(out.Data.WitnessNodes)*2)
	skippedDisabled := 0
	skippedNoAddrs := 0
	for _, w := range out.Data.WitnessNodes {
		if w.Enabled == nil || !*w.Enabled {
			skippedDisabled++
			continue
		}
		if len(w.PeerAddrs) == 0 {
			skippedNoAddrs++
			continue
		}
		for _, a := range w.PeerAddrs {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			peers = append(peers, a)
		}
	}
	if len(peers) == 0 {
		return nil, fmt.Errorf("witnessNodes query returned 0 usable peers "+
			"(total=%d skipped_disabled=%d skipped_no_addrs=%d) — refusing to start in degraded mode",
			len(out.Data.WitnessNodes), skippedDisabled, skippedNoAddrs)
	}
	return peers, nil
}
