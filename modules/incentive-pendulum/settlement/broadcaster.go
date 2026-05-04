package settlement

import (
	"encoding/json"
	"fmt"

	"vsc-node/lib/hive"

	"github.com/vsc-eco/hivego"
)

// Broadcaster is the wire-out side of the W5 settlement leader: takes a
// fully-built SettlementPayload, serializes it as a vsc.pendulum_settlement
// custom-JSON op, and broadcasts via Hive. Returns the L1 tx ID on success.
//
// Implementations MUST be safe to call from a block-tick goroutine. nil
// broadcaster is treated as "broadcast disabled" by the orchestration —
// useful for state-engine-only test harnesses.
type Broadcaster interface {
	Broadcast(payload SettlementPayload) (string, error)
}

// HiveBroadcaster is the production Broadcaster, posting via the same
// hivego.CustomJsonOperation pattern the TSS module uses.
type HiveBroadcaster struct {
	creator      hive.HiveTransactionCreator
	hiveUsername string
}

// NewHiveBroadcaster builds a Broadcaster wired to a live Hive RPC client.
// hiveUsername MUST be the active-auth account that signs the
// vsc.pendulum_settlement op — typically identityConfig.Get().HiveUsername.
func NewHiveBroadcaster(creator hive.HiveTransactionCreator, hiveUsername string) *HiveBroadcaster {
	return &HiveBroadcaster{creator: creator, hiveUsername: hiveUsername}
}

// Broadcast emits the payload as a single-op transaction signed by
// hiveUsername's active key. Returns the broadcast tx ID.
func (b *HiveBroadcaster) Broadcast(payload SettlementPayload) (string, error) {
	if b == nil || b.creator == nil {
		return "", fmt.Errorf("hive broadcaster not configured")
	}
	if b.hiveUsername == "" {
		return "", fmt.Errorf("hive broadcaster missing username")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal settlement payload: %w", err)
	}
	op := hivego.CustomJsonOperation{
		RequiredAuths:        []string{b.hiveUsername},
		RequiredPostingAuths: []string{},
		Id:                   "vsc.pendulum_settlement",
		Json:                 string(body),
	}
	tx := b.creator.MakeTransaction([]hivego.HiveOperation{op})
	if err := b.creator.PopulateSigningProps(&tx, nil); err != nil {
		return "", fmt.Errorf("populate signing props: %w", err)
	}
	sig, err := b.creator.Sign(tx)
	if err != nil {
		return "", fmt.Errorf("sign settlement tx: %w", err)
	}
	tx.AddSig(sig)
	txID, err := b.creator.Broadcast(tx)
	if err != nil {
		return "", fmt.Errorf("broadcast settlement tx: %w", err)
	}
	return txID, nil
}
