package data_availability_client

import (
	"context"
	"fmt"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/lib/vsclog"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	systemconfig "vsc-node/modules/common/system-config"
	data_availability_spec "vsc-node/modules/data-availability/spec"
	"vsc-node/modules/db/vsc/elections"
	libp2p "vsc-node/modules/p2p"
	start_status "vsc-node/modules/start-status"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/chebyrash/promise"
	"github.com/hasura/go-graphql-client"
)

var log = vsclog.Module("da-client")

type DataAvailability struct {
	p2p         *libp2p.P2PServer
	service     libp2p.PubSubService[p2pMessage]
	conf        common.IdentityConfig
	sconf       systemconfig.SystemConfig
	dl          *datalayer.DataLayer
	circuit     dids.PartialBlsCircuit
	startStatus start_status.StartStatus

	peerWait    time.Duration
	attemptWait time.Duration
}

// DA-START-1: the contract deployer asks for its storage proof right after it starts,
// and a node makes its first connections only at p2p Start (P2P-START-1). A request
// published before any server was on the topic reached no one, and the deploy failed
// 15 s later with a bare "context deadline exceeded". RequestProofWithElection now waits
// (bounded) for peers on the topic, sends the request once more if the first collects
// too few signatures, and says what it got when it gives up.
const (
	defaultProofPeerWait    = 60 * time.Second
	defaultProofAttemptWait = 15 * time.Second
	proofRequestAttempts    = 2
	proofMeshSettle         = 2 * time.Second
)

var _ a.Plugin = (*DataAvailability)(nil)

func New(p2p *libp2p.P2PServer, conf common.IdentityConfig, sconf systemconfig.SystemConfig, dl *datalayer.DataLayer) *DataAvailability {
	return &DataAvailability{
		p2p:         p2p,
		conf:        conf,
		sconf:       sconf,
		dl:          dl,
		startStatus: start_status.New(),
		peerWait:    defaultProofPeerWait,
		attemptWait: defaultProofAttemptWait,
	}
}

// SetProofTimeouts overrides how long a proof request waits for topic peers and for
// signatures per attempt. For tests only.
func (d *DataAvailability) SetProofTimeouts(peerWait, attemptWait time.Duration) {
	d.peerWait = peerWait
	d.attemptWait = attemptWait
}

// Init implements aggregate.Plugin.
func (d *DataAvailability) Init() error {
	return nil
}

// Start implements aggregate.Plugin.
func (d *DataAvailability) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		log.Info("starting data availability client")
		err := d.startP2P()
		if err != nil {
			reject(err)
			return
		}
		log.Verbose("p2p started")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err = d.service.Started().Await(ctx)
		log.Verbose("service started")
		if err != nil {
			d.startStatus.TriggerStartFailure(err)
			reject(err)
			return
		}
		d.startStatus.TriggerStart()
		log.Info("data availability client ready")
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

func FetchElection(gqlUrl string) (elections.ElectionResult, error) {
	client := graphql.NewClient(gqlUrl, nil).WithDebug(true)
	var q struct {
		ElectionByBlockHeight elections.ElectionResult `graphql:"electionByBlockHeight"`
	}
	err := client.Query(context.Background(), &q, nil)
	return q.ElectionByBlockHeight, err
}

func (d *DataAvailability) RequestProof(gqlUrl string, data []byte) (stateEngine.StorageProof, error) {
	election, err := FetchElection(gqlUrl)
	if err != nil {
		return stateEngine.StorageProof{}, err
	}

	return d.RequestProofWithElection(data, election)
}

// waitForTopicPeers waits until `need` peers are on the topic, at most peerWait. Fewer
// can still do: the mesh relays the request past direct peers, so it goes out anyway
// with at least one. None at all is an error. Gossipsub grafts new peers into the mesh
// on its 1 s heartbeat, and a request published the moment they appear can lose its
// signature replies, so it then waits proofMeshSettle before returning.
func (d *DataAvailability) waitForTopicPeers(need int) error {
	deadline := time.Now().Add(d.peerWait)
	peers := d.service.TopicPeers()
	for peers < need && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		peers = d.service.TopicPeers()
	}
	if peers == 0 {
		return fmt.Errorf("no data availability peers after %s: check the bootnodes and that this node can reach them", d.peerWait)
	}
	if peers < need {
		log.Warn("data availability proof: fewer topic peers than signers needed, sending anyway",
			"topicPeers", peers, "need", need)
	}
	time.Sleep(proofMeshSettle)
	return nil
}

// awaitSigners reports whether the circuit reaches `need` signers within wait.
func awaitSigners(circuit dids.PartialBlsCircuit, need int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for circuit.SignerCount() < need {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
	return true
}

func (d *DataAvailability) RequestProofWithElection(data []byte, election elections.ElectionResult) (stateEngine.StorageProof, error) {
	gen := dids.NewBlsCircuitGenerator(election.MemberKeys())
	c, err := d.dl.PutRaw(data, common_types.PutRawOptions{})
	if err != nil {
		return stateEngine.StorageProof{}, err
	}
	circuit, err := gen.Generate(*c)
	if err != nil {
		return stateEngine.StorageProof{}, err
	}
	d.circuit = circuit
	need := d.sconf.ConsensusParams().MinSpSigners

	if err := d.waitForTopicPeers(need); err != nil {
		return stateEngine.StorageProof{}, err
	}

	msg := data_availability_spec.NewP2pMessage(data_availability_spec.P2pMessageData, data)
	for attempt := 1; ; attempt++ {
		if err := d.service.Send(msg); err != nil {
			return stateEngine.StorageProof{}, err
		}
		if awaitSigners(circuit, need, d.attemptWait) {
			break
		}
		peers := d.service.TopicPeers()
		if attempt == proofRequestAttempts {
			return stateEngine.StorageProof{}, fmt.Errorf(
				"data availability proof collected %d of the %d signatures it needs after %d requests (%d peers on the topic)",
				circuit.SignerCount(), need, attempt, peers)
		}
		log.Warn("data availability proof: too few signatures, sending the request again",
			"signers", circuit.SignerCount(), "need", need, "topicPeers", peers)
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
