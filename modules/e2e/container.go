package e2e

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"
	cbortypes "vsc-node/lib/cbor-types"
	"vsc-node/lib/test_utils"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/vsc-eco/hivego"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/hive_blocks"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"
)

type E2EContainer struct {
	NodeCount int
	Steps     []Step

	HiveCreator stateEngine.MockCreator
	OnStart     chan interface{}

	//Internal
	runningNodes   []Node
	aggregateNodes *aggregate.Aggregate
	nodeNames      []string
	mockReader     *stateEngine.MockReader
	hiveConsumer   blockconsumer.HiveConsumer
	r2e            *E2ERunner
	client         NodeClient
}

func (c *E2EContainer) VSCBroadcast() *transactionpool.InternalBroadcast {
	return &transactionpool.InternalBroadcast{
		TxPool: c.runningNodes[0].TxPool,
	}
}

func (c *E2EContainer) AddStep(funcx ...Step) int {
	c.Steps = append(c.Steps, funcx...)
	return len(c.Steps) - 1
}

func (c *E2EContainer) RunSteps(t *testing.T) error {
	for _, step := range c.Steps {
		if step.TestFunc == nil {
			return fmt.Errorf("step %s has no TestFunc", step.Name)
		}
		ctx := StepCtx{
			Container: c,
		}
		eval, err := step.TestFunc(ctx)
		if err != nil {
			t.Error(err)
			return fmt.Errorf("step %s failed: %w", step.Name, err)
		}
		if eval != nil {
			err = eval(ctx)
			if err != nil {
				t.Error(err)
				return fmt.Errorf("step %s evaluation failed: %w", step.Name, err)
			}
		}
	}

	return nil
}

func (c *E2EContainer) Runner() *E2ERunner {
	return c.r2e
}

func (c *E2EContainer) Client() NodeClient {
	return c.client
}

func (c *E2EContainer) initClient() {
	client := MakeClient(MakeClientInput{
		// BrcstFunc: broadcastFunc,
	})

	agg := aggregate.New(client.Plugins)

	agg.Init()
	agg.Start().Await(context.Background())

	peerAddrs := make([]string, 0)

	for _, node := range c.runningNodes {
		for _, addr := range node.P2P.Addrs() {
			peerAddrs = append(peerAddrs, addr.String()+"/p2p/"+node.P2P.ID().String())
		}
	}

	for _, peerStr := range peerAddrs {
		peerId, _ := peer.AddrInfoFromString(peerStr)
		ctx := context.Background()
		ctx, _ = context.WithTimeout(ctx, 5*time.Second)
		// fmt.Println("Trying to connect", peerId)
		client.P2PService.Connect(ctx, *peerId)
	}

	c.client = client
}

func (c *E2EContainer) Init() error {
	cbortypes.RegisterTypes()

	c.mockReader = stateEngine.NewMockReader(c.hiveConsumer.ProcessBlock)

	mockCreator := stateEngine.MockCreator{
		Mr: c.mockReader,
	}

	broadcastFunc := func(tx hivego.HiveTransaction) error {
		insertOps := TransformTx(tx)

		txId, _ := tx.GenerateTrxId()

		mockCreator.BroadcastOps(insertOps, txId)

		return nil
	}

	//Make primary node

	c.r2e = &E2ERunner{
		BlockEvent: make(chan uint64),
	}

	// nodeNames := make([]string, 0)
	c.nodeNames = append(c.nodeNames, "e2e-1")
	for i := 2; i < c.NodeCount+1; i++ {
		name := "e2e-" + strconv.Itoa(i)
		c.nodeNames = append(c.nodeNames, name)
	}

	primaryNode := MakeNode(MakeNodeInput{
		Username:  "e2e-1",
		BrcstFunc: broadcastFunc,
		Runner:    c.r2e,
		Primary:   true,
	})
	c.runningNodes = append(c.runningNodes, *primaryNode)

	//Make the remaining 3 nodes for consensus operation
	for i := 2; i < c.NodeCount+1; i++ {
		name := "e2e-" + strconv.Itoa(i)
		c.runningNodes = append(c.runningNodes, *MakeNode(MakeNodeInput{
			Username:  name,
			BrcstFunc: broadcastFunc,
			Runner:    nil,
		}))
	}

	plugs := make([]aggregate.Plugin, 0)

	for _, node := range c.runningNodes {
		plugs = append(plugs, node.Aggregate)
	}

	c.HiveCreator = mockCreator

	c.aggregateNodes = aggregate.New(plugs)

	return nil
}

func (c *E2EContainer) Start(t *testing.T) error {
	test_utils.RunPlugin(t, c.aggregateNodes, false)
	// go c.aggregateNodes.Start()

	plugsz := make([]aggregate.Plugin, 0)

	for _, node := range c.runningNodes {
		plugsz = append(plugsz, &node)
	}
	startupAggregate := aggregate.New(plugsz)

	startupAggregate.Init()
	startupAggregate.Run()

	// test_utils.RunPlugin(t, startupAggregate, false)

	c.mockReader.ProcessFunction = func(block hive_blocks.HiveBlock, headHeight *uint64) {
		for _, node := range c.runningNodes {
			node.MockHiveBlocks.HighestBlock = block.BlockNumber
		}
		for _, node := range c.runningNodes {
			node.HiveConsumer.ProcessBlock(block, headHeight)
		}
	}

	func() {

		peerAddrs := make([]string, 0)

		for _, node := range c.runningNodes {
			for _, addr := range node.P2P.Addrs() {
				peerAddrs = append(peerAddrs, addr.String()+"/p2p/"+node.P2P.ID().String())
			}
		}

		for _, node := range c.runningNodes {
			for _, peerStr := range peerAddrs {
				peerId, _ := peer.AddrInfoFromString(peerStr)
				ctx := context.Background()
				ctx, _ = context.WithTimeout(ctx, 5*time.Second)
				// fmt.Println("Trying to connect", peerId)
				node.P2P.Connect(ctx, *peerId)
			}
		}
	}()

	c.initClient()

	c.mockReader.StartRealtime()

	test_utils.RunPlugin(t, c.r2e, false)

	return nil
}

func (c *E2EContainer) Stop() error {
	c.aggregateNodes.Stop()
	return nil
}

func NewContainer(nodeCount int) *E2EContainer {
	return &E2EContainer{
		NodeCount: nodeCount,
	}
}
