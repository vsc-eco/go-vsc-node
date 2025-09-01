package btcrelay

import (
	"context"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"
	"vsc-node/modules/oracle/httputils"

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
	broadcastChan chan<- *BtcHeadBlock,
) {
	ticker := time.NewTicker(relayInterval)

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			go b.fetchChain(broadcastChan)
		}
	}
}

func (b *BtcChainRelay) fetchChain(broadcastChan chan<- *BtcHeadBlock) {
	// query chain metadata
	apiUrl, _ := httputils.MakeUrl(
		"https://api.blockcypher.com/v1/btc/main",
		nil,
	) // valid url, no error returns

	req, err := httputils.MakeRequest(http.MethodGet, apiUrl, nil)
	if err != nil {
		log.Println("invalid request", err)
		return
	}

	chain, err := httputils.SendRequest[btcChainMetadata](req)
	if err != nil {
		log.Println("failed to query for chain metadata:", err)
		return
	}

	// query head block
	// valid hard coded url, no need to handle error
	blockUrl, _ := url.Parse("https://api.blockcypher.com/v1/btc/main/blocks")

	apiUrl, err = httputils.MakeUrl(blockUrl.JoinPath(chain.Hash).String(), nil)
	if err != nil {
		log.Println("invalid url", err)
		return
	}

	req, err = httputils.MakeRequest(http.MethodGet, apiUrl, nil)
	if err != nil {
		log.Println("invalid request", err)
		return
	}

	headBlock, err := httputils.SendRequest[BtcHeadBlock](req, v)
	if err != nil {
		log.Println("failed to query for head block:", err)
		return
	}

	broadcastChan <- headBlock
}
