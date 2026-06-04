package gateway

// review7 C11 — unbounded gateway action sweep.
//
// C11-a/b: executeActions processed every pending action GetPendingActions
// returned, with no cap, building an unbounded ops slice and an oversized L1
// multisig tx. An attacker queuing many tiny actions could thereby stall the
// gateway. The batch is now bounded by ledgerDb.MaxGatewayActionBatch (the DB
// read is SetLimit-bounded too).
//
// C11-c: the VSC_GATEWAY_*_INTERVAL env overrides are devnet-only; on mainnet
// the canonical intervals are enforced so a divergent interval cannot fork a
// node.

import (
	"encoding/json"
	"fmt"
	"testing"

	systemconfig "vsc-node/modules/common/system-config"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/lib/test_utils"

	"github.com/vsc-eco/hivego"
)

// capturingCreator records the vsc.actions header JSON so the test can read the
// executed-action list. All other ops are inert (embeds stubHiveCreator).
type capturingCreator struct {
	stubHiveCreator
	headerJSON string
}

func (c *capturingCreator) CustomJson(_ []string, _ []string, id string, jsonStr string) hivego.HiveOperation {
	if id == "vsc.actions" {
		c.headerJSON = jsonStr
	}
	return nil
}

func TestAuditReview7_C11a_ExecuteActionsCapsBatch(t *testing.T) {
	const bh = uint64(20) // bh % ACTION_INTERVAL == 0

	// Queue far more pending withdraws than the cap.
	const queued = ledgerDb.MaxGatewayActionBatch * 2
	actions := make(map[string]ledgerDb.ActionRecord, queued)
	for i := 0; i < queued; i++ {
		id := fmt.Sprintf("withdraw-%05d", i)
		// All at height 1 (<= bh) so every action is eligible this tick; the
		// deterministic (block_height, id) sort then orders them by id.
		actions[id] = ledgerDb.ActionRecord{
			Id: id, Status: "pending", Amount: 1000, Asset: "hbd",
			To: "hive:bob", Type: "withdraw", BlockHeight: 1,
		}
	}

	creator := &capturingCreator{}
	ms := &MultiSig{
		sconf:         systemconfig.MocknetConfig(),
		ledgerActions: &test_utils.MockActionsDb{Actions: actions},
		hiveCreator:   creator,
	}

	if _, err := ms.executeActions(bh); err != nil {
		t.Fatalf("executeActions: unexpected error: %v", err)
	}

	var header struct {
		Ops []string `json:"ops"`
	}
	if err := json.Unmarshal([]byte(creator.headerJSON), &header); err != nil {
		t.Fatalf("failed to parse vsc.actions header %q: %v", creator.headerJSON, err)
	}
	if len(header.Ops) != ledgerDb.MaxGatewayActionBatch {
		t.Fatalf("C11-a: executeActions processed %d actions, want capped at %d",
			len(header.Ops), ledgerDb.MaxGatewayActionBatch)
	}
}

func TestAuditReview7_C11c_MainnetForcesCanonicalIntervals(t *testing.T) {
	// Save + restore the package globals so we don't disturb other tests.
	savedR, savedA, savedS := ROTATION_INTERVAL, ACTION_INTERVAL, SYNC_INTERVAL
	defer func() { ROTATION_INTERVAL, ACTION_INTERVAL, SYNC_INTERVAL = savedR, savedA, savedS }()

	// Simulate an env override having been applied.
	ROTATION_INTERVAL, ACTION_INTERVAL, SYNC_INTERVAL = 3, 5, 7

	// Off-mainnet: overrides are preserved (devnet convenience).
	enforceMainnetIntervals(false)
	if ROTATION_INTERVAL != 3 || ACTION_INTERVAL != 5 || SYNC_INTERVAL != 7 {
		t.Fatalf("off-mainnet must preserve env overrides, got %d/%d/%d",
			ROTATION_INTERVAL, ACTION_INTERVAL, SYNC_INTERVAL)
	}

	// On mainnet: overrides are forced back to canonical values.
	enforceMainnetIntervals(true)
	if ROTATION_INTERVAL != mainnetRotationInterval ||
		ACTION_INTERVAL != mainnetActionInterval ||
		SYNC_INTERVAL != mainnetSyncInterval {
		t.Fatalf("mainnet must force canonical intervals, got %d/%d/%d",
			ROTATION_INTERVAL, ACTION_INTERVAL, SYNC_INTERVAL)
	}
}
