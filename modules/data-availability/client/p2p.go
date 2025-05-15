package data_availability_client

import (
	"context"
	"fmt"
	"slices"
	"vsc-node/lib/dids"
	"vsc-node/lib/utils"
	data_availability_spec "vsc-node/modules/data-availability/spec"
	libp2p "vsc-node/modules/p2p"
	"weak"

	"github.com/chebyrash/promise"
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

// var count = atomic.Int32{}
// var sigCount = atomic.Int32{}

// HandleMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg p2pMessage, send libp2p.SendFunc[p2pMessage]) error {
	// fmt.Println("client message count:", count.Add(1), msg.Type().String())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	switch msg.Type() {
	case data_availability_spec.P2pMessageSignature:
		d := s.Value()
		if d == nil || d.circuit == nil {
			return nil
		}

		added, err := promise.All(ctx, utils.Map(d.circuit.CircuitMap(), func(member dids.BlsDID) *promise.Promise[bool] {
			return promise.New(func(resolve func(bool), reject func(error)) {
				added, err := d.circuit.AddAndVerifyRaw(member, msg.Data())
				if err != nil {
					reject(err)
				}
				resolve(added)
			})
		})...).Await(ctx)

		if err != nil {
			return err
		}

		if !slices.Contains(*added, true) {
			return fmt.Errorf("message did not add any signatures")
		}

		// fmt.Println("client message sig count:", sigCount.Add(1), msg.Type().String())

		return nil
	}
	return nil
}
