package cbortypes

import (
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"

	cbornode "github.com/ipfs/go-ipld-cbor"
)

func RegisterTypes() {
	cbornode.RegisterCborType(vscBlocks.VscBlock{})
	cbornode.RegisterCborType(vscBlocks.VscHeader{})
	cbornode.RegisterCborType(vscBlocks.VscBlockTx{})
	cbornode.RegisterCborType(struct {
		Br    [2]int  "refmt:\"br\""
		Prevb *string "refmt:\"prevb\""
	}{})
	cbornode.RegisterCborType(struct {
		Prevb *string "refmt:\"prevb\""
	}{})
}
