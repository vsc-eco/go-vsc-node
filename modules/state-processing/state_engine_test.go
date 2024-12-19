package stateEngine_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/transactions"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/hive/streamer"

	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/test_utils"

	stateEngine "vsc-node/modules/state-processing"

	"github.com/libp2p/go-libp2p"
	kadDht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	"github.com/stretchr/testify/assert"
	"github.com/vsc-eco/hivego"
)

var BOOTSTRAP = []string{
	"/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWAvxZcLJmZVUaoAtey28REvaBwxvfTvQfxWtXJ2fpqWnw",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
	"/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",         // mars.i.ipfs.io
	"/ip4/104.131.131.82/udp/4001/quic-v1/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ", // mars.i.ipfs.io
	"/ip4/85.215.198.234/tcp/4001/p2p/12D3KooWJf8f9anWjHLc5CPcVdpmzGMAKhGXCjKsjqCQ5o8r1e1d",
	"/ip4/95.211.231.201/tcp/4001/p2p/12D3KooWJAsC5ZtFNGpLZCQVzwfmssodvxJ4g1SuvtqEvk7KFjfZ",
}

type setupResult struct {
	host host.Host
	dht  *kadDht.IpfsDHT
}

func setupEnv() setupResult {
	pkbytes := []byte("PRIVATE_KEY_TEST_ONLY")
	pk, _, _ := crypto.GenerateSecp256k1Key(bytes.NewReader(pkbytes))
	host, _ := libp2p.New(libp2p.Identity(pk))
	ctx := context.Background()

	dht, _ := kadDht.New(ctx, host)
	routedHost := rhost.Wrap(host, dht)
	go func() {
		dht.Bootstrap(ctx)
		for _, peerStr := range BOOTSTRAP {
			peerId, _ := peer.AddrInfoFromString(peerStr)

			routedHost.Connect(ctx, *peerId)
		}
	}()

	return setupResult{
		host: routedHost,
		dht:  dht,
	}
}

func TestStateEngine(t *testing.T) {
	conf := db.NewDbConfig()
	db := db.New(conf)
	vscDb := vsc.New(db)
	hiveBlocks, err := hive_blocks.New(vscDb)
	electionDb := elections.New(vscDb)
	witnessesDb := witnesses.New(vscDb)
	contractDb := contracts.New(vscDb)
	txDb := transactions.New(vscDb)

	filter := func(op hivego.Operation) bool {
		if op.Type == "custom_json" {
			if strings.HasPrefix(op.Value["id"].(string), "vsc.") {
				return true
			}
		}
		if op.Type == "account_update" || op.Type == "account_update2" {
			return true
		}

		if op.Type == "transfer" {
			if strings.HasPrefix(op.Value["to"].(string), "vsc.") {
				return true
			}

			if strings.HasPrefix(op.Value["from"].(string), "vsc.") {
				return true
			}
		}

		return false
	}

	client := hivego.NewHiveRpc("https://api.hive.blog")
	s := streamer.NewStreamer(client, hiveBlocks, []streamer.FilterFunc{filter}, nil)

	// slow down the streamer a bit for real data
	streamer.AcceptableBlockLag = 0
	streamer.BlockBatchSize = 500
	streamer.DefaultBlockStart = 81614028
	streamer.HeadBlockCheckPollIntervalBeforeFirstUpdate = time.Millisecond * 250
	streamer.MinTimeBetweenBlockBatchFetches = time.Millisecond * 250
	streamer.DbPollInterval = time.Millisecond * 500

	assert.NoError(t, err)

	setup := setupEnv()

	dl := DataLayer.New(setup.host, setup.dht)

	se := stateEngine.New(dl, witnessesDb, electionDb, contractDb, txDb)

	se.Commit()
	process := func(block hive_blocks.HiveBlock) {
		for _, tx := range block.Transactions {
			se.ProcessTx(tx, stateEngine.ProcessExtraInfo{
				BlockId:     block.BlockID,
				BlockHeight: block.BlockNumber,
				Timestamp:   block.Timestamp,
			})
		}
	}
	sr := streamer.NewStreamReader(hiveBlocks, process)

	agg := aggregate.New([]aggregate.Plugin{
		conf,
		db,
		vscDb,
		electionDb,
		witnessesDb,
		contractDb,
		txDb,
		hiveBlocks,
		s,
		sr,
	})

	test_utils.RunPlugin(t, agg)

	fmt.Println(err)

	select {}
}
