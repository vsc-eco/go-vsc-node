package blockproducer

import (
	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	stateEngine "vsc-node/modules/state-processing"
	"vsc-node/modules/vstream"

	"github.com/chebyrash/promise"
)

var CONSENSUS_SPECS = common.CONSENSUS_SPECS

type BlockProducer struct {
	a.Plugin

	StateEngine *stateEngine.StateEngine
	VStream     *vstream.VStream
}

func (bp *BlockProducer) BlockTick(bh uint64) {
	if bh%CONSENSUS_SPECS.SlotLength == 0 {
		bp.ProduceBlock()
	}
}

func (bp *BlockProducer) ProduceBlock() {

}

func (bp *BlockProducer) Init() error {
	bp.VStream.RegisterBlockTick("block-producer", bp.BlockTick, false)
	return nil
}

func (bp *BlockProducer) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

func (bp *BlockProducer) Stop() error {
	return nil
}

func New(vstream *vstream.VStream, se *stateEngine.StateEngine) *BlockProducer {
	return &BlockProducer{
		StateEngine: se,
		VStream:     vstream,
	}
}
