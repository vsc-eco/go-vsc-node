package state_engine

import (
	"errors"
	"fmt"
	"strings"

	"vsc-node/modules/common/delegationmode"
	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/witnesses"

	"go.mongodb.org/mongo-driver/mongo"
)

// getWitnessAtHeightOrBlock is the FAIL-STOP witness read the delegation timelock
// relies on (the witness analog of GetElectionInfoOrBlock). A genuine "no
// announcement below height" (mongo.ErrNoDocuments) returns nil — every node sees
// that identically. A transient infra error BLOCKS until the DB recovers, so a
// per-node blip can never flip the timelock decision: at ingest it would persist
// a divergent maturity (durable fork until reindex), and at read it would gate
// stake acceptance / the settlement split differently across nodes. `account` is
// the bare Hive name.
func (se *StateEngine) getWitnessAtHeightOrBlock(account string, height uint64) *witnesses.Witness {
	var out *witnesses.Witness
	bh := height
	blockingRetry(fmt.Sprintf("GetWitnessAtHeight(%s @%d)", account, height), func() error {
		w, err := se.witnessDb.GetWitnessAtHeight(account, &bh)
		if err == nil {
			out = w
			return nil
		}
		if errors.Is(err, mongo.ErrNoDocuments) {
			out = nil // deterministic absence: no prior announcement below height
			return nil
		}
		return err // infra failure → keep blocking
	})
	return out
}

// NodeDelegationMode returns the EFFECTIVE consensus-delegation mode node
// `account` has opted into, as of blockHeight, normalized to a known value
// (delegationmode.{Deactivated,Share,Custom}). It defaults to Deactivated when
// the node has no announcement at-or-before blockHeight, an unset mode, or the
// witness DB is unavailable — making delegation strict opt-in.
//
// "Effective" accounts for the downgrade timelock (consensus 0.5.0+): an adverse
// mode change (leaving Share, which strips delegators' on-chain reward share) is
// deferred until DELEGATION_DOWNGRADE_LOCK_EPOCHS elapse, so this returns the
// prior protected mode until then (see resolveDelegationMode /
// computeDelegationTimelock). Non-adverse changes take effect immediately.
//
// Determinism: the mode comes from the operator's own authenticated Hive
// `update_account` announcement (json_metadata → witness record), which every
// node ingests identically from L1. Reading it at a fixed blockHeight therefore
// yields the same answer on every node, which is required because this gates
// consensus_stake acceptance and the settlement reward split.
//
// Account form: witness records are keyed by the bare Hive account name, while
// callers pass the normalized "hive:" form used throughout the ledger; the
// prefix is stripped before the lookup.
func (se *StateEngine) NodeDelegationMode(account string, blockHeight uint64) string {
	if se == nil || se.witnessDb == nil || account == "" {
		return delegationmode.Default
	}
	bare := strings.TrimPrefix(account, "hive:")
	// Fail-stop the witness read: a transient DB error must NOT silently resolve
	// to Default (which would gate stake acceptance / the settlement split
	// differently on a node with a blip vs its peers). Only a genuine absence
	// (nil) falls through to Default — strict opt-in.
	w := se.getWitnessAtHeightOrBlock(bare, blockHeight)
	if w == nil {
		return delegationmode.Default
	}
	return se.resolveDelegationMode(w, blockHeight)
}

// effectiveDelegationMode is the PURE timelock resolution: given the witness row,
// the chain epoch at the read height, and whether the 0.5.0 gate is active there,
// it returns the mode in force. A zero maturity (pre-0.5.0 / non-adverse rows), an
// inactive gate, or an already-matured epoch all yield the announced target;
// while an adverse downgrade is still pending it returns the protected effective
// mode. The two callers differ only in HOW they obtain the epoch/gate — the
// consensus path fail-stops, the API path is best-effort — so the resolution
// itself lives here once.
func effectiveDelegationMode(w *witnesses.Witness, currentEpoch uint64, gateActive bool) string {
	target := delegationmode.Normalize(w.DelegationMode)
	if w.DelegationModeMaturityEpoch == 0 || !gateActive || currentEpoch >= w.DelegationModeMaturityEpoch {
		return target
	}
	return delegationmode.Normalize(w.DelegationModeEffective) // still protected
}

// resolveDelegationMode maps a witness row to the mode in force at blockHeight for
// the CONSENSUS paths (stake acceptance, settlement split). It resolves the epoch
// via the fail-stop GetElectionInfoOrBlock — the same read the unbond release uses
// — so every node agrees or halts, never a silent per-node fork. A zero maturity
// skips the election read entirely (byte-identical to the pre-feature read).
func (se *StateEngine) resolveDelegationMode(w *witnesses.Witness, blockHeight uint64) string {
	if w.DelegationModeMaturityEpoch == 0 {
		return delegationmode.Normalize(w.DelegationMode)
	}
	elec, found := se.GetElectionInfoOrBlock(blockHeight)
	gateActive := found && DelegatedStakeActiveForElection(elec)
	return effectiveDelegationMode(w, elec.Epoch, gateActive)
}

// NodeDelegationModeBestEffort is the NON-BLOCKING, API-facing read of the
// effective delegation mode. Unlike NodeDelegationMode (fail-stop, for consensus
// paths), it never blocks and never fabricates a value: it returns ok=false when
// the mode cannot be determined — no announcement exists, or a witness/election
// read fails transiently — so the caller can surface null ("not found") instead
// of hanging a request or reporting a wrong default. Only ok=true carries a real,
// resolved mode.
func (se *StateEngine) NodeDelegationModeBestEffort(account string, blockHeight uint64) (string, bool) {
	if se == nil || se.witnessDb == nil || account == "" {
		return "", false
	}
	bare := strings.TrimPrefix(account, "hive:")
	bh := blockHeight
	w, err := se.witnessDb.GetWitnessAtHeight(bare, &bh)
	if err != nil || w == nil {
		return "", false // read failed OR no announcement → no result to report
	}
	if w.DelegationModeMaturityEpoch == 0 {
		return delegationmode.Normalize(w.DelegationMode), true
	}
	// Pending downgrade: resolve the epoch with a plain (non-blocking) election
	// read. Any error (including no election at this height) → don't guess.
	elec, err := se.electionDb.GetElectionByHeight(blockHeight)
	if err != nil {
		return "", false
	}
	return effectiveDelegationMode(w, elec.Epoch, DelegatedStakeActiveForElection(elec)), true
}

// computeDelegationTimelock resolves the (effective, maturityEpoch) pair to store
// on a witness announcement row at height Hn for `announced`, enforcing the
// downgrade timelock. It is called at ingest (state engine, before
// SetWitnessUpdate) and returns zero-values (no fields persisted) for pre-0.5.0
// history and for every non-adverse change, keeping those rows byte-clean.
//
// Determinism/replay: reads only on-chain, height-addressable state — the
// election at Hn (fail-stop) and the immediately-preceding witness row (always
// within the 6-record retention window, since it is the newest row < Hn). The
// prior row carries the current effective/pending state forward, so pruning the
// original adverse-announce row never loses it, and re-announcing the same
// pending downgrade carries the original maturity forward WITHOUT resetting it.
func (se *StateEngine) computeDelegationTimelock(account string, Hn uint64, announced string) (string, uint64) {
	target := delegationmode.Normalize(announced)

	// Gate + epoch from one fail-stop election read. Below 0.5.0 (or pre-genesis)
	// store zero-values → readers see the announced mode immediately.
	elec, found := se.GetElectionInfoOrBlock(Hn)
	if !found || !DelegatedStakeActiveForElection(elec) {
		return "", 0
	}
	epoch := elec.Epoch

	bare := strings.TrimPrefix(account, "hive:")
	// FAIL-STOP: a swallowed error here would flip the timelock decision (prev=nil
	// → treated as no prior mode → an adverse downgrade stored as immediate) and
	// PERSIST that divergence on this node's witness row, forking it from peers
	// until a full reindex. Block on infra errors; ErrNoDocuments → nil (no prior).
	prev := se.getWitnessAtHeightOrBlock(bare, Hn) // strict height < Hn → prior row

	// Mode actually in force at Hn (respecting any still-pending prior downgrade).
	effNow := target
	pPending := false
	pTarget := ""
	if prev != nil {
		pTarget = delegationmode.Normalize(prev.DelegationMode)
		if prev.DelegationModeMaturityEpoch != 0 && epoch < prev.DelegationModeMaturityEpoch {
			pPending = true
			effNow = delegationmode.Normalize(prev.DelegationModeEffective) // still-protected mode
		} else {
			effNow = pTarget // prior downgrade matured, or none was pending
		}
	}

	if delegationmode.IsAdverseTransition(effNow, target) {
		if pPending && pTarget == target {
			// Re-announcement of the same still-pending downgrade → carry the
			// original maturity (NO reset), else a periodic re-announce would
			// push the timer out forever.
			return effNow, prev.DelegationModeMaturityEpoch
		}
		// New distinct adverse transition → start the timer from the current epoch.
		return effNow, epoch + params.DELEGATION_DOWNGRADE_LOCK_EPOCHS
	}
	// Non-adverse (→ Share, lateral, or a return to the protected mode): effective
	// immediately, cancelling any pending downgrade. Zero maturity ⇒ no fields
	// persisted and readers return Normalize(DelegationMode) == target.
	return "", 0
}

// PendulumShareDelegations returns, for every committee `member` (in "hive:"
// form) running delegationmode.Share at blockHeight, that node's positive stake
// edges: node -> (delegator -> net HIVE staked). Nodes in any other mode, or
// with no edges, are omitted. The result feeds settlement.ComposeRecord, which
// splits each share node's pendulum slice pro-rata across these edges.
//
// Determinism: edges come from LedgerSystem.AllDelegationEdges (a single ledger
// scan) and modes from the witness DB — both identical on every node at a fixed
// blockHeight. The producer, the apply-time re-derivation, and the structural
// validator MUST all call this with the SAME blockHeight (the settlement's
// SnapshotRangeTo) so they agree byte-for-byte.
//
// Returns ok=false when the edge scan fails transiently, so the producer can
// abort the election attempt and re-derivation can fall back to structural
// validation — never distribute against a partial view.
func (se *StateEngine) PendulumShareDelegations(members []string, blockHeight uint64) (map[string]map[string]int64, bool) {
	if se == nil || se.LedgerSystem == nil {
		return nil, false
	}
	allEdges, ok := se.LedgerSystem.AllDelegationEdges(blockHeight)
	if !ok {
		return nil, false
	}
	out := make(map[string]map[string]int64)
	for _, m := range members {
		if !delegationmode.SharesRewards(se.NodeDelegationMode(m, blockHeight)) {
			continue
		}
		edges := allEdges[m]
		if len(edges) == 0 {
			continue
		}
		// Copy so the returned map never aliases AllDelegationEdges' internal
		// state, and drop any non-positive edge defensively.
		cp := make(map[string]int64, len(edges))
		for d, s := range edges {
			if s > 0 {
				cp[d] = s
			}
		}
		if len(cp) > 0 {
			out[m] = cp
		}
	}
	return out, true
}
