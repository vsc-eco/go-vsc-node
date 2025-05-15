package data_availability_client

import (
	"context"
	"errors"
	"fmt"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	data_availability_spec "vsc-node/modules/data-availability/spec"
	"vsc-node/modules/db/vsc/elections"
	libp2p "vsc-node/modules/p2p"
	start_status "vsc-node/modules/start-status"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/chebyrash/promise"
	"github.com/hasura/go-graphql-client"
)

type DataAvailability struct {
	p2p         *libp2p.P2PServer
	service     libp2p.PubSubService[p2pMessage]
	conf        common.IdentityConfig
	dl          *datalayer.DataLayer
	circuit     dids.PartialBlsCircuit
	startStatus start_status.StartStatus
}

var _ a.Plugin = (*DataAvailability)(nil)

func New(p2p *libp2p.P2PServer, conf common.IdentityConfig, dl *datalayer.DataLayer) *DataAvailability {
	return &DataAvailability{
		p2p:         p2p,
		conf:        conf,
		dl:          dl,
		startStatus: start_status.New(),
	}
}

// Init implements aggregate.Plugin.
func (d *DataAvailability) Init() error {
	return nil
}

// Start implements aggregate.Plugin.
func (d *DataAvailability) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		err := d.startP2P()
		if err != nil {
			reject(err)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err = d.service.Started().Await(ctx)
		if err != nil {
			d.startStatus.TriggerStartFailure(err)
			reject(err)
			return
		}
		d.startStatus.TriggerStart()
		<-d.service.Context().Done()
		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
func (d *DataAvailability) Stop() error {
	return d.stopP2P()
}

func (d *DataAvailability) Started() *promise.Promise[any] {
	return d.startStatus.Started()
}

func FetchElection() (elections.ElectionResult, error) {
	client := graphql.NewClient("https://api.vsc.eco/api/v1/graphql", nil).WithDebug(true)
	var q struct {
		ElectionByBlockHeight elections.ElectionResult `graphql:"electionByBlockHeight"`
	}
	err := client.Query(context.Background(), &q, nil)
	return q.ElectionByBlockHeight, err
}

func (d *DataAvailability) RequestProof(data []byte) (stateEngine.StorageProof, error) {
	election, err := FetchElection()
	if err != nil {
		return stateEngine.StorageProof{}, err
	}

	return d.RequestProofWithElection(data, election)
}

func (d *DataAvailability) RequestProofWithElection(data []byte, election elections.ElectionResult) (stateEngine.StorageProof, error) {
	gen := dids.NewBlsCircuitGenerator(election.MemberKeys())
	c, err := d.dl.PutRaw(data, datalayer.PutRawOptions{})
	if err != nil {
		return stateEngine.StorageProof{}, err
	}
	circuit, err := gen.Generate(*c)
	if err != nil {
		return stateEngine.StorageProof{}, err
	}
	d.circuit = circuit
	err = d.service.Send(data_availability_spec.NewP2pMessage(data_availability_spec.P2pMessageData, data))
	if err != nil {
		return stateEngine.StorageProof{}, err
	}

	ctx, cancel := context.WithTimeoutCause(context.Background(), 15*time.Second, errors.New("data availability proof did not collect enough signatures"))
	defer cancel()

	t := time.NewTicker(200 * time.Millisecond)

loop:
	for {
		select {
		case <-ctx.Done():
			fmt.Println("signer count:", circuit.SignerCount())
			return stateEngine.StorageProof{}, ctx.Err()
		case <-t.C:
			if circuit.SignerCount() >= stateEngine.STORAGE_PROOF_MINIMUM_SIGNERS {
				break loop
			}
		}
	}

	finalCircuit, err := circuit.Finalize()
	if err != nil {
		return stateEngine.StorageProof{}, err
	}

	serial, err := finalCircuit.Serialize()
	if err != nil {
		return stateEngine.StorageProof{}, err
	}

	return stateEngine.StorageProof{
		c.String(),
		*serial,
	}, nil
}
