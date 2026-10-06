package haf

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	"github.com/vsc-eco/hivego"
)

// timestampFormat matches the timestamp the Hive API block streamer stores,
// which graphql timestamp joins and the state engine consume verbatim.
const timestampFormat = "2006-01-02T15:04:05"

// zeroTrxId mirrors the all-zero transaction id the account history API
// reports for virtual operations generated outside any transaction.
const zeroTrxId = "0000000000000000000000000000000000000000"

type opBody struct {
	Type  string                 `json:"type"`
	Value map[string]interface{} `json:"value"`
}

// AssembleBlocks merges HAF header and operation rows into HiveBlock values.
// Every header yields a block (the state engine must tick empty heights too);
// a block's Transactions only contain transactions with at least one
// operation passing filters — all operations of such a transaction are kept.
//
// The fetcher delivers operation rows pre-ordered by
// (block_num, trx_in_block, op_pos_real): real operations (op_pos_real IS NOT
// NULL) arrive in their dense, consensus position within each transaction,
// and virtual operations (op_pos_real IS NULL) sort to the tail. Virtual
// operations are collected separately and attached only when the block
// qualifies for virtual-op capture (see BlockParams.NeedsVirtualOps), then
// gated by vFilters.
func AssembleBlocks(
	headers []BlockRow,
	ops []OpRow,
	filters []streamer.FilterFunc,
	vFilters []streamer.VirtualFilterFunc,
) ([]hive_blocks.HiveBlock, error) {
	blocks := make([]hive_blocks.HiveBlock, 0, len(headers))
	opsByBlock := map[uint64][]OpRow{}
	for _, op := range ops {
		opsByBlock[op.BlockNum] = append(opsByBlock[op.BlockNum], op)
	}

	for _, h := range headers {
		block, err := assembleBlock(h, opsByBlock[h.Num], filters, vFilters)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func assembleBlock(
	h BlockRow,
	ops []OpRow,
	filters []streamer.FilterFunc,
	vFilters []streamer.VirtualFilterFunc,
) (hive_blocks.HiveBlock, error) {
	block := hive_blocks.HiveBlock{
		BlockNumber:  h.Num,
		BlockID:      hex.EncodeToString(h.Hash),
		Witness:      h.ProducerName,
		Timestamp:    h.CreatedAt.UTC().Format(timestampFormat),
		MerkleRoot:   hex.EncodeToString(h.MerkleRoot),
		Transactions: []hive_blocks.Tx{},
	}

	// group real operations per transaction; virtual ones aside
	type trxGroup struct {
		index int32
		hash  []byte
		ops   []OpRow
	}
	trxOrder := []int32{}
	trxGroups := map[int32]*trxGroup{}
	virtualOps := []OpRow{}
	for _, row := range ops {
		if row.OpPosReal == nil {
			virtualOps = append(virtualOps, row)
			continue
		}
		g, ok := trxGroups[row.TrxInBlock]
		if !ok {
			g = &trxGroup{index: row.TrxInBlock, hash: row.TrxHash}
			trxGroups[row.TrxInBlock] = g
			trxOrder = append(trxOrder, row.TrxInBlock)
		}
		if len(g.hash) == 0 && len(row.TrxHash) > 0 {
			g.hash = row.TrxHash
		}
		g.ops = append(g.ops, row)
	}

	needsVirtualOps := false
	for _, idx := range trxOrder {
		g := trxGroups[idx]
		tx := hive_blocks.Tx{
			Index:         int(g.index),
			TransactionID: hex.EncodeToString(g.hash),
			Operations:    make([]hivego.Operation, 0, len(g.ops)),
		}
		shouldInclude := false
		for _, row := range g.ops {
			op, err := parseRealOp(row)
			if err != nil {
				return hive_blocks.HiveBlock{}, err
			}
			// Mirror the Hive API streamer: every operation of an included
			// transaction is kept, and each filter call sees a fresh
			// BlockParams so a non-matching filter cannot leak state.
			for _, filter := range filters {
				blockParams := &streamer.BlockParams{
					NeedsVirtualOps: false,
					BlockHeight:     h.Num,
				}
				if filter(op, blockParams) {
					if blockParams.NeedsVirtualOps {
						needsVirtualOps = true
					}
					shouldInclude = true
					break
				}
			}
			tx.Operations = append(tx.Operations, op)
		}
		if shouldInclude {
			block.Transactions = append(block.Transactions, tx)
		}
	}

	// Virtual-op capture parity: the API streamer only pulls virtual ops when
	// a transaction in the block asked for them (savings movements above the
	// claim start height), and then keeps only what vFilters accept.
	if needsVirtualOps && len(virtualOps) > 0 {
		filtered := []hivego.VirtualOp{}
		for _, row := range virtualOps {
			op, err := parseVirtualOp(h, row)
			if err != nil {
				return hive_blocks.HiveBlock{}, err
			}
			for _, vFilter := range vFilters {
				if vFilter(op) {
					filtered = append(filtered, op)
					break
				}
			}
		}
		if len(filtered) > 0 {
			block.VirtualOps = filtered
		}
	}

	return block, nil
}

// parseRealOp decodes a stored operation body into the hivego form the state
// engine consumes, with the "_operation" type suffix stripped exactly as the
// Hive API streamer does.
func parseRealOp(row OpRow) (hivego.Operation, error) {
	body, err := parseBody(row)
	if err != nil {
		return hivego.Operation{}, err
	}
	return hivego.Operation{
		Type:  stripOpSuffix(body.Type),
		Value: body.Value,
	}, nil
}

// parseVirtualOp decodes a virtual operation body. Unlike real operations the
// type keeps its "_operation" suffix (matching account_history_api output and
// the interest_operation dispatch in the state engine).
func parseVirtualOp(h BlockRow, row OpRow) (hivego.VirtualOp, error) {
	body, err := parseBody(row)
	if err != nil {
		return hivego.VirtualOp{}, err
	}
	trxId := hex.EncodeToString(row.TrxHash)
	if trxId == "" {
		trxId = zeroTrxId
	}
	return hivego.VirtualOp{
		Block: int(h.Num),
		Op: struct {
			Type  string                 `json:"type"`
			Value map[string]interface{} `json:"value"`
		}{
			Type:  body.Type,
			Value: body.Value,
		},
		TrxId:      trxId,
		TrxInBlock: int(row.TrxInBlock),
		VirtualOp:  true,
		Timestamp:  h.CreatedAt.UTC().Format(timestampFormat),
	}, nil
}

func parseBody(row OpRow) (opBody, error) {
	var body opBody
	if err := json.Unmarshal(row.Body, &body); err != nil {
		return opBody{}, fmt.Errorf("haf: decode op body at block %d: %w", row.BlockNum, err)
	}
	return body, nil
}

func stripOpSuffix(t string) string {
	return strings.TrimSuffix(t, "_operation")
}
