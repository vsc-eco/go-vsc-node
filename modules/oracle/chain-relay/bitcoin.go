package chainrelay

import (
	"vsc-node/modules/oracle/p2p"
)

const bitcoinSymbol = "BTC"

type bitcoinRelayer struct {
}

var _ chainRelay = &bitcoinRelayer{}

func (b *bitcoinRelayer) Symbol() string {
	return bitcoinSymbol
}

// GetBlock implements chainRelay.
func (b *bitcoinRelayer) GetBlock(
	blockDepth int,
) (*p2p.BlockRelay, error) {
	panic("unimplemented")
}

// Init implements chainRelay.
func (b *bitcoinRelayer) Init() error {
	return nil
}

// Stop implements chainRelay.
func (b *bitcoinRelayer) Stop() error {
	return nil
}
