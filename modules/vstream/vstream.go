package vstream

import (
	"vsc-node/lib/utils"
	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/chebyrash/promise"
)

//VSC Block streaming module

type VStream struct {
	ticks map[string]*BlockTick
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

func (v *VStream) ProcessBlock(blk hive_blocks.HiveBlock) {

}

type BlockTick struct {
	funck BTFunc
	async bool
}

type BTFunc func(bh uint64)

func New() *VStream {
	return &VStream{
		ticks: make(map[string]*BlockTick),
	}
}
