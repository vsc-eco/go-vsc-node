package transactionpool

import (
	"context"
	"encoding/json"
	"vsc-node/modules/common"
	libp2p "vsc-node/modules/p2p"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

type p2pSpec struct {
	conf common.IdentityConfig

	txp *TransactionPool
}

type p2pMessage struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

// ValidateMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) ValidateMessage(ctx context.Context, from peer.ID, msg *pubsub.Message, parsedMsg p2pMessage) bool {
	// Can add a blacklist for spammers or ignore previously seen messages.
	return true
}

func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg p2pMessage, send libp2p.SendFunc[p2pMessage]) error {
	if msg.Type == "announce_tx" {
		s.txp.ReceiveTx(msg)
	}

	return nil
}

func (p2pSpec) HandleRawMessage(ctx context.Context, rawMsg *pubsub.Message, send libp2p.SendFunc[p2pMessage]) error {
	// Not typically necessary to implement this method.
	return nil
}

func (p2pSpec) ParseMessage(data []byte) (p2pMessage, error) {
	res := p2pMessage{}
	json.Unmarshal(data, &res)
	return res, nil
}

func (p2pSpec) SerializeMessage(msg p2pMessage) []byte {
	ff, _ := json.Marshal(msg)
	return ff
}

func (p2pSpec) Topic() string {
	return "/vsc/mainnet/txpool/v1"
}

var _ libp2p.PubSubServiceParams[p2pMessage] = p2pSpec{}

type p2pService struct {
}

func (txp *TransactionPool) startP2P() error {
	service, err := libp2p.NewPubSubService(txp.p2p, p2pSpec{
		txp: txp,
	})

	if err != nil {
		return err
	}
	txp.service = service

	return nil
}

func (txp *TransactionPool) stopP2P() error {

	return nil
}
