package btcrelay

import (
	"context"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"
	"vsc-node/modules/oracle/httputils"
	"vsc-node/modules/oracle/p2p"

	"github.com/go-playground/validator/v10"
)

var v = validator.New()

type BtcChainRelay struct {
	httpClient *http.Client
}

// https://www.blockcypher.com/dev/bitcoin/#block
type BtcHeadBlock struct {
	Hash       string `json:"hash,omitempty"       validate:"hexadecimal"`
	Height     uint32 `json:"height,omitempty"`
	PrevBlock  string `json:"prev_block,omitempty" validate:"hexadecimal"`
	MerkleRoot string `json:"mrkl_root,omitempty"  validate:"hexadecimal"`
	Timestamp  string `json:"time,omitempty"`
	Fees       uint32 `json:"fees,omitempty"`
}

type btcChainMetadata struct {
	Hash string `json:"hash" validate:"hexadecimal"`
}

func New() BtcChainRelay {
	httpClient := http.DefaultClient
	httpClient.Jar, _ = cookiejar.New(nil)

	return BtcChainRelay{
		httpClient: httpClient,
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
		}
	}
}

func (b *BtcChainRelay) fetchChain() (*BtcHeadBlock, error) {
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

	return httputils.SendRequest[BtcHeadBlock](req, v)
}
