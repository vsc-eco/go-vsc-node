package transactionpool

import (
	"encoding/json"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/transactions"

	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
)

func HashKeyAuths(keyAuths []string) string {
	if len(keyAuths) == 0 {
		return ""
	}
	if len(keyAuths) < 2 {
		return keyAuths[0]
	} else {
		keyMap := make(map[string]bool)

		for _, key := range keyAuths {
			keyMap[key] = true
		}

		dagBytes, _ := common.EncodeDagCbor(keyMap)

		cidz, _ := cid.Prefix{
			Version:  1,
			Codec:    uint64(multicodec.DagCbor),
			MhType:   multihash.SHA2_256,
			MhLength: -1,
		}.Sum(dagBytes)

		return cidz.String()
	}
}

func DecodeTxCbor(op VSCTransactionOp, input interface{}) error {
	node, _ := cbornode.Decode(op.Payload, multihash.SHA2_256, -1)
	jsonBytes, _ := node.MarshalJSON()

	return json.Unmarshal(jsonBytes, input)
}

// Per-op-type maximum RC costs, used by StaticMaxRcCost to decide whether
// a declared Headers.RcLimit can cover a multi-op tx's worst case. Values
// mirror the per-op success-path costs in state-processing/transactions.go
// and the block-producer fallback at block-producer/blockProducer.go:331.
// Kept in this file so they're accessible from both the ingestion gate
// here and the sequencing gate in block-producer.
const (
	RcCostTransfer uint64 = 100
	RcCostWithdraw uint64 = 200
	RcCostStake    uint64 = 200
	RcCostUnstake  uint64 = 200
	RcCostCallMin  uint64 = 100 // minimum floor; call ops declare their own
	RcCostUnknown  uint64 = 50
)

// callRcLimit decodes a call op's payload and returns its declared
// rc_limit. Returns RcCostCallMin if the field is missing or zero so the
// cap can't be trivially bypassed by a malformed op.
func callRcLimit(op VSCTransactionOp) uint64 {
	var payload struct {
		RcLimit uint64 `json:"rc_limit"`
	}
	if err := DecodeTxCbor(op, &payload); err != nil {
		return RcCostCallMin
	}
	if payload.RcLimit < RcCostCallMin {
		return RcCostCallMin
	}
	return payload.RcLimit
}

// StaticMaxRcCost returns the maximum possible total RC consumption for a
// list of offchain ops. Used to reject multi-op txs whose declared
// Headers.RcLimit cannot cover the worst-case cost, enforcing:
//
//	Σ(per-op max cost) ≤ Headers.RcLimit
//
// For `call` ops it uses the per-op rc_limit decoded from the payload (the
// WASM gas ceiling). For fixed-cost ops it uses the success-path RcUsed
// returned by state-processing's per-type ExecuteTx methods.
func StaticMaxRcCost(ops []VSCTransactionOp) uint64 {
	var total uint64
	for _, op := range ops {
		switch op.Type {
		case "call":
			total += callRcLimit(op)
		case "transfer":
			total += RcCostTransfer
		case "withdraw":
			total += RcCostWithdraw
		case "stake_hbd":
			total += RcCostStake
		case "unstake_hbd":
			total += RcCostUnstake
		default:
			total += RcCostUnknown
		}
	}
	return total
}

// StaticMaxRcCostFromRecord is the sequencing-layer analogue of
// StaticMaxRcCost that works on a block producer's TransactionRecord ops.
// The record's per-op Data map carries the decoded rc_limit for call ops
// (see TxVscCallContract.ToData).
func StaticMaxRcCostFromRecord(ops []transactions.TransactionOperation) uint64 {
	var total uint64
	for _, op := range ops {
		switch op.Type {
		case "call":
			total += recordCallRcLimit(op)
		case "transfer":
			total += RcCostTransfer
		case "withdraw":
			total += RcCostWithdraw
		case "stake_hbd":
			total += RcCostStake
		case "unstake_hbd":
			total += RcCostUnstake
		default:
			total += RcCostUnknown
		}
	}
	return total
}

// recordCallRcLimit extracts the call op's rc_limit from the Data map.
// The field is stored as a uint by TxVscCallContract.ToData but may be
// round-tripped through BSON/JSON as int32/int64/float64, so handle all.
// Missing or malformed field falls back to the minimum floor.
func recordCallRcLimit(op transactions.TransactionOperation) uint64 {
	raw, ok := op.Data["rc_limit"]
	if !ok {
		return RcCostCallMin
	}
	var v uint64
	switch n := raw.(type) {
	case uint:
		v = uint64(n)
	case uint32:
		v = uint64(n)
	case uint64:
		v = n
	case int:
		if n > 0 {
			v = uint64(n)
		}
	case int32:
		if n > 0 {
			v = uint64(n)
		}
	case int64:
		if n > 0 {
			v = uint64(n)
		}
	case float64:
		if n > 0 {
			v = uint64(n)
		}
	default:
		return RcCostCallMin
	}
	if v < RcCostCallMin {
		return RcCostCallMin
	}
	return v
}
