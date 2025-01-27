package vscBlocks

import (
	"time"
	"vsc-node/modules/aggregate"
)

type VscBlocks interface {
	aggregate.Plugin
	StoreHeader(header VscHeader)
}

type VscHeader struct {
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
