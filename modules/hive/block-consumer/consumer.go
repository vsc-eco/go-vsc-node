package blockconsumer

import (
	"vsc-node/lib/utils"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/db/vsc/hive_blocks"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/chebyrash/promise"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	processedHeight = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "magi", Subsystem: "hive",
		Name: "processed_height",
		Help: "Most recent Hive block height processed by this node",
	})
	headHeightGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "magi", Subsystem: "hive",
		Name: "head_height",
		Help: "Most recent Hive head block height observed by this node",
	})
)

//VSC Block streaming module

type HiveConsumer struct {
	ticks map[string]*BlockTick

	StateEngine *stateEngine.StateEngine

	bh uint64
	hh uint64

	headFetcher HeadHeightGetter
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
		headHeightGauge.Set(float64(*headHeight))
	}
	for _, tick := range v.ticks {
		if tick.async {
			go tick.funck(blk.BlockNumber, headHeight)
		} else {
			tick.funck(blk.BlockNumber, headHeight)
		}
	}
	if v.StateEngine != nil {
		v.StateEngine.ProcessBlock(blk)
	}
	v.bh = blk.BlockNumber
	processedHeight.Set(float64(blk.BlockNumber))
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
