package btcrelay

import (
	"net/http"
	"vsc-node/modules/oracle/httputils"
	"vsc-node/modules/oracle/p2p"

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
