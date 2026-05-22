package rewards

import (
	"math/big"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"vsc-node/lib/intmath"
	"vsc-node/modules/incentive-pendulum/oracle"
)

// audit_unfixed_test.go — differential tests for medium-severity audit findings
// that remain unfixed on this branch. Each test demonstrates the bug-precondition
// and PASSES against current code; the trailing comment in each notes how a
// post-fix would flip the assertion (failing the current test once fixed).

// -----------------------------------------------------------------------------
// #58 — Empty blocks earn full production reward.
//
// ScoreBlockProduction treats any slot whose height appears in
// producedSlotHeights as "produced" — there is no signal for whether the block
// contained zero on-chain transactions ("empty" block). A proposer that
// produces only no-op headers, gaining the production reward without
// contributing any actual L2 work, currently incurs no penalty.
//
// Post-fix expectation: an empty block (no L2 transactions) should be treated
// as a miss and incur BlockProductionMissBps. This test would then have a
// non-empty penalty map and the assertion below would flip.
// -----------------------------------------------------------------------------

func TestAuditUnfixed_58_EmptyBlocksEarnFullProductionReward(t *testing.T) {
	committee := []string{"alice", "bob", "carol"}

	// Full tick window: every slot is scheduled and every slot is "produced"
	// — but, conceptually, each of these blocks is EMPTY (no L2 txs). The
	// production scorer has no way to express that, so it returns no
	// penalties.
	slots := []SlotProposer{
		{SlotHeight: 100, Account: "alice"},
		{SlotHeight: 110, Account: "bob"},
		{SlotHeight: 120, Account: "carol"},
		{SlotHeight: 130, Account: "alice"},
		{SlotHeight: 140, Account: "bob"},
		{SlotHeight: 150, Account: "carol"},
	}
	produced := map[uint64]struct{}{
		100: {}, 110: {}, 120: {}, 130: {}, 140: {}, 150: {},
	}

	got := ScoreBlockProduction(slots, produced, committee)

	// Current (buggy) behaviour: empty-but-present blocks produce no penalty
	// for anyone. The map is empty because every slot height in `slots` is
	// present in `produced`.
	if len(got) != 0 {
		t.Fatalf("expected empty penalty map (current bug: production scorer "+
			"cannot detect empty blocks), got %v", got)
	}

	// Post-fix note: once empty blocks are penalised, every committee member
	// should incur 2 × BlockProductionMissBps (two empty blocks each). The
	// assertion above would then fail and the test should be inverted (or
	// renamed) when the fix lands.
}

// -----------------------------------------------------------------------------
// #43 — Trimmed-mean lets a <75% colluding cluster shift price.
//
// TrustedHivePriceBps trims floor(N/4) from each end and means the middle. The
// docstring promises that "a colluding cluster pushing extreme values off any
// single side has to overwhelm 75% of the trusted set" — but because the trim
// is symmetric, a 75%/25% asymmetric cluster can push the kept window entirely
// onto the colluder's value. With N=8 and a 6/2 split (75%/25%), trim=2 drops
// both honest 1.0x quotes off the bottom plus 2 of the colluder's 1.5x quotes
// off the top, leaving 4 colluder quotes at 1.5x in the kept window. Mean =
// 1.5x (50% deviation), far above the >1.3x threshold checked here.
//
// Post-fix expectation (per-finding rec): median-based aggregation would put
// the result at the central quote (1.5x with 6 colluders out of 8) only when
// colluders are >50% — but additionally the recommended fix narrows the trim
// or uses median which (for the 6/2 split here) still equals the dominant
// value, so the cleanest post-fix differential is the inverse split: 2 at
// 1.5x, 6 at 1.0x — median 1.0x even though the mean would be skewed. The
// post-fix variant of this test should construct that <50%-colluder case and
// assert the result is the honest 1.0x value.
// -----------------------------------------------------------------------------

func TestAuditUnfixed_43_TrimmedMeanLetsSubMajorityShiftPrice(t *testing.T) {
	// True/honest price: 1.0 HBD per HIVE → 10_000 bps.
	const truePriceBps = int64(intmath.BpsScale) // 10_000

	// 6 colluders at 1.5× (1500 raw HBD per 1000 raw HIVE), 2 honest at 1.0×.
	// Names sorted lexicographically so sort.Strings in TrustedHivePriceBps
	// is deterministic regardless of map iteration order.
	quotes := map[string]oracle.Quote{
		"c1": {HbdRaw: 1500, HiveRaw: 1000}, // 1.5×
		"c2": {HbdRaw: 1500, HiveRaw: 1000},
		"c3": {HbdRaw: 1500, HiveRaw: 1000},
		"c4": {HbdRaw: 1500, HiveRaw: 1000},
		"c5": {HbdRaw: 1500, HiveRaw: 1000},
		"c6": {HbdRaw: 1500, HiveRaw: 1000},
		"h1": {HbdRaw: 1000, HiveRaw: 1000}, // 1.0× (honest)
		"h2": {HbdRaw: 1000, HiveRaw: 1000},
	}
	trusted := map[string]bool{
		"c1": true, "c2": true, "c3": true, "c4": true, "c5": true, "c6": true,
		"h1": true, "h2": true,
	}

	gotBps, ok := oracle.TrustedHivePriceBps(quotes, trusted)
	if !ok {
		t.Fatalf("TrustedHivePriceBps returned !ok with 8 valid quotes")
	}

	// Sorted prices: [1.0, 1.0, 1.5, 1.5, 1.5, 1.5, 1.5, 1.5] (×10_000 bps)
	// trim = floor(8/4) = 2  → kept = [1.5, 1.5, 1.5, 1.5] → mean = 1.5×.
	// Threshold from the test brief: > 1.3× true price.
	threshold := new(big.Int).Mul(big.NewInt(truePriceBps), big.NewInt(13))
	threshold.Quo(threshold, big.NewInt(10)) // 1.3 × true = 13_000 bps
	got := big.NewInt(gotBps)

	if got.Cmp(threshold) <= 0 {
		t.Fatalf("expected colluders to shift trimmed mean above 1.3× true "+
			"(=%s bps); got %d bps", threshold.String(), gotBps)
	}

	// Sanity: confirm we're really seeing ~1.5× (50% deviation), not just
	// barely over the threshold.
	expectMean := big.NewInt(int64(15_000)) // 1.5 × 10_000
	if got.Cmp(expectMean) != 0 {
		t.Logf("note: trimmed mean = %d bps (expected exactly %s for the "+
			"6/2 collusion split)", gotBps, expectMean.String())
	}

	// Post-fix note: with a median (or a per-finding-recommended
	// 75%-collusion-threshold trim), the 6/2 split here still resolves to
	// 1.5× because the colluders ARE the majority. The cleanest post-fix
	// differential is the symmetric case: 2 colluders at 1.5×, 6 honest at
	// 1.0×, where median == 1.0× but the current trimmed mean would still
	// land in the colluders' favour relative to a true median. When the fix
	// lands, this test should be inverted / replaced with the sub-majority
	// case described above.
}

// -----------------------------------------------------------------------------
// #59 — Dead code: OracleEvidence / SlashBpsRaw have no non-test callers.
//
// The audit flagged the OracleEvidence struct + SlashBpsRaw/SlashBps trio
// (modules/incentive-pendulum/slashing.go) as dead code superseded by the
// per-tick rewards/ aggregator. This test grep-verifies that no production
// (.go non-_test.go) file outside slashing.go itself references those names,
// proving the dead-code condition is still present on this branch.
//
// Post-fix expectation: the symbols are either removed (this test would then
// fail to find the slashing.go reference and should be deleted) or wired into
// a live caller (this test would then find a non-test caller and fail).
// -----------------------------------------------------------------------------

func TestAuditUnfixed_59_OracleEvidenceAndSlashBpsRawHaveNoLiveCallers(t *testing.T) {
	// Locate the repo root relative to this test file. runtime.Caller gives
	// us the absolute path of this _test.go file regardless of where `go
	// test` was invoked from.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// .../modules/incentive-pendulum/rewards/audit_unfixed_test.go → repo root
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))

	// Sanity: go.mod sits at repo root.
	if _, err := exec.LookPath("grep"); err != nil {
		t.Skipf("grep not available on PATH: %v", err)
	}

	for _, sym := range []string{"OracleEvidence", "SlashBpsRaw"} {
		// grep -rn for the symbol across all .go files, then filter out
		// _test.go files and slashing.go itself. Any surviving line means
		// the symbol has a live (production) caller.
		cmd := exec.Command("grep", "-rn", "--include=*.go", sym, repoRoot)
		out, err := cmd.Output()
		// grep exits 1 when no match found — that's a valid (and desired)
		// result for a fully-removed symbol. We only care if exec itself
		// fails; otherwise we parse whatever lines came back.
		if err != nil {
			if exitErr, isExit := err.(*exec.ExitError); isExit && exitErr.ExitCode() == 1 {
				// No matches at all — symbol fully removed; dead-code
				// finding is post-fix-resolved by removal. This branch
				// would fire only if/when the fix lands. Note and continue.
				t.Logf("symbol %q has no occurrences at all — finding "+
					"would be resolved by removal", sym)
				continue
			}
			t.Fatalf("grep failed for %q: %v (output=%q)", sym, err, string(out))
		}

		var liveCallers []string
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if line == "" {
				continue
			}
			// Pull file path off the "path:lineno:content" form.
			colon := strings.Index(line, ":")
			if colon < 0 {
				continue
			}
			path := line[:colon]
			// Skip test files — they're allowed to reference dead code.
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			// Skip the file that DEFINES the symbol.
			if strings.HasSuffix(path, "modules/incentive-pendulum/slashing.go") {
				continue
			}
			liveCallers = append(liveCallers, line)
		}

		if len(liveCallers) != 0 {
			t.Errorf("symbol %q has %d live (non-test, non-self) caller(s):\n%s",
				sym, len(liveCallers), strings.Join(liveCallers, "\n"))
		}
	}

	// Post-fix note: once #59 is addressed by either deletion or wiring
	// SlashBpsRaw into a real call site, one of the loop iterations above
	// will produce either "no occurrences" (delete) or live callers (wire-
	// in), and this test should be removed or inverted accordingly.
}
