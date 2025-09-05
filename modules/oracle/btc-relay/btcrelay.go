package btcrelay

import (
	// "log"
	"net/http"
	// "os"
	// "path/filepath"
	"vsc-node/modules/oracle/httputils"
	"vsc-node/modules/oracle/p2p"

	// "github.com/btcsuite/btcd/rpcclient"
	"github.com/go-playground/validator/v10"
)

var v = validator.New()

type BtcChainRelay struct {
	btcChan chan *p2p.BtcHeadBlock
}

type btcChainMetadata struct {
	Hash string `json:"hash" validate:"hexadecimal"`
}

func New(btcChan chan *p2p.BtcHeadBlock) BtcChainRelay {
	/*
		// btcdHomeDir := btcutil.AppDataDir(".btcd", false)

		certs, err := os.ReadFile(filepath.Join("../../../.btcd", "rpc.cert"))
		if err != nil {
			panic(err)
		}

		connCfg := &rpcclient.ConnConfig{
			Host:         "0.0.0.0:8334",
			Endpoint:     "",
			User:         "myuser",
			Pass:         "mypass",
			Certificates: certs,
		}

		client, err := rpcclient.New(connCfg, nil)
		if err != nil {
			log.Fatal("failed to connect to rprc server", err)
		}
		defer client.Shutdown()

		// Query the RPC server for the current block count and display it.
		blockCount, err := client.GetBlockCount()
		if err != nil {
			log.Fatal("failed to get blocks", err)
		}
		log.Printf("Block count: %d", blockCount)

	*/
	return BtcChainRelay{
		btcChan: btcChan,
	}
}

func (b *BtcChainRelay) FetchChain() (*p2p.BtcHeadBlock, error) {
	// query chain metadata

	// valid url, no error returns
	apiUrl, _ := httputils.MakeUrl(
		"https://api.blockcypher.com/v1/btc/main",
		nil,
	)

	req, err := httputils.MakeRequest(http.MethodGet, apiUrl, nil)
	if err != nil {
		return nil, err
	}

	chain, err := httputils.SendRequest[btcChainMetadata](req, v)
	if err != nil {
		return nil, err
	}

	// query head block

	// valid hard coded url, no need to handle error
	apiUrl, _ = httputils.MakeUrl(
		"https://api.blockcypher.com/v1/btc/main/blocks",
		nil,
	)
	apiUrl = apiUrl.JoinPath(chain.Hash)

	req, err = httputils.MakeRequest(http.MethodGet, apiUrl, nil)
	if err != nil {
		return nil, err
	}

	return httputils.SendRequest[p2p.BtcHeadBlock](req, v)
}
