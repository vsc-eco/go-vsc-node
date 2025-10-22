package price

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/oracle/price/api"
	transactionpool "vsc-node/modules/transaction-pool"

	blocks "github.com/ipfs/go-block-format"
)

const (
	contractID         = ""
	basePriceOracleDID = "did:vsc:price-oracle"
	contractCallAction = ""
)

type PricePoint struct {
	Symbol         string `json:"symbol"`
	api.PricePoint `json:",inline"`
}

func makeTx(priceMap map[string]api.PricePoint) (blocks.Block, error) {
	// serialize input
	pricePoints := make([]PricePoint, 0, len(priceMap))

	for sym, pricePoint := range priceMap {
		pricePoints = append(pricePoints, PricePoint{
			Symbol:     sym,
			PricePoint: pricePoint,
		})
	}

	slices.SortFunc(pricePoints, func(a PricePoint, b PricePoint) int {
		return strings.Compare(a.Symbol, b.Symbol)
	})

	payload, err := json.Marshal(pricePoints)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize payload: %w", err)
	}

	// make signable block
	op := transactionpool.VscContractCall{
		ContractId: contractID,
		Action:     contractCallAction,
		Payload:    string(payload),
		Intents:    []contracts.Intent{},
		RcLimit:    1000,
		Caller:     basePriceOracleDID,
		NetId:      "vsc-mainnet",
	}

	vOp, err := op.SerializeVSC()
	if err != nil {
		return nil, fmt.Errorf("failed to serialize vsc operation: %w", err)
	}

	nonce, err := getNonce()
	if err != nil {
		return nil, fmt.Errorf("failed to get nonce: %w", err)
	}

	vscTx := &transactionpool.VSCTransaction{
		Ops:   []transactionpool.VSCTransactionOp{vOp},
		Nonce: nonce,
		NetId: "vsc-mainnet",
	}

	signableBlock, err := vscTx.ToSignableBlock()
	if err != nil {
		return nil, fmt.Errorf("failed to make signable block: %w", err)
	}

	return signableBlock, nil
}

func getNonce() (uint64, error) {
	// TODO: implement getNonce function
	return 0, nil
}
