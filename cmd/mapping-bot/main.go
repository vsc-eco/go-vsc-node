package main

import (
	"fmt"
	"log"
	"net/http"
	"vsc-node/cmd/mapping-bot/ingest"
	"vsc-node/cmd/mapping-bot/parser"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/hasura/go-graphql-client"
)

const graphQLUrl = "https://api.vsc.eco/api/v1/graphql"

func main() {
	graphQlClient := graphql.NewClient(graphQLUrl, nil)
	obserbedTxs,

		lastBlockHeight := uint32(918291)
	blockHeight := lastBlockHeight + 1

	client := ingest.NewMempoolClient()
	hash, status, err := client.GetBlockHashAtHeight(blockHeight)
	if status == http.StatusNotFound {
		fmt.Println("No new block.")
		return
	} else if err != nil {
		log.Fatal(err.Error())
	}
	blockBytes, err := client.GetRawBlock(hash)
	if err != nil {
		log.Fatalln(err.Error())
	}

	// map of vsc to btc addresses
	// addressRegistry := make(map[string]string)
	addressRegistry := map[string]string{
		"hive:milo-hpr": "bc1qmk308hkyav7s6fd37y28ajhc22q4xeg3e24caa",
	}

	blockParser := parser.NewBlockParser(addressRegistry, &chaincfg.MainNetParams)

	foundTxs, err := blockParser.ParseBlock(blockBytes, blockHeight)
	if err != nil {
		log.Fatalln(err.Error())
	}

	fmt.Println(foundTxs)

	err = parser.VerifyTransaction(&foundTxs[0])
	if err != nil {
		fmt.Println(err.Error())
	} else {
		fmt.Println("transaction verified")
	}
}
