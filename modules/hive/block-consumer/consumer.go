package blockconsumer

import (
	"sync/atomic"
	"vsc-node/lib/utils"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/db/vsc/hive_blocks"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/chebyrash/promise"
)

//VSC Block streaming module

type HiveConsumer struct {
	ticks map[string]*BlockTick

	StateEngine *stateEngine.StateEngine

	bh uint64
	hh uint64

	headFetcher HeadHeightGetter

	// processed and asyncTicks back the devnet block pacemaker's quiescence
	// probe (ProcessedHeight / AsyncTicksInFlight). They carry no consensus state.
	processed  atomic.Uint64
	asyncTicks atomic.Int64
}

//Make a module!

func (v *HiveConsumer) Init() error {
	return nil
}

func (v *HiveConsumer) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

func (v *HiveConsumer) Stop() error {
	return nil
}

func (v *HiveConsumer) RegisterBlockTick(name string, funck BTFunc, async bool) {
	v.ticks[name] = &BlockTick{
		funck: funck,
		async: async,
	}
}

func (v *HiveConsumer) ProcessBlock(blk hive_blocks.HiveBlock, headHeight *uint64) {
	if headHeight != nil {
		v.hh = *headHeight
	}
	for _, tick := range v.ticks {
		if tick.async {
			v.asyncTicks.Add(1)
			go func(funck BTFunc) {
				defer v.asyncTicks.Add(-1)
				funck(blk.BlockNumber, headHeight)
			}(tick.funck)
		} else {
			tick.funck(blk.BlockNumber, headHeight)
		}
	}
	if v.StateEngine != nil {
		v.StateEngine.ProcessBlock(blk)
	}
	v.bh = blk.BlockNumber
	v.processed.Store(blk.BlockNumber)
}

// ProcessedHeight is the last Hive block whose sync ticks and state-engine pass
// have completed on this node.
func (v *HiveConsumer) ProcessedHeight() uint64 {
	return v.processed.Load()
}

// AsyncTicksInFlight counts async block ticks (TSS, elections, oracle) that have
// been dispatched and not yet returned.
func (v *HiveConsumer) AsyncTicksInFlight() int64 {
	return v.asyncTicks.Load()
}

func (v *HiveConsumer) BlockStatus() common_types.BlockStatusGetter {
	return &blockGetter{
		v,
	}
}

type blockGetter struct {
	*HiveConsumer
}

func (b *blockGetter) HeadHeight() *uint64 {
	if b.headFetcher != nil {
		h := b.headFetcher()
		return &h
	}
	if b.hh > 0 {
		h := b.hh
		return &h
	}
	return nil
}
func (b *blockGetter) BlockHeight() uint64 {
	return b.bh
}

type HeadHeightGetter func() uint64

type BlockTick struct {
	funck BTFunc
	async bool
}

type BTFunc func(bh uint64, headHeight *uint64)

func New(se *stateEngine.StateEngine) *HiveConsumer {
	return &HiveConsumer{
		StateEngine: se,
		ticks:       make(map[string]*BlockTick),
	}
}
