package btcrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"

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
	apiUrl := "https://api.blockcypher.com/v1/btc/main"
	chain, err := fetchData[btcChainMetadata](b.httpClient, apiUrl)
	if err != nil {
		log.Println("failed to query for chain metadata:", err)
		return
	}

	// query head block
	// valid hard coded url, no need to handle error
	blockUrl, _ := url.Parse("https://api.blockcypher.com/v1/btc/main/blocks")

	apiUrl = blockUrl.JoinPath(chain.Hash).String()
	headBlock, err := fetchData[BtcHeadBlock](b.httpClient, apiUrl)
	if err != nil {
		log.Println("failed to query for head block:", err)
		return
	}

	broadcastChan <- headBlock
}

func fetchData[T any](httpClient *http.Client, url string) (*T, error) {
	res, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request failed with status %s", res.Status)
	}

	buf := new(T)
	if err := json.NewDecoder(res.Body).Decode(buf); err != nil {
		return nil, err
	}

	if err := v.Struct(buf); err != nil {
		return nil, err
	}

	return buf, nil
}
