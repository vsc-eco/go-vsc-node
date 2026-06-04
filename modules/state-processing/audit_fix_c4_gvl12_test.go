package state_engine_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	rcDb "vsc-node/modules/db/vsc/rcs"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"

	stateEngine "vsc-node/modules/state-processing"
	tss_helpers "vsc-node/modules/tss/helpers"
)

// countingElectionDb wraps the real MockElectionDb and records every height
// passed to GetElectionByHeight. This is the observable discriminator for the
// GV-L12 staleness boundary: in state_engine.go the tss_commitment handler
// calls GetElectionByHeight(commitment.BlockHeight) at line ~1201 ONLY AFTER
// the staleness check at line ~1196 lets the commitment through (i.e. when it
// does NOT `continue`). So "was GetElectionByHeight called with the
// commitment's BlockHeight?" tells us, deterministically and without depending
// on BLS verification, whether the staleness gate accepted or rejected the
// commitment.
type countingElectionDb struct {
	*test_utils.MockElectionDb
	mu              sync.Mutex
	heightsQueried  map[uint64]int
}

func (c *countingElectionDb) GetElectionByHeight(height uint64) (elections.ElectionResult, error) {
	c.mu.Lock()
	c.heightsQueried[height]++
	c.mu.Unlock()
	return c.MockElectionDb.GetElectionByHeight(height)
}

func (c *countingElectionDb) queried(height uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.heightsQueried[height] > 0
}

// gvl12Env builds a StateEngine wired with a countingElectionDb so the
// GV-L12 test can observe the real ProcessBlock path. It mirrors
// newTestEnvWithConsensus's wiring but swaps in the counting election DB.
type gvl12Env struct {
	Reader     *stateEngine.MockReader
	Creator    *stateEngine.MockCreator
	ElectionDb *countingElectionDb
}

func newGVL12Env() *gvl12Env {
	sconf := systemconfig.MocknetConfig()

	witnessesDb := &test_utils.MockWitnessDb{
		ByAccount: make(map[string]*witnesses.Witness),
		ByPeerId:  make(map[string][]witnesses.Witness),
	}
	electionDb := &countingElectionDb{
		MockElectionDb: &test_utils.MockElectionDb{
			Elections:         make(map[uint64]*elections.ElectionResult),
			ElectionsByHeight: make(map[uint64]elections.ElectionResult),
		},
		heightsQueried: make(map[uint64]int),
	}
	contractDb := &test_utils.MockContractDb{Contracts: make(map[string]contracts.Contract)}
	contractState := &test_utils.MockContractStateDb{Outputs: make(map[string]contracts.ContractOutput)}
	txDb := &test_utils.MockTxDb{Records: make(map[string]transactions.TransactionRecord)}
	ledgerDbImpl := &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	balanceDb := &test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	interestClaims := &test_utils.MockInterestClaimsDb{}
	vscBlocksDb := &test_utils.MockVscBlocksDb{Blocks: make([]vscBlocks.VscHeaderRecord, 0)}
	actionsDb := &test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
	mockRcDb := &test_utils.MockRcDb{Records: make(map[string][]rcDb.RcRecord)}
	nonceDb := &test_utils.MockNonceDb{Nonces: make(map[string]uint64)}
	tssKeys := &test_utils.MockTssKeysDb{Keys: make(map[string]tss_db.TssKey)}
	tssCommitments := &test_utils.MockTssCommitmentsDb{Commitments: make(map[string]tss_db.TssCommitment)}
	tssRequests := &test_utils.MockTssRequestsDb{Requests: make(map[string]tss_db.TssRequest)}

	se := stateEngine.New(
		sconf, nil,
		witnessesDb, electionDb, contractDb, contractState,
		txDb, ledgerDbImpl, balanceDb, nil,
		interestClaims, vscBlocksDb, actionsDb, mockRcDb, nonceDb,
		tssKeys, tssCommitments, tssRequests,
		nil, // pendulumSettlementsDb
		nil, // consensusStateDb
		nil, // wasm
		nil, // identityConfig
	)

	mockReader := stateEngine.NewMockReader()
	mockReader.ProcessFunction = func(block hive_blocks.HiveBlock, headHeight *uint64) {
		se.ProcessBlock(block)
	}
	mockCreator := stateEngine.NewMockCreator(mockReader)

	return &gvl12Env{
		Reader:     mockReader,
		Creator:    mockCreator,
		ElectionDb: electionDb,
	}
}

// submitCommitmentAtHeight broadcasts a vsc.tss_commitment whose BlockHeight is
// commitmentHeight, then processes the next block. The block number processed is
// Reader.LastBlock+1, so the caller controls the commitment's age via LastBlock.
func (e *gvl12Env) submitCommitmentAtHeight(commitmentHeight uint64) {
	commitment := tss_helpers.SignedCommitment{
		BaseCommitment: tss_helpers.BaseCommitment{
			Type:        "reshare",
			SessionId:   "reshare-gvl12-testkey",
			KeyId:       "testkey",
			Commitment:  "AAAA",
			Epoch:       1,
			BlockHeight: commitmentHeight,
		},
		Signature: "deadbeef",
		BitSet:    "AA",
	}
	jsonBytes, _ := json.Marshal([]tss_helpers.SignedCommitment{commitment})
	e.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"testwitness"},
		Id:            "vsc.tss_commitment",
		Json:          string(jsonBytes),
	})
	e.Reader.CreateBlock()
	time.Sleep(200 * time.Millisecond)
}

// TestFixGVL12_StalenessBoundary_Production — TRUE regression for GV-L12. This
// drives the REAL staleness check in state_engine.go:1196 through ProcessBlock,
// not a reimplemented predicate. The defect in the prior version of this test
// was that it hardcoded `<=` in a local closure and never touched production
// code, so it stayed green even when the fix was reverted to `<`.
//
// Discriminator: GetElectionByHeight(commitment.BlockHeight) at state_engine.go
// ~1201 is reached ONLY when the staleness check does NOT `continue`. We submit
// a commitment whose age is EXACTLY TSS_COMMITMENT_MAX_STALENESS (28800) and
// assert the election lookup is NEVER performed for that height — i.e. the
// commitment was rejected at the staleness gate.
//
//	with fix  (`<=`): bh + 28800 <= block  -> 28800 <= 28800 -> true  -> REJECT (no lookup)  -> PASS
//	with bug  (`<`) : bh + 28800 <  block  -> 28800 <  28800 -> false -> ACCEPT (lookup runs) -> FAIL
//
// We also pin the two neighbouring ages so the boundary is anchored from both
// sides: MAX-1 must be accepted (lookup runs) under both operators, and MAX+1
// must be rejected (no lookup) under both operators. Only the MAX case flips
// between `<` and `<=`, which is exactly what GV-L12 changed.
func TestFixGVL12_StalenessBoundary_Production(t *testing.T) {
	maxStale := params.TSS_COMMITMENT_MAX_STALENESS
	if maxStale != 28800 {
		t.Fatalf("GV-L12: expected TSS_COMMITMENT_MAX_STALENESS=28800, got %d", maxStale)
	}

	// Use a fixed block number well above maxStale so commitment heights stay
	// positive and unambiguous. block.BlockNumber = Reader.LastBlock + 1.
	const blockNumber = uint64(50000)
	lastBlock := blockNumber - 1 // CreateBlock() processes lastBlock+1

	cases := []struct {
		name             string
		age              uint64
		wantLookupOnPath bool // true => staleness passed => election lookup must occur
		why              string
	}{
		{
			name:             "age==MAX-1 (fresh, both operators accept)",
			age:              maxStale - 1,
			wantLookupOnPath: true,
			why:              "within window: bh+28800 > block under both < and <=",
		},
		{
			name:             "age==MAX (BOUNDARY — GV-L12 flips this)",
			age:              maxStale,
			wantLookupOnPath: false,
			why:              "with fixed <= this is rejected; with buggy < it is wrongly accepted",
		},
		{
			name:             "age==MAX+1 (stale, both operators reject)",
			age:              maxStale + 1,
			wantLookupOnPath: false,
			why:              "beyond window: rejected under both < and <=",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newGVL12Env()
			env.Reader.LastBlock = lastBlock

			commitmentHeight := blockNumber - c.age
			// Sanity: confirm the age we intend is the age we get.
			if commitmentHeight+c.age != blockNumber {
				t.Fatalf("test setup: commitmentHeight=%d age=%d != blockNumber=%d",
					commitmentHeight, c.age, blockNumber)
			}

			env.submitCommitmentAtHeight(commitmentHeight)

			gotLookup := env.ElectionDb.queried(commitmentHeight)
			if gotLookup != c.wantLookupOnPath {
				if c.age == maxStale && gotLookup {
					t.Fatalf("GV-L12 REGRESSED: a commitment at age==MAX_STALENESS (%d) "+
						"passed the staleness gate (election lookup ran for height %d). "+
						"The production check at state_engine.go:1196 is using strict `<` "+
						"instead of `<=`. (%s)",
						maxStale, commitmentHeight, c.why)
				}
				t.Fatalf("GV-L12 %s: election-lookup-reached=%v want %v (%s)",
					c.name, gotLookup, c.wantLookupOnPath, c.why)
			}
		})
	}
}
