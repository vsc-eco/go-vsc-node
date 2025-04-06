package vstream

import (
	"vsc-node/lib/utils"
	"vsc-node/modules/db/vsc/hive_blocks"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/chebyrash/promise"
)

//VSC Block streaming module

type VStream struct {
	ticks map[string]*BlockTick

	StateEngine *stateEngine.StateEngine
}

//Make a module!

func (v *VStream) Init() error {
	return nil
}

func (v *VStream) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

func (v *VStream) Stop() error {
	return nil
}

func (v *VStream) RegisterBlockTick(name string, funck BTFunc, async bool) {
	v.ticks[name] = &BlockTick{
		funck: funck,
		async: async,
	}
}

func (v *VStream) ProcessBlock(blk hive_blocks.HiveBlock, headHeight uint64) {
	for _, tick := range v.ticks {
		if tick.async {
			go tick.funck(blk.BlockNumber, headHeight)
		} else {
			tick.funck(blk.BlockNumber, headHeight)
		}
	}
	v.StateEngine.ProcessBlock(blk)
}

type BlockTick struct {
	funck BTFunc
	async bool
}

type BTFunc func(bh uint64, headHeight uint64)

func New(se *stateEngine.StateEngine) *VStream {
	return &VStream{
		StateEngine: se,
		ticks:       make(map[string]*BlockTick),
	}
}
