package blockproducer

import (
	"context"
	"encoding/json"
	"fmt"
	"vsc-node/modules/common"

	libp2p "vsc-node/modules/p2p"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

type p2pMessage struct {
	//Add more stuff later
	Type       string `json:"type"`
	SlotHeight uint64 `json:"block_height"`

	Data map[string]interface{} `json:"data"`
}

type p2pSpec struct {
	conf common.IdentityConfig
	bp   *BlockProducer
}

func (p2pSpec) ValidateMessage(ctx context.Context, from peer.ID, msg *pubsub.Message, parsedMsg p2pMessage) bool {
	// Can add a blacklist for spammers or ignore previously seen messages.
	return true
}

func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg p2pMessage, send libp2p.SendFunc[p2pMessage]) error {
	//Do something with the message
	if msg.Type == "block" {

		sig, err := s.bp.HandleBlockMsg(msg)

		if err != nil {
			return nil
		}

		send(p2pMessage{
			Type:       "block_sig",
			SlotHeight: msg.SlotHeight,

			Data: map[string]interface{}{
				"sig":     sig,
				"account": s.conf.Config.Get().HiveUsername,
			},
		})
	}

	if msg.Type == "block_sig" {

		if s.bp.sigChannels[msg.SlotHeight] != nil {
			s.bp.sigChannels[msg.SlotHeight] <- msg
		}
	}
	return nil
}

func (p2pSpec) HandleRawMessage(ctx context.Context, rawMsg *pubsub.Message, send libp2p.SendFunc[p2pMessage]) error {
	// Not typically necessary to implement this method.
	return nil
}

// ParseMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) ParseMessage(data []byte) (p2pMessage, error) {

	var res p2pMessage
	if err := json.Unmarshal(data, &res); err != nil {
		return res, fmt.Errorf("failed to unmarshal message: %w", err)
	}
	return res, nil
}
func (p2pSpec) SerializeMessage(msg p2pMessage) []byte {
	jsonBytes, _ := json.Marshal(msg)
	return jsonBytes
}

func (p2pSpec) Topic() string {
	return "/vsc/mainet/block-producer/v1"
}

var _ libp2p.PubSubServiceParams[p2pMessage] = p2pSpec{}

func (bp *BlockProducer) startP2P() error {
	var err error
	bp.service, err = libp2p.NewPubSubService(bp.p2p, p2pSpec{
		conf: bp.config,
		bp:   bp,
	})
	return err
}

func (bp *BlockProducer) stopP2P() error {
	if bp.service == nil {
		return nil
	}
	return bp.service.Close()
}
