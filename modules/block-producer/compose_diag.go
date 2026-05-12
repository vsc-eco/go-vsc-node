package blockproducer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// composeInputDigest summarises the state-engine in-memory inputs that
// GenerateBlock reads from when composing a block. The expectation: two
// nodes that have processed the same on-chain history up to the same
// slotHeight produce identical digests. If digests differ, the
// state-engine in-memory state has diverged and that's why the block
// CIDs don't match.
//
// All four sub-digests are hex-encoded SHA-256 of a deterministic byte
// sequence (sorted by stable keys, primitive fields concatenated with
// length-prefixed separators).
type composeInputDigest struct {
	OplogHash           string // LedgerState.Oplog contents
	TxOutHash           string // TxOutIds + TxOutput map
	TempOutputsHash     string // TempOutputs map (per-contract cache, deletions, base CID)
	ContractResultsHash string // ContractResults map
}

func (d composeInputDigest) String() string {
	return fmt.Sprintf("oplog=%s txout=%s outputs=%s results=%s",
		d.OplogHash, d.TxOutHash, d.TempOutputsHash, d.ContractResultsHash)
}

// snapshotComposeInputs walks the live state-engine accumulators and
// builds the digest. Read-only; safe to call from GenerateBlock without
// disturbing the state.
func (bp *BlockProducer) snapshotComposeInputs() composeInputDigest {
	if bp == nil || bp.StateEngine == nil {
		return composeInputDigest{}
	}

	return composeInputDigest{
		OplogHash:           hashLedgerOplog(bp),
		TxOutHash:           hashTxOutputs(bp),
		TempOutputsHash:     hashTempOutputs(bp),
		ContractResultsHash: hashContractResults(bp),
	}
}

func hashLedgerOplog(bp *BlockProducer) string {
	if bp.StateEngine.LedgerState == nil {
		return shortHex(sha256.Sum256(nil))
	}
	oplog := bp.StateEngine.LedgerState.Oplog
	// Sort by (BlockHeight, BIdx, OpIdx, Id) so we hash a canonical order.
	type indexed struct {
		bh, bi, oi int64
		id         string
		i          int
	}
	idx := make([]indexed, len(oplog))
	for i, ev := range oplog {
		idx[i] = indexed{int64(ev.BlockHeight), ev.BIdx, ev.OpIdx, ev.Id, i}
	}
	sort.Slice(idx, func(a, b int) bool {
		if idx[a].bh != idx[b].bh {
			return idx[a].bh < idx[b].bh
		}
		if idx[a].bi != idx[b].bi {
			return idx[a].bi < idx[b].bi
		}
		if idx[a].oi != idx[b].oi {
			return idx[a].oi < idx[b].oi
		}
		return idx[a].id < idx[b].id
	})

	h := sha256.New()
	fmt.Fprintf(h, "len=%d\n", len(oplog))
	for _, x := range idx {
		ev := oplog[x.i]
		fmt.Fprintf(h, "id=%s|t=%s|fr=%s|to=%s|am=%d|as=%s|memo=%s|bh=%d|bi=%d|oi=%d\n",
			ev.Id, ev.Type, ev.From, ev.To, ev.Amount, ev.Asset, ev.Memo,
			ev.BlockHeight, ev.BIdx, ev.OpIdx)
	}
	return shortHexBytes(h.Sum(nil))
}

func hashTxOutputs(bp *BlockProducer) string {
	ids := append([]string(nil), bp.StateEngine.TxOutIds...)
	sort.Strings(ids)
	h := sha256.New()
	fmt.Fprintf(h, "ids=%d outs=%d\n", len(ids), len(bp.StateEngine.TxOutput))
	for _, id := range ids {
		out := bp.StateEngine.TxOutput[id]
		ledgerIds := append([]string(nil), out.LedgerIds...)
		sort.Strings(ledgerIds)
		fmt.Fprintf(h, "%s|ok=%t|rc=%d|lids=%s\n", id, out.Ok, out.RcUsed, strings.Join(ledgerIds, ","))
	}
	return shortHexBytes(h.Sum(nil))
}

func hashTempOutputs(bp *BlockProducer) string {
	contractIds := make([]string, 0, len(bp.StateEngine.TempOutputs))
	for k := range bp.StateEngine.TempOutputs {
		contractIds = append(contractIds, k)
	}
	sort.Strings(contractIds)
	h := sha256.New()
	fmt.Fprintf(h, "contracts=%d\n", len(contractIds))
	for _, c := range contractIds {
		out := bp.StateEngine.TempOutputs[c]
		if out == nil {
			fmt.Fprintf(h, "%s|nil\n", c)
			continue
		}
		cacheKeys := make([]string, 0, len(out.Cache))
		for k := range out.Cache {
			cacheKeys = append(cacheKeys, k)
		}
		sort.Strings(cacheKeys)
		delKeys := make([]string, 0, len(out.Deletions))
		for k := range out.Deletions {
			delKeys = append(delKeys, k)
		}
		sort.Strings(delKeys)
		fmt.Fprintf(h, "%s|base=%s|del=%d|cache=%d\n", c, out.Cid, len(delKeys), len(cacheKeys))
		for _, k := range delKeys {
			fmt.Fprintf(h, "  d:%s=%t\n", k, out.Deletions[k])
		}
		for _, k := range cacheKeys {
			v := out.Cache[k]
			vh := sha256.Sum256(v)
			fmt.Fprintf(h, "  c:%s=%s\n", k, shortHexBytes(vh[:]))
		}
	}
	return shortHexBytes(h.Sum(nil))
}

func hashContractResults(bp *BlockProducer) string {
	contractIds := make([]string, 0, len(bp.StateEngine.ContractResults))
	for k := range bp.StateEngine.ContractResults {
		contractIds = append(contractIds, k)
	}
	sort.Strings(contractIds)
	h := sha256.New()
	fmt.Fprintf(h, "contracts=%d\n", len(contractIds))
	for _, c := range contractIds {
		results := bp.StateEngine.ContractResults[c]
		// ContractResults preserves execution order; the block is composed
		// in that order so DO NOT sort the slice — instead hash positionally.
		fmt.Fprintf(h, "%s|n=%d\n", c, len(results))
		for i, r := range results {
			errMsg := r.ErrMsg
			if r.Err != nil {
				errMsg = fmt.Sprintf("%s|%v", errMsg, *r.Err)
			}
			fmt.Fprintf(h, "  %d:tx=%s|ok=%t|ret=%s|err=%s|logs=%d|tss=%d\n",
				i, r.TxId, r.Success, r.Ret, errMsg, len(r.Logs), len(r.TssOps))
		}
	}
	return shortHexBytes(h.Sum(nil))
}

func shortHex(sum [32]byte) string { return shortHexBytes(sum[:]) }

func shortHexBytes(b []byte) string {
	// 16 hex chars (64 bits) is enough to spot divergence visually while
	// keeping log lines short.
	if len(b) > 8 {
		return hex.EncodeToString(b[:8])
	}
	return hex.EncodeToString(b)
}
