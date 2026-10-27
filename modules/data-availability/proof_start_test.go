package data_availability_test

import (
	"context"
	"fmt"
	"testing"
	"time"
	"vsc-node/lib/test_utils"
	"vsc-node/lib/utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/config"
	"vsc-node/modules/db/vsc/elections"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

// proofCluster starts a requesting client, `idle` other clients (on the data
// availability topic, but they never sign) and MinSpSigners servers, all unconnected.
func proofCluster(t *testing.T, name string, idle int) (*Node, []*Node, []*Node, elections.ElectionResult) {
	t.Helper()
	config.UseMainConfigDuringTests = true
	client := MakeNode(MakeNodeInput{Username: name + "-client", Client: true})
	idlers := make([]*Node, idle)
	for i := range idlers {
		idlers[i] = MakeNode(MakeNodeInput{Username: fmt.Sprint(name, "-idle-", i), Client: true})
	}
	servers := make([]*Node, client.sysConfig.ConsensusParams().MinSpSigners)
	for i := range servers {
		servers[i] = MakeNode(MakeNodeInput{Username: fmt.Sprint(name, "-server-", i)})
	}
	nodes := append(append([]*Node{client}, idlers...), servers...)
	test_utils.RunPlugin(t, aggregate.New(utils.Map(nodes, func(n *Node) aggregate.Plugin { return n })), false)
	for _, n := range nodes {
		require.NoError(t, n.NukeDb())
	}

	election := elections.ElectionResult{}
	election.Members = utils.Map(servers, func(s *Node) elections.ElectionMember {
		return elections.ElectionMember{Key: s.ConsensusKey()}
	})
	election.Weights = utils.Map(servers, func(s *Node) uint64 { return 1 })
	election.TotalWeight = uint64(len(servers))
	return client, idlers, servers, election
}

func connectTo(t *testing.T, from *Node, to []*Node) {
	t.Helper()
	for _, n := range to {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := from.p2p.Connect(ctx, peer.AddrInfo{ID: n.p2p.ID(), Addrs: n.p2p.Addrs()})
		cancel()
		require.NoError(t, err)
	}
}

type proofResult struct {
	proof stateEngine.StorageProof
	err   error
}

func requestProofAsync(client *Node, data []byte, election elections.ElectionResult) chan proofResult {
	done := make(chan proofResult, 1)
	go func() {
		p, err := client.client.RequestProofWithElection(data, election)
		done <- proofResult{p, err}
	}()
	return done
}

// DA-START-1: the contract deployer requests its storage proof right after it starts,
// and since P2P-START-1 a node makes its first connections only at p2p Start. Servers
// that join the topic a few seconds after the request must still sign it.
func TestRequestProof_ServersArriveAfterTheRequest(t *testing.T) {
	client, _, servers, election := proofCluster(t, "late", 0)
	start := time.Now()
	done := requestProofAsync(client, []byte("deployer wasm, servers arrive late"), election)

	time.Sleep(3 * time.Second)
	connectTo(t, client, servers)

	select {
	case res := <-done:
		require.NoError(t, res.err, "the request went out before any server was on the topic")
		require.True(t, res.proof.Verify(election, client.sysConfig), "proof does not verify")
		// Sent once the servers are on the topic, not rescued by the resend 15 s later.
		require.Less(t, time.Since(start), 12*time.Second, "the first request went out before any server was on the topic")
	case <-time.After(90 * time.Second):
		t.Fatal("RequestProofWithElection did not return")
	}
}

// DA-START-1: a request that reached only peers that do not sign is sent once more.
// The servers connect after gossipsub's message cache (5 heartbeats) has dropped the
// first request, so only a second send can reach them.
func TestRequestProof_ResendsOnceWhenTheFirstRequestIsUnanswered(t *testing.T) {
	client, idlers, servers, election := proofCluster(t, "resend", 3)
	connectTo(t, client, idlers)
	time.Sleep(time.Second)
	done := requestProofAsync(client, []byte("deployer wasm, first request unanswered"), election)

	time.Sleep(7 * time.Second)
	connectTo(t, client, servers)

	select {
	case res := <-done:
		require.NoError(t, res.err, "the unanswered request was never sent again")
		require.True(t, res.proof.Verify(election, client.sysConfig), "proof does not verify")
	case <-time.After(90 * time.Second):
		t.Fatal("RequestProofWithElection did not return")
	}
}

// DA-START-1: with no peer on the topic the request is not sent into the void; it
// fails after the bounded wait and says why.
func TestRequestProof_NoPeersFailsWithAReason(t *testing.T) {
	client, _, _, election := proofCluster(t, "alone", 0)
	client.client.SetProofTimeouts(2*time.Second, 2*time.Second)
	start := time.Now()
	_, err := client.client.RequestProofWithElection([]byte("deployer wasm, nobody listening"), election)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no data availability peers")
	require.Less(t, time.Since(start), 10*time.Second, "the peer wait is bounded")
}
