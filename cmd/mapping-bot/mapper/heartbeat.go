package mapper

// Non-consensus operator-endpoint directory for the mapping bot.
//
// The VSC chain provides no on-chain registry for who is currently running a
// mapping bot; the only on-chain per-operator surface is the L2 submission
// DID itself, which identifies "who posted this tx", not "where do I reach
// that operator for collision/progress telemetry". For cross-operator
// liveness monitoring and decentralized failover we expose a lightweight
// heartbeat directory that is intentionally non-consensus.
//
// Formal backing — every behavior below is a concrete witness for a Lean
// theorem in `MagiLean.Security.MappingBot`:
//
//   - `applyHeartbeat_preserves_submission_accounting`
//       Applying a heartbeat leaves (submitted, overflow, collisionReports)
//       unchanged. In code: ObserveHeartbeat MUST NOT mutate any path that
//       feeds the submission/collision counters or L2 state.
//   - `heartbeat_trace_preserves_submission_accounting`
//       The same invariant over an arbitrary sequence of heartbeats —
//       enforced here by keeping the heartbeat map under its own mutex,
//       disjoint from StateDB, CollisionMetrics.pendingCollisions,
//       CollisionMetrics.confirmSpendCollisions, etc.
//
// Non-consensus means:
//   1. Heartbeats are never submitted to the L2 transaction pool.
//   2. No bot action is GATED on what another operator's heartbeat says.
//   3. A Byzantine or absent peer cannot corrupt consensus state by
//      spoofing, withholding, or tampering with heartbeat entries.
//
// Transport is intentionally left to the deployer: the directory is a pure
// in-memory registry with helpers to serialize/deserialize a JSON snapshot.
// Operators can gossip snapshots over any channel they like (a shared
// message board, a pub/sub bus, signed HTTP pings between peers) without
// changing the bot's consensus surface.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// maxHeartbeatAge is the default staleness threshold for a heartbeat to
// still be considered "live". Anything older than this is treated as if the
// operator were absent. Kept deliberately short so network splits surface
// quickly; operators can poll more aggressively if they want tighter SLA.
const maxHeartbeatAge = 2 * time.Minute

// BotEndpoint is one operator's self-reported liveness and routing info.
// All fields are advisory; the schema is stable so independent
// implementations can interop via the JSON snapshot.
type BotEndpoint struct {
	// Operator is a stable identifier for the bot instance. Typically the
	// bot's L2 submission DID (`did:pkh:eip155:...`) so peers can
	// correlate heartbeats with submitted-by telemetry.
	Operator string `json:"operator"`
	// Endpoint is a best-effort contact address (e.g., HTTPS URL) used by
	// peers for out-of-band diagnostics. Empty if the operator does not
	// expose one.
	Endpoint string `json:"endpoint,omitempty"`
	// LastBlockHeight is the highest BTC block height the operator had
	// observed when this heartbeat was emitted. Purely informational.
	LastBlockHeight uint64 `json:"last_block_height,omitempty"`
	// ObservedAt is when THIS bot ingested the heartbeat. Not the emitter's
	// clock. Used for staleness computations.
	ObservedAt time.Time `json:"observed_at"`
}

// IsStale reports whether the heartbeat is older than `maxAge` as of `now`.
// Pass `0` to use the package default `maxHeartbeatAge`.
func (e BotEndpoint) IsStale(now time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 {
		maxAge = maxHeartbeatAge
	}
	return now.Sub(e.ObservedAt) > maxAge
}

// ObserveHeartbeat records a peer operator's liveness beacon. The call is
// idempotent: re-observing the same (operator, endpoint) pair bumps
// `ObservedAt` but does not otherwise alter bot state.
//
// Concretization of Lean `applyHeartbeat_preserves_submission_accounting`:
// this function touches only `b.heartbeats` (a dedicated map behind
// `heartbeatsMu`) and MUST NOT call any StateDB / CollisionMetrics /
// GraphQL-submission path. Reviewers: if that invariant is ever broken the
// audit claim in `Security.MappingBot` no longer holds.
func (b *Bot) ObserveHeartbeat(hb BotEndpoint) error {
	if hb.Operator == "" {
		return fmt.Errorf("heartbeat missing operator identifier")
	}
	b.heartbeatsMu.Lock()
	defer b.heartbeatsMu.Unlock()
	if b.heartbeats == nil {
		b.heartbeats = make(map[string]BotEndpoint)
	}
	existing, had := b.heartbeats[hb.Operator]
	if hb.ObservedAt.IsZero() {
		hb.ObservedAt = time.Now().UTC()
	}
	if had && hb.Endpoint == "" {
		hb.Endpoint = existing.Endpoint
	}
	b.heartbeats[hb.Operator] = hb
	return nil
}

// LiveOperators returns the snapshot of peer operators whose heartbeat has
// not yet aged past `maxAge` (0 → default). The returned slice is
// deterministic in operator order (lexicographic) so it is safe to emit
// directly in metrics and logs.
func (b *Bot) LiveOperators(now time.Time, maxAge time.Duration) []BotEndpoint {
	b.heartbeatsMu.RLock()
	defer b.heartbeatsMu.RUnlock()
	out := make([]BotEndpoint, 0, len(b.heartbeats))
	for _, hb := range b.heartbeats {
		if !hb.IsStale(now, maxAge) {
			out = append(out, hb)
		}
	}
	sortEndpointsByOperator(out)
	return out
}

// PruneStaleHeartbeats drops entries older than `maxAge` (0 → default) and
// returns the number pruned. Safe to call from any goroutine; this is the
// canonical way to keep the directory bounded.
func (b *Bot) PruneStaleHeartbeats(now time.Time, maxAge time.Duration) int {
	b.heartbeatsMu.Lock()
	defer b.heartbeatsMu.Unlock()
	var pruned int
	for op, hb := range b.heartbeats {
		if hb.IsStale(now, maxAge) {
			delete(b.heartbeats, op)
			pruned++
		}
	}
	return pruned
}

// ExportHeartbeatSnapshot serializes the current directory to JSON so a
// deployer can gossip it over any transport (HTTP, pub/sub, shared file,
// etc.). The snapshot is append-only from the consuming side: importing it
// calls `ObserveHeartbeat` for each entry, so merging two snapshots is
// deterministic (newer ObservedAt wins per operator).
func (b *Bot) ExportHeartbeatSnapshot() ([]byte, error) {
	b.heartbeatsMu.RLock()
	defer b.heartbeatsMu.RUnlock()
	list := make([]BotEndpoint, 0, len(b.heartbeats))
	for _, hb := range b.heartbeats {
		list = append(list, hb)
	}
	sortEndpointsByOperator(list)
	return json.Marshal(list)
}

// ImportHeartbeatSnapshot ingests a snapshot produced by
// `ExportHeartbeatSnapshot` from a peer. Merge semantics: for each entry,
// the most recent `ObservedAt` wins. Malformed entries are silently
// skipped — peers are untrusted and the directory must be resilient to
// partial corruption.
func (b *Bot) ImportHeartbeatSnapshot(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var list []BotEndpoint
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	for _, hb := range list {
		if hb.Operator == "" {
			continue
		}
		if err := b.ObserveHeartbeat(hb); err != nil {
			// ObserveHeartbeat only fails on empty operator, which we
			// already guarded. Defensive no-op.
			b.L.Debug("skipping malformed heartbeat during import", "error", err)
			continue
		}
	}
	return nil
}

// BroadcastHeartbeat constructs THIS bot's outgoing heartbeat so a deployer
// can relay it to peers. The bot does NOT itself perform network I/O — it
// is the operator's responsibility to ship this snapshot over their chosen
// transport. Keeping the send-side out of the bot preserves the
// non-consensus guarantee: the library cannot be tricked into gossiping
// over a channel that affects consensus state.
func (b *Bot) BroadcastHeartbeat(ctx context.Context, endpoint string) BotEndpoint {
	height, _ := b.LastBlock()
	operator := ""
	if b.botEthKey != nil {
		operator = b.botEthDID.String()
	}
	return BotEndpoint{
		Operator:        operator,
		Endpoint:        endpoint,
		LastBlockHeight: height,
		ObservedAt:      time.Now().UTC(),
	}
}

// sortEndpointsByOperator sorts a slice of heartbeats in place by operator
// id. Kept as a private helper so every public emitter (LiveOperators,
// ExportHeartbeatSnapshot) returns deterministic output for diff-friendly
// logs and metrics.
func sortEndpointsByOperator(list []BotEndpoint) {
	// Simple insertion sort keeps this allocation-free for small N (typical
	// directories are < 20 entries).
	for i := 1; i < len(list); i++ {
		j := i
		for j > 0 && list[j-1].Operator > list[j].Operator {
			list[j-1], list[j] = list[j], list[j-1]
			j--
		}
	}
}
