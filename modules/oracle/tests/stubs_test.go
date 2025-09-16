package tests

import (
	"context"
	"testing"
	"time"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/oracle/p2p"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	"vsc-node/modules/vstream"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
)

var (
	conf       common.IdentityConfig
	electionDb elections.Elections
	witnessDb  witnesses.Witnesses
)

type stubP2pServer struct {
	*libp2p.P2PServer
	t *testing.T
}

func (s *stubP2pServer) SendToAll(topic string, message []byte) {
	assert.Equal(s.t, p2p.OracleTopic, topic)
}

type stubOracleP2pSpec struct {
	*p2p.OracleP2pSpec
	t *testing.T
}

func (p *stubOracleP2pSpec) Initialize(
	broadcastPriceChan chan<- []p2p.AveragePricePoint,
	priceBlockSignatureChan chan<- p2p.OracleBlock,
	broadcastPriceBlockSignatureChan chan<- p2p.OracleBlock,
) {
	p.OracleP2pSpec.Initialize(
		broadcastPriceChan,
		priceBlockSignatureChan,
		broadcastPriceBlockSignatureChan,
	)
}

func (p *stubOracleP2pSpec) HandleMessage(
	ctx context.Context,
	from peer.ID,
	msg p2p.Msg,
	send libp2p.SendFunc[p2p.Msg],
) error {
	return nil
}

type stubVStream struct {
	funck  vstream.BTFunc
	ticker *time.Ticker
	ctx    context.Context
}

func (s *stubVStream) RegisterBlockTick(_ string, f vstream.BTFunc, _ bool) {
	s.funck = f
}

func (s *stubVStream) Start() {
	bh := uint64(0)
	blockHeight := uint64(0)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.ticker.C:
			s.funck(bh, &blockHeight)
			blockHeight += 1
		}
	}
}

type stubBlockScheduler struct{}

func (s *stubBlockScheduler) GetSchedule(
	slotHeight uint64,
) []stateEngine.WitnessSlot {
	out := []stateEngine.WitnessSlot{}
	return out
}
