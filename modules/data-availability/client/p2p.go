package data_availability_client

import (
	"context"
	"fmt"
	"sync/atomic"
	data_availability_spec "vsc-node/modules/data-availability/spec"
	libp2p "vsc-node/modules/p2p"
	"weak"

	"github.com/libp2p/go-libp2p/core/peer"
)

type p2pSpec struct {
	data_availability_spec.P2pSpec
	weak.Pointer[DataAvailability]
}

type p2pMessage = data_availability_spec.P2pMessage

var _ libp2p.PubSubServiceParams[p2pMessage] = p2pSpec{}

func (d *DataAvailability) startP2P() error {
	var err error
	d.service, err = libp2p.NewPubSubService(d.p2p, p2pSpec{data_availability_spec.New(d.conf, d.dl), weak.Make(d)})
	return err
}

func (d *DataAvailability) stopP2P() error {
	if d.service == nil {
		return nil
	}
	return d.service.Close()
}

var count = atomic.Int32{}

// HandleMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg p2pMessage, send libp2p.SendFunc[p2pMessage]) error {
	fmt.Println("client message count:", count.Add(1))
	switch msg.Type() {
	case data_availability_spec.P2pMessageSignature:
		d := s.Value()
		if d == nil || d.circuit == nil {
			return nil
		}
		return d.circuit.AddAndVerifyRaw("", msg.Data())
	}
	return nil
}
