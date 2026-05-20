package price

import (
	"encoding/json"
	"testing"
	"vsc-node/modules/oracle/p2p"
)

func TestValidateBlock_AllChecksPass(t *testing.T) {
	w := &priceBlockWitness{}

	local := map[string]PricePoint{
		"BTC": {Price: 50000.0, Volume: 1000000.0},
		"ETH": {Price: 3000.0, Volume: 500000.0},
	}
	data, _ := json.Marshal(local)
	block := &p2p.OracleBlock{Data: data}

	if !w.validateBlock(block, local) {
		t.Fatal("validateBlock returned false when all prices and volumes match")
	}
}

func TestValidateBlock_PriceMismatch(t *testing.T) {
	w := &priceBlockWitness{}

	broadcast := map[string]PricePoint{
		"BTC": {Price: 50000.0, Volume: 1000000.0},
	}
	local := map[string]PricePoint{
		"BTC": {Price: 99999.0, Volume: 1000000.0},
	}
	data, _ := json.Marshal(broadcast)
	block := &p2p.OracleBlock{Data: data}

	if w.validateBlock(block, local) {
		t.Fatal("validateBlock should reject mismatched price")
	}
}

func TestValidateBlock_MissingSymbol(t *testing.T) {
	w := &priceBlockWitness{}

	broadcast := map[string]PricePoint{
		"BTC": {Price: 50000.0, Volume: 1000000.0},
	}
	local := map[string]PricePoint{
		"BTC": {Price: 50000.0, Volume: 1000000.0},
		"ETH": {Price: 3000.0, Volume: 500000.0},
	}
	data, _ := json.Marshal(broadcast)
	block := &p2p.OracleBlock{Data: data}

	if w.validateBlock(block, local) {
		t.Fatal("validateBlock should reject when local has symbols broadcast lacks")
	}
}

func TestValidateBlock_BadJSON(t *testing.T) {
	w := &priceBlockWitness{}

	block := &p2p.OracleBlock{Data: []byte("not json")}
	local := map[string]PricePoint{"BTC": {Price: 1.0, Volume: 1.0}}

	if w.validateBlock(block, local) {
		t.Fatal("validateBlock should reject unparseable data")
	}
}
