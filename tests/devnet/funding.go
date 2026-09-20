package devnet

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/vsc-eco/hivego"
)

// rcHbdBackfill is the L2 HBD credit each witness account receives so its
// RC budget (balance + RC_HIVE_FREE_AMOUNT) matches what the old raised
// ephemeral allowance provided: 990,000 HBD units funded + 10,000 free =
// 1,000,000 available RCs. RCs are backed 1:1 by the account's L2 HBD
// balance, so the funding must be credited to the L2 ledger, not just the
// L1 account (SPV-heavy contract ops like map/migrate need the gas headroom).
const rcHbdBackfill = "990.000" // TBD per witness, in Hive-float string form

// rcHbdBackfillUnits is rcHbdBackfill in milli-HBD ledger units (3 decimals).
const rcHbdBackfillUnits = 990_000

// fundAccounts transfers TBD and TESTS from initminer to the witness
// accounts so they can pay contract deployment fees and have RCs.
//
// Alongside the L1 funding, each witness receives an L2 HBD backfill via a
// direct transfer to the gateway account (memo &to=<witness>), which credits
// the witness's VSC ledger balance. Without it, an account whose L2 HBD
// balance is ~0 can only afford the 10,000-RC free tier — far short of the
// gas SPV-heavy ops consume.
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
			// RC backing: credit the witness's L2 ledger directly through the
			// gateway deposit path (state engine ingests gateway transfers and
			// routes the credit via the memo's &to= destination).
			hivego.TransferOperation{
				From:   "initminer",
				To:     "vsc.gateway",
				Amount: rcHbdBackfill + " TBD",
				Memo:   "&to=" + witnessName,
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

// waitForRcBackfill polls the L2 ledger until every witness's HBD balance
// reflects the RC backfill, so tests that immediately spend the RC budget
// never race the deposit indexing.
func (d *Devnet) waitForRcBackfill(ctx context.Context) error {
	target := int64(rcHbdBackfillUnits)

	deadline := time.Now().Add(3 * time.Minute)
	var lastErr error
	for {
		missing := make([]string, 0, d.cfg.Nodes)
		for n := 1; n <= d.cfg.Nodes; n++ {
			bal, err := d.GetAccountBalance(ctx, 1, "hive:"+d.witnessAccount(n))
			if err != nil {
				lastErr = err
				missing = append(missing, d.witnessAccount(n))
				continue
			}
			if bal.Hbd < target {
				missing = append(missing, d.witnessAccount(n))
			}
		}
		if len(missing) == 0 {
			log.Printf("[devnet] RC backfill credited on L2 for all %d witnesses", d.cfg.Nodes)
			return nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("RC backfill not credited after 3m (missing: %v, last error: %w)", missing, lastErr)
			}
			return fmt.Errorf("RC backfill not credited after 3m (missing: %v)", missing)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("RC backfill not credited, waiting canceled (missing: %v): %w", missing, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}
