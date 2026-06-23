package e2e

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/vsc-eco/hivego"
)

func TransformTx(tx hivego.HiveTransaction) []hivego.Operation {
	var insertOps []hivego.Operation
	for _, op := range tx.Operations {
		opName := op.OpName()

		//Prepass, convert to flat map[string]interface{}
		var Value map[string]interface{}
		bval, _ := json.Marshal(op)
		json.Unmarshal(bval, &Value)

		// Do operation specific parsing to match hivego.Operation format
		if opName == "transfer" || opName == "transfer_from_savings" || opName == "transfer_to_savings" {
			rawAmount := Value["amount"].(string)

			splitAmt := strings.Split(rawAmount, " ")
			var nai string
			if splitAmt[1] == "HBD" {
				nai = "@@000000013"
			} else if splitAmt[1] == "HIVE" {
				nai = "@@000000021"
			}

			amtFloat, _ := strconv.ParseFloat(splitAmt[0], 64)

			//3 decimal places.
			amount := map[string]interface{}{
				"nai":       nai,
				"amount":    strconv.Itoa(int(amtFloat * 1000)),
				"precision": 3,
			}

			Value["amount"] = amount
		}
		//Probably not needed? Add more specific parsers as required

		insertOps = append(insertOps, hivego.Operation{
			Type:  opName,
			Value: Value,
		})
	}

	return insertOps
}
