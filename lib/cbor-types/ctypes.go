package cbortypes

import (
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"

	cbornode "github.com/ipfs/go-ipld-cbor"
)

func RegisterTypes() {
	cbornode.RegisterCborType(vscBlocks.VscBlock{})
	cbornode.RegisterCborType(vscBlocks.VscHeader{})
}
