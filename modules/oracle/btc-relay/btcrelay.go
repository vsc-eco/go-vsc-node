package btcrelay

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"
	"vsc-node/modules/oracle/httputils"
	"vsc-node/modules/oracle/p2p"

	"github.com/go-playground/validator/v10"
)

var v = validator.New()

const blockcypherUrl = "https://api.blockcypher.com/v1/btc/main"

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

func (b *BtcChainRelay) Poll(
	ctx context.Context,
	relayInterval time.Duration,
	broadcastChan chan<- p2p.Msg,
) {
	ticker := time.NewTicker(relayInterval)

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			headBlock, err := b.fetchChain()
			if err != nil {
				log.Println("failed to fetch BTC head block.", err)
				continue
			}

			broadcastChan <- &p2p.OracleMessage{
				Type: p2p.MsgBtcChainRelay,
				Data: headBlock,
			}

		case headBlock := <-b.btcChan:
			fmt.Println(headBlock)
		}
	}
}

func (b *BtcChainRelay) fetchChain() (*p2p.BtcHeadBlock, error) {
	// query chain metadata

	// valid url, no error returns
	apiUrl, _ := httputils.MakeUrl(
		blockcypherUrl,
		nil,
	)

	req, err := httputils.MakeRequest(http.MethodGet, apiUrl, nil)
	if err != nil {
		return nil, err
	}

	chain, err := httputils.SendRequest[btcChainMetadata](req)
	if err != nil {
		return nil, err
	}

	// query head block

	// valid hard coded url, no need to handle error
	blockUrl, _ := url.Parse("https://api.blockcypher.com/v1/btc/main/blocks")

	apiUrl, err = httputils.MakeUrl(blockUrl.JoinPath(chain.Hash).String(), nil)
	if err != nil {
		return nil, err
	}

	req, err = httputils.MakeRequest(http.MethodGet, apiUrl, nil)
	if err != nil {
		return nil, err
	}

	return httputils.SendRequest[p2p.BtcHeadBlock](req, v)
}
