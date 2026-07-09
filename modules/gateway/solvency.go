package gateway

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"vsc-node/modules/common"
	"vsc-node/modules/common/params"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// Automatic vault-solvency halt (v0.6.0 vault protections, brief fix 1).
//
// A node-local, NON-DETERMINISTIC monitor: it compares the ledger's expected
// vault liability against the gateway account's observed on-chain balance and,
// on a confirmed shortfall, escalates to the on-chain halt EFFECT (a vsc.halt op
// every node then processes identically). Per the brief's governing constraint,
// the detection (a live balance read) is quarantined from consensus — it runs
// per-node off the block tick and NEVER gates state processing; only its effect
// (vsc.halt) is on-chain and deterministic. Honest nodes each see the depleted
// real balance and trip independently, so this fires even against an out-of-band
// drain, with no vote to win.
//
// Safety posture: false positives are the dominant risk (fr-sync lag, savings
// interest, rounding, in-transit withdrawals), so the monitor is OFF by default,
// ALARMS before it halts, requires N consecutive confirmations (hysteresis), and
// DEGRADES to no action whenever a data source is unavailable. It can never halt
// mainnet unless an operator explicitly opts in AND the liability source + halt
// trigger are wired.

// SolvencyMode is the monitor's escalation posture.
type SolvencyMode int

const (
	SolvencyOff   SolvencyMode = iota // disabled (default) — the monitor does nothing
	SolvencyAlarm                     // detect + log loudly, never halt
	SolvencyHalt                      // detect + broadcast vsc.halt after confirmations
)

// LiabilitySource returns the ledger's EXPECTED gateway liability for an asset
// (total backed L2 balance, in raw ledger units) at a height. ok=false means the
// figure is unavailable (not wired / transient error) and the monitor degrades to
// no action rather than risk a false halt on missing data.
//
// The ledger-backed implementation (fix 1's cross-account liability accounting)
// is ledgerLiabilitySource, wired in MultiSig.Init. The interface stays as the
// seam so tests can substitute a fake and future assets can plug in.
type LiabilitySource interface {
	ExpectedLiability(asset string, blockHeight uint64) (int64, bool)
}

// HaltTrigger escalates a CONFIRMED breach into the on-chain halt effect by
// broadcasting a vsc.halt op signed by THIS node's own committee account. It is
// injected so the block-producer self-broadcast path stays out of the monitor;
// nil means alarm-only even in SolvencyHalt mode.
type HaltTrigger func(bh uint64, reason string)

// solvencyAssets are the gateway-backed assets whose solvency is tracked. Both
// legs map unambiguously to observed on-chain buckets:
//
//	hbd:  L2 liability Σ(HBD+HBD_SAVINGS) ↔ observed liquid+savings HBD. L2
//	      "staking" HBD physically IS moving it into Hive savings (the gateway
//	      emits TransferToSavings for the net stake), so both buckets are covered.
//	hive: L2 liability Σ(Hive+HIVE_CONSENSUS) + protocol-held slash residual ↔
//	      observed liquid(+savings) HIVE. Verified against the ledger flow:
//	      consensus_stake/unstake are LEDGER-ONLY bucket moves — consensus_stake
//	      writes no ActionRecord, and consensus_unstake's action is settled by the
//	      state engine as a ledger credit at the release epoch, never by the
//	      gateway (its batch filter is withdraw/stake/unstake). The gateway never
//	      powers up or savings-moves HIVE, so ALL physical HIVE — liquid, bonded,
//	      and slashed-into-reserve — sits in the gateway's liquid L1 balance.
var solvencyAssets = []string{"hbd", "hive"}

// SolvencyMonitor holds the per-node detection state. Its methods are safe for
// the goroutine BlockTick launches per sample.
type SolvencyMonitor struct {
	mode          SolvencyMode
	gapBps        uint64 // shortfall tolerance as basis points of expected liability (clamped 0..10000)
	confirmations int    // consecutive breaches required before acting (hysteresis)
	interval      uint64 // sample every N L1 blocks

	gatewayWallet string
	accountClient hiveAccountClient
	liability     LiabilitySource
	trigger       HaltTrigger

	running atomic.Bool // in-flight guard: skip a sample if the previous one is still running

	mu        sync.Mutex
	breach    map[string]int  // consecutive-breach counter per asset
	triggered map[string]bool // latch: a halt was already broadcast for this breach episode
}

// NewSolvencyMonitorFromEnv builds the monitor from environment configuration,
// or returns nil when VSC_SOLVENCY_MONITOR is unset/off (the default). Knobs:
//
//	VSC_SOLVENCY_MONITOR       off | alarm | halt   (default off)
//	VSC_SOLVENCY_GAP_BPS       shortfall tolerance in bps of liability (default 100 = 1%)
//	VSC_SOLVENCY_CONFIRMATIONS consecutive breaches before acting (default 3)
//	VSC_SOLVENCY_INTERVAL      sample period in blocks (default SYNC_INTERVAL)
//
// liability and trigger may be nil; with a nil liability the monitor logs a
// degraded cycle and takes no action, and with a nil trigger it stays alarm-only.
func NewSolvencyMonitorFromEnv(gatewayWallet string, accountClient hiveAccountClient, liability LiabilitySource, trigger HaltTrigger) *SolvencyMonitor {
	mode := SolvencyOff
	switch strings.ToLower(os.Getenv("VSC_SOLVENCY_MONITOR")) {
	case "alarm":
		mode = SolvencyAlarm
	case "halt":
		mode = SolvencyHalt
	}
	if mode == SolvencyOff {
		return nil
	}

	gapBps := uint64(100) // 1%
	if v := os.Getenv("VSC_SOLVENCY_GAP_BPS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			gapBps = n
		}
	}
	if gapBps > 10000 { // cap at 100% so the tolerance math cannot overflow
		gapBps = 10000
	}
	confirmations := 3
	if v := os.Getenv("VSC_SOLVENCY_CONFIRMATIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			confirmations = n
		}
	}
	interval := SYNC_INTERVAL
	if v := os.Getenv("VSC_SOLVENCY_INTERVAL"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			interval = n
		}
	}

	log.Warn("solvency monitor ENABLED",
		"mode", mode, "gapBps", gapBps, "confirmations", confirmations, "intervalBlocks", interval,
		"liabilitySourceWired", liability != nil, "haltTriggerWired", trigger != nil)

	return &SolvencyMonitor{
		mode:          mode,
		gapBps:        gapBps,
		confirmations: confirmations,
		interval:      interval,
		gatewayWallet: gatewayWallet,
		accountClient: accountClient,
		liability:     liability,
		trigger:       trigger,
		breach:        make(map[string]int),
		triggered:     make(map[string]bool),
	}
}

// Check runs one detection cycle. Safe to call as a goroutine; never blocks the
// consensus path and never mutates chain state (only, via the trigger, broadcasts
// a vsc.halt that the chain then processes normally).
func (m *SolvencyMonitor) Check(bh uint64) {
	if m == nil || m.mode == SolvencyOff {
		return
	}
	// In-flight guard: if a previous sample is still running (e.g. a slow balance
	// scan under a too-small interval), skip this one so overlapping goroutines
	// cannot inflate the consecutive-breach counter from a single balance snapshot.
	if !m.running.CompareAndSwap(false, true) {
		log.Warn("solvency monitor: previous sample still running — skipping this cycle", "bh", bh)
		return
	}
	defer m.running.Store(false)

	if m.liability == nil {
		log.Warn("solvency monitor: no liability source wired — detection inert this cycle "+
			"(wire a LiabilitySource to enable fix 1)", "bh", bh)
		return
	}
	observed, ok := m.observedBalances()
	if !ok {
		// Brief: if the balance read is unavailable, detection degrades — do not
		// act on missing data.
		log.Warn("solvency monitor: on-chain balance read unavailable — detection degraded this cycle", "bh", bh)
		return
	}

	for _, asset := range solvencyAssets {
		exp, lok := m.liability.ExpectedLiability(asset, bh)
		if !lok {
			log.Warn("solvency monitor: expected-liability unavailable — degraded", "asset", asset, "bh", bh)
			continue
		}
		obs := observed[asset]
		// tolerance = gapBps of expected liability, computed EXACTLY and
		// overflow-safe: the quotient term keeps the large factor small, the
		// remainder term restores the precision the old divide-first lost (which
		// truncated tol to 0 for any liability < 10000 raw units).
		tol := exp/10000*int64(m.gapBps) + (exp%10000)*int64(m.gapBps)/10000
		shortfall := exp - obs

		if shortfall <= tol {
			// Solvent: reset the counter (hysteresis needs CONSECUTIVE breaches)
			// and clear the fire latch so a future episode can halt again.
			m.mu.Lock()
			cleared := m.breach[asset] != 0 || m.triggered[asset]
			m.breach[asset] = 0
			m.triggered[asset] = false
			m.mu.Unlock()
			if cleared {
				log.Info("solvency monitor: shortfall cleared", "asset", asset, "expected", exp, "observed", obs, "bh", bh)
			}
			continue
		}

		m.mu.Lock()
		m.breach[asset]++
		n := m.breach[asset]
		m.mu.Unlock()

		log.Warn("SOLVENCY ALARM: gateway shortfall detected",
			"asset", asset, "expected", exp, "observed", obs, "shortfall", shortfall,
			"toleranceBps", m.gapBps, "consecutive", n, "confirmations", m.confirmations, "bh", bh)

		if n < m.confirmations {
			continue
		}
		if m.mode != SolvencyHalt || m.trigger == nil {
			log.Error("SOLVENCY: confirmations reached but NOT halting "+
				"(mode is alarm-only or no halt trigger wired)", "asset", asset, "bh", bh)
			continue
		}
		// Halt mode: broadcast vsc.halt at most ONCE per breach episode. The latch
		// clears only on a solvent cycle, so a sustained shortfall does not re-post
		// a signed (and cooldown-rejected) vsc.halt every interval, draining RC.
		m.mu.Lock()
		alreadyFired := m.triggered[asset]
		m.triggered[asset] = true
		m.mu.Unlock()
		if alreadyFired {
			continue
		}
		reason := "solvency shortfall " + asset + " " + strconv.FormatInt(shortfall, 10)
		log.Error("SOLVENCY HALT: broadcasting vsc.halt", "asset", asset, "shortfall", shortfall, "bh", bh)
		m.trigger(bh, reason)
	}
}

// observedBalances reads the gateway account's on-chain HBD and HIVE, INCLUDING
// savings (the cold reserve lives there), and returns them in raw ledger units.
func (m *SolvencyMonitor) observedBalances() (map[string]int64, bool) {
	if m.accountClient == nil {
		return nil, false
	}
	accs, err := m.accountClient.GetAccount([]string{m.gatewayWallet})
	if err != nil || len(accs) == 0 {
		return nil, false
	}
	a := accs[0]
	out := make(map[string]int64, len(solvencyAssets))
	for _, asset := range solvencyAssets {
		var v int64
		var ok bool
		switch asset {
		case "hbd":
			v, ok = sumHiveAmounts("hbd", a.HbdBalance, a.SavingsHbdBalance)
		case "hive":
			v, ok = sumHiveAmounts("hive", a.Balance, a.SavingsBalance)
		default:
			ok = false
		}
		if !ok {
			return nil, false // degrade on a missing/unparseable TRACKED balance
		}
		out[asset] = v
	}
	return out, true
}

// sumHiveAmounts parses and sums several "1.234 SYMBOL" balance strings for one asset.
func sumHiveAmounts(asset string, parts ...string) (int64, bool) {
	var total int64
	for _, p := range parts {
		v, ok := parseHiveAmount(p, asset)
		if !ok {
			return 0, false
		}
		total += v
	}
	return total, true
}

// parseHiveAmount converts a Hive balance string like "1.234 HBD" to raw ledger
// units, dropping the trailing symbol.
func parseHiveAmount(s, asset string) (int64, bool) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, false
	}
	v, err := common.ParseAssetAmount(f[0], asset)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ledgerLiabilitySource is the real LiabilitySource (brief fix 1's cross-account
// liability accounting): it sums the gateway's expected on-chain holdings from
// the L2 ledger balance snapshot, plus (for HIVE) the protocol-held slash
// residual the snapshot excludes. Per asset:
//
//	hbd  = Σ(HBD + HBD_SAVINGS)      L2 HBD (liquid + staked-in-savings) is backed
//	                                 by the gateway's HBD liquid + savings balance.
//	hive = Σ(Hive + HIVE_CONSENSUS)  L2 HIVE (liquid + consensus-staked) is backed
//	     + protocolHeldHive          by the gateway's LIQUID balance — verified:
//	                                 consensus_stake/unstake never move HIVE at L1
//	                                 (ledger-only bucket moves; the unstake action
//	                                 settles as a state-engine ledger credit, not a
//	                                 gateway op), and the gateway never powers up
//	                                 or savings-moves HIVE.
//
// Balances.GetAll scans the whole balance collection, so the monitor samples it
// on a long interval (default SYNC_INTERVAL ~6h), off the consensus path. The
// figure is a heuristic tripwire compared within the gap tolerance, not a
// consensus value, so a slightly-stale snapshot is acceptable.
//
// Known under-count (safe direction): HIVE in a consensus_unstake maturation
// window (bond already debited, release not yet credited) is in neither bucket,
// so expected dips below observed — surplus, which can never false-halt.
type ledgerLiabilitySource struct {
	balanceDb ledgerDb.Balances
	// ledgerRecords reads the protocol slash accounts' meta rows (excluded from
	// the balance snapshot). nil degrades the HIVE leg only; HBD is unaffected.
	ledgerRecords ledgerDb.Ledger
}

func newLedgerLiabilitySource(balanceDb ledgerDb.Balances, ledgerRecords ledgerDb.Ledger) *ledgerLiabilitySource {
	if balanceDb == nil {
		return nil
	}
	return &ledgerLiabilitySource{balanceDb: balanceDb, ledgerRecords: ledgerRecords}
}

func (s *ledgerLiabilitySource) ExpectedLiability(asset string, blockHeight uint64) (int64, bool) {
	if s == nil || s.balanceDb == nil {
		return 0, false
	}
	recs, err := s.balanceDb.GetAll(blockHeight)
	if err != nil {
		return 0, false
	}
	var total int64
	for _, r := range recs {
		switch asset {
		case "hbd":
			total += r.HBD + r.HBD_SAVINGS
		case "hive":
			total += r.Hive + r.HIVE_CONSENSUS
		default:
			return 0, false
		}
	}
	if asset == "hive" {
		// Slash residual leaves the balance snapshot (protocol-meta rows) but the
		// physical HIVE stays on the gateway account. Without this term every
		// finalized slash would open a permanent expected<observed gap that masks
		// a REAL shortfall of the same size.
		held, ok := s.protocolHeldHive(blockHeight)
		if !ok {
			return 0, false // degrade the HIVE leg; never compare a partial figure
		}
		total += held
	}
	return total, true
}

// protocolHeldHive sums slash-residual HIVE held on the protocol accounts at a
// height — value that left user/node balance records (the rows are
// IsProtocolMetaLedgerType, excluded from the snapshot) while the physical HIVE
// remained in the gateway's L1 balance:
//
//	reserve  = Σ SafetySlashReserve (credits) + Σ ReservePayoutDebit (negative)
//	           — mirrors ledgerSystem.ReserveAvailable, height-bounded. A payout's
//	           paired recipient credit is spendable and re-enters the snapshot, so
//	           the payout is conserved across the two terms.
//	pending  = Σ SafetySlashHiveBurnPending (credits) + Σ ...PendingRelease
//	           (negative on cancel or maturity-promotion) — live in-flight
//	           residual; markers are Amount=0 rows and excluded by op-type.
//
// Every slashed unit is in exactly one of {re-credited bond, pending, reserve}:
// the reverse path caps bond re-credits by the amount committed here (see
// applySafetySlashReverse's headroom), so this can never double-count against
// the snapshot's HIVE_CONSENSUS. ok=false on any read error — the monitor
// degrades rather than act on a partial figure (never blocks: unlike the
// consensus-path readers of these rows, the monitor must not fail-stop).
func (s *ledgerLiabilitySource) protocolHeldHive(blockHeight uint64) (int64, bool) {
	if s.ledgerRecords == nil {
		return 0, false
	}
	var total int64
	for _, q := range []struct {
		account string
		opTypes []string
	}{
		{params.ProtocolSlashReserveAccount, []string{
			ledgerSystem.LedgerTypeSafetySlashReserve,
			ledgerSystem.LedgerTypeReservePayoutDebit,
		}},
		{params.ProtocolSlashPendingBurnAccount, []string{
			ledgerSystem.LedgerTypeSafetySlashHiveBurnPending,
			ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingRelease,
		}},
	} {
		recs, err := s.ledgerRecords.GetLedgerRange(q.account, 0, blockHeight, "hive",
			ledgerDb.LedgerOptions{OpType: q.opTypes})
		if err != nil || recs == nil {
			return 0, false
		}
		for _, r := range *recs {
			total += r.Amount
		}
	}
	return total, true
}
