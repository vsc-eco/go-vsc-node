package oracle

import (
	"encoding/json"

	"vsc-node/lib/hive"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/oracle/chain"

	"github.com/vsc-eco/hivego"
)

// N4 — the BTC solvency watch's runner (from 0.13.0). At each checkpoint
// (params.BTC_SOLVENCY_CHECK_EVERY) a witness of the election checks the BTC
// contract's book against its own bitcoind (chain.ObserveBtcSolvency) in the
// background, one check at a time, and votes "vsc.btc_solvency" from its
// account only when what it saw differs from the current halt: insolvent while
// not halted, solvent while halted. A healthy chain costs no operations. The
// state engine tallies the votes (state-processing btc_solvency.go).

// SetHiveCreator gives the oracle the node's Hive transaction creator, for
// the solvency votes.
func (o *Oracle) SetHiveCreator(h hive.HiveTransactionCreator) { o.hiveCreator = h }

// btcSolvencyVoteFor returns the vote to send for checkpoint c, if any.
func btcSolvencyVoteFor(c uint64, obs chain.SolvencyObservation, halted bool) (string, bool) {
	if obs.Insolvent == halted {
		return "", false
	}
	raw, _ := json.Marshal(map[string]any{
		"c": c, "insolvent": obs.Insolvent, "missing_sats": obs.MissingSats,
		"missing": obs.Missing, "tip": obs.Tip, "digest": obs.Digest,
	})
	return string(raw), true
}

// startBtcSolvencyCheck runs checkpoint bh's check unless the watch is off,
// not yet active, not configured here, or a check is still running.
func (o *Oracle) startBtcSolvencyCheck(bh uint64) {
	if o.hiveCreator == nil || o.stateEngine == nil || o.chainOracle == nil {
		return
	}
	if !consensusversion.BtcSolvencyWatchActive(o.stateEngine.ActiveConsensusVersion(bh)) || o.stateEngine.BtcSolvencyWatchOff() {
		return
	}
	if !o.solvencyRunning.CompareAndSwap(false, true) {
		o.logger.Warn("BTC solvency watch: the previous check is still running; skipping this checkpoint", "c", bh)
		return
	}
	state, l1, done, ok := o.chainOracle.BtcSolvencyInputs()
	if !ok {
		o.solvencyRunning.Store(false)
		return
	}
	go func() {
		defer o.solvencyRunning.Store(false)
		defer done()
		obs, err := chain.ObserveBtcSolvency(state, l1, o.stateEngine.SystemConfig().TssParams().SolvencyGapSats)
		if err != nil {
			o.logger.Warn("BTC solvency watch: abstaining at this checkpoint", "c", bh, "err", err)
			return
		}
		// One line per observed checkpoint (I-40 follow-up, 11-05): a solvent
		// check casts no vote and logged nothing, so neither an operator nor a
		// test could tell a watch that looked from one that never ran.
		o.logger.Info("BTC solvency watch: checkpoint observed", "c", bh, "checked", obs.Checked, "missing", obs.Missing, "tip", obs.Tip)
		if obs.Missing > 0 {
			o.logger.Warn("BTC solvency watch: registered vault UTXOs are spent on Bitcoin without a reservation",
				"c", bh, "missing", obs.Missing, "missing_sats", obs.MissingSats, "digest", obs.Digest, "insolvent", obs.Insolvent)
		}
		vote, send := btcSolvencyVoteFor(bh, obs, o.stateEngine.BtcSolvencyHalted())
		if !send {
			return
		}
		o.broadcastBtcSolvencyVote(bh, vote)
	}()
}

func (o *Oracle) broadcastBtcSolvencyVote(c uint64, vote string) {
	tx := o.hiveCreator.MakeTransaction([]hivego.HiveOperation{hivego.CustomJsonOperation{
		RequiredAuths:        []string{o.conf.Get().HiveUsername},
		RequiredPostingAuths: []string{},
		Id:                   "vsc.btc_solvency",
		Json:                 vote,
	}})
	if err := o.hiveCreator.PopulateSigningProps(&tx, nil); err != nil {
		o.logger.Error("BTC solvency vote: hive tx signing props unavailable; not broadcasting", "c", c, "err", err)
		return
	}
	sig, err := o.hiveCreator.Sign(tx)
	if err != nil {
		o.logger.Error("BTC solvency vote: hive tx signing failed; not broadcasting", "c", c, "err", err)
		return
	}
	tx.AddSig(sig)
	if _, err := o.hiveCreator.Broadcast(tx); err != nil {
		o.logger.Error("BTC solvency vote: broadcast failed", "c", c, "err", err)
		return
	}
	o.logger.Warn("BTC solvency vote broadcast", "c", c, "vote", vote)
}
