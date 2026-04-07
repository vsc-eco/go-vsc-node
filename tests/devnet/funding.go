package devnet

import (
	"fmt"
	"log"

	"github.com/vsc-eco/hivego"
)

// fundAccounts transfers TBD and TESTS from initminer to the witness
// accounts so they can pay contract deployment fees and have RCs.
func (d *Devnet) fundAccounts() error {
	hiveClient := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	hiveClient.ChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"
	wif := d.cfg.InitminerWIF

	var ops []hivego.HiveOperation
	for n := 1; n <= d.cfg.Nodes; n++ {
		witnessName := fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, n)
		ops = append(ops,
			hivego.TransferOperation{
				From:   "initminer",
				To:     witnessName,
				Amount: "100.000 TBD",
				Memo:   "devnet funding",
			},
			hivego.TransferOperation{
				From:   "initminer",
				To:     witnessName,
				Amount: "10000.000 TESTS",
				Memo:   "devnet funding",
			},
		)
	}

	log.Printf("[devnet] funding %d witness accounts from initminer...", d.cfg.Nodes)
	_, err := hiveClient.Broadcast(ops, &wif)
	if err != nil {
		return fmt.Errorf("funding accounts: %w", err)
	}

	log.Printf("[devnet] accounts funded")
	return nil
}
