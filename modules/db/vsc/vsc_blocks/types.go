package vscBlocks

import (
	"time"
	"vsc-node/modules/aggregate"

	"github.com/ipfs/go-cid"
)

type VscBlocks interface {
	aggregate.Plugin
	StoreHeader(header VscHeaderRecord)
	GetBlockByHeight(height uint64) (*VscHeaderRecord, error)
	GetBlockById(id string) (*VscHeaderRecord, error)
}

type VscHeaderRecord struct {
	Id string `bson:"id"`
	//CID of content
	BlockContent string   `bson:"block"`
	EndBlock     int      `bson:"end_block"`
	MerkleRoot   string   `bson:"merkle_root"`
	Proposer     string   `bson:"proposer"`
	SigRoot      string   `bson:"sig_root"`
	Signers      []string `bson:"signers"`
	SlotHeight   int      `bson:"slot_height"`
	StartBlock   int      `bson:"start_block"`
	Stats        struct {
		Size uint64 `bson:"size"`
	} `bson:"stats"`
	Ts time.Time `bson:"ts"`
}

type VscBlock struct {
	Transactions []VscBlockTx `refmt:"txs"`
	Headers      struct {
		Prevb *string `refmt:"prevb"`
	} `refmt:"headers"`
	MerkleRoot *string `refmt:"merkle_root"`
	SigRoot    *string `refmt:"sig_root"`
}

type VscBlockTx struct {
	Id   string  `refmt:"id"`
	Op   *string `refmt:"op"`
	Type int     `refmt:"type"`
}

// VSC Block header which goes on chain
// This would be signed by BLS consensus
type VscHeader struct {
	Version string `refmt:"__v"`
	Type    string `refmt:"__t"`
	Headers struct {
		Br    [2]int `refmt:"br"`
		Prevb *string
	} `refmt:"headers"`
	MerkleRoot *string `refmt:"merkle_root"`
	Block      cid.Cid `refmt:"block"`
}
