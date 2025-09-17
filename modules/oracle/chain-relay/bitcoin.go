package chainrelay

import (
	"errors"
	"os"
	"vsc-node/modules/oracle/p2p"

	"github.com/btcsuite/btcd/rpcclient"
)

const bitcoinSymbol = "BTC"

type bitcoinRelayer struct {
	rpcConfig rpcclient.ConnConfig
}

var (
	_ chainRelay = &bitcoinRelayer{}

	errInvalidConf = errors.New("invalid config")
)

func (b *bitcoinRelayer) Symbol() string {
	return bitcoinSymbol
}

// GetBlock implements chainRelay.
func (b *bitcoinRelayer) GetBlock() (*p2p.BlockRelay, error) {
	panic("unimplemented")
}

// Init implements chainRelay.
func (b *bitcoinRelayer) Init() error {
	var (
		rpcUsername, rpcUsernameOk = os.LookupEnv("BTCD_RPC_USERNAME")
		rpcPassword, rpcPasswordOk = os.LookupEnv("BTCD_RPC_PASSWORD")
		rpcHost, rpcHostOk         = os.LookupEnv("BTCD_RPC_HOST")
	)

	if !rpcUsernameOk || !rpcPasswordOk || !rpcHostOk {
		return errInvalidConf
	}

	b.rpcConfig = rpcclient.ConnConfig{
		Host:         rpcHost,
		User:         rpcUsername,
		Pass:         rpcPassword,
		HTTPPostMode: true,
		DisableTLS:   true,
	}

	/*
		headblock, err := client.GetBlockCount()
		if err != nil {
			log.Fatal(err)
		}

		// Get block at specific height
		blockHash, err := client.GetBlockHash(headblock)
		if err != nil {
			log.Fatal(err)
		}

		block, err := client.GetBlock(blockHash)
		if err != nil {
			log.Fatal(err)
		}

		log.Printf("Block: %+v", block)
	*/
	return nil
}

// Stop implements chainRelay.
func (b *bitcoinRelayer) Shutdown() error {
	return nil
}

func (b *bitcoinRelayer) connect() (*rpcclient.Client, error) {
	return rpcclient.New(&b.rpcConfig, nil)
}
