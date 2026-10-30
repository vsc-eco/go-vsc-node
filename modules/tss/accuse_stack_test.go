package tss

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"vsc-node/modules/common/consensusversion"
	tss_helpers "vsc-node/modules/tss/helpers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Accusation stacking (0.12.0, TssAccuseStackActive). With one exclusion per
// reshare, two members that attest ready and then fail kept every attempt of a
// key failing: the exclusion alternated between them. Mainnet 0.3 blames
// (epochs 1898-2203) show 32 epochs where two or more such members were missing
// from the reshare that finally landed.

func stackNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("n%02d", i)
	}
	return out
}

func stackParticipants(names []string) []Participant {
	out := make([]Participant, len(names))
	for i, n := range names {
		out[i] = Participant{Account: n}
	}
	return out
}

func TestAccuseStack_Gate(t *testing.T) {
	assert.False(t, consensusversion.TssAccuseStackActive(consensusversion.V0_11_0))
	assert.True(t, consensusversion.TssAccuseStackActive(consensusversion.V0_12_0))
	assert.Equal(t, 1, accuseMaxExclusions, "the old rule is unchanged")
	assert.Equal(t, 2, accuseMaxExclusionsStacked)
}

func TestAccuseStack_CapOrderAndOldMinimum(t *testing.T) {
	all := stackNames(7)
	got := selectAccusedExclusionsStacked(map[string]int{"n04": 1, "n02": 2, "n06": 3}, all, all, 5, 5, 0)
	assert.Equal(t, map[string]bool{"n06": true, "n02": true}, got, "two parties, most-named first")

	tie := selectAccusedExclusionsStacked(map[string]int{"n05": 1, "n03": 1, "n01": 1}, all, all, 5, 5, 0)
	assert.Equal(t, map[string]bool{"n01": true, "n03": true}, tie, "ties break by account")

	// The most-named party is an old member at the old minimum: nobody is left
	// out in its place (unchanged from the one-party rule).
	old := []string{"n00", "n01", "n02"}
	full := selectAccusedExclusionsStacked(map[string]int{"n00": 3, "n05": 1}, old, all, 3, 5, 0)
	assert.Empty(t, full)
	// One fits on the old side, the next does not: stop after the first.
	old4 := []string{"n00", "n01", "n02", "n03"}
	one := selectAccusedExclusionsStacked(map[string]int{"n00": 3, "n01": 2}, old4, all, 3, 5, 0)
	assert.Equal(t, map[string]bool{"n00": true}, one)
	// A party outside the old committee takes no old slot.
	topNew := selectAccusedExclusionsStacked(map[string]int{"n00": 3, "n05": 4}, old4, all, 3, 5, 0)
	assert.Equal(t, map[string]bool{"n05": true, "n00": true}, topNew)
}

func TestAccuseStack_BlameOrBanTakesASlot(t *testing.T) {
	all := stackNames(9)
	counts := map[string]int{"n07": 2, "n08": 1}
	assert.Len(t, selectAccusedExclusionsStacked(counts, all, all, 6, 6, 0), 2)
	assert.Equal(t, map[string]bool{"n07": true}, selectAccusedExclusionsStacked(counts, all, all, 6, 6, 1), "one blamed or banned member leaves one accusation slot")
	assert.Empty(t, selectAccusedExclusionsStacked(counts, all, all, 6, 6, 2))
}

// A node that was accused and then stopped attesting is out already; it must not
// take the slot a node that still attests and fails needs.
func TestAccuseStack_AccusedOffTheListsTakesNoSlot(t *testing.T) {
	ready := stackNames(7)[:6] // n06 accused earlier, no longer ready
	counts := map[string]int{"n06": 5, "n04": 1, "n05": 1}
	got := selectAccusedExclusionsStacked(counts, ready, ready, 4, 4, 0)
	assert.Equal(t, map[string]bool{"n04": true, "n05": true}, got)

	// The one-party rule spends its slot on n06 and leaves both failing nodes in.
	old := selectAccusedExclusions(counts, ready, 4, 0)
	assert.Equal(t, map[string]bool{"n06": true}, old)
}

// Below the stacked rule applyAccusedExclusions drops every exclusion once the
// set would take the new side below its minimum. With a cap of two that would
// put a failing node back in; the stacked selection keeps what fits instead.
func TestAccuseStack_StopsAtNewMinimumKeepsWhatFits(t *testing.T) {
	names := stackNames(16)
	newMin := accuseNewMin(consensusversion.V0_12_0, 16)
	require.Equal(t, 13, newMin)
	ready := stackParticipants(names[:14]) // two of 16 not ready: one new slot
	counts := map[string]int{"n00": 2, "n01": 1}
	_, keepNew, out := applyAccusedExclusions(ready, ready, counts, 16, 0, newMin, true)
	assert.Equal(t, map[string]bool{"n00": true}, out, "the most-named party that fits stays out")
	assert.Len(t, keepNew, 13)

	// Three ready short of the minimum already: nothing can be left out.
	short := stackParticipants(names[:12])
	_, keepNew, out = applyAccusedExclusions(short, short, counts, 16, 0, newMin, true)
	assert.Empty(t, out)
	assert.Len(t, keepNew, 12)

	// An old-only accused party with the new side already short is dropped too
	// (the reshare is refused anyway): same as before.
	oldOnly := append(stackParticipants(names[:12]), Participant{Account: "gone"})
	_, _, out = applyAccusedExclusions(oldOnly, short, map[string]int{"gone": 1}, 13, 0, newMin, true)
	assert.Empty(t, out)
}

// The live testnet case of 2026-10-29 (block 7761400): 6 seats, magi-team-node
// not ready, magi.contracts accused for the restart at 7760700. Leaving it out
// too would take the new side below 5 of 6, so it stays in, under both rules.
func TestAccuseStack_TestnetRestartCase(t *testing.T) {
	names := []string{"magi-team-node", "magi.contracts", "magi.test1", "magi.test2", "magi.test3", "tibfox"}
	ready := stackParticipants(names[1:])
	newMin := accuseNewMin(consensusversion.V0_12_0, len(names))
	require.Equal(t, 5, newMin)
	counts := map[string]int{"magi.contracts": 1}
	for _, stack := range []bool{false, true} {
		_, keepNew, out := applyAccusedExclusions(ready, ready, counts, 6, 0, newMin, stack)
		assert.Empty(t, out, "stack=%v", stack)
		assert.Len(t, keepNew, 5, "stack=%v", stack)
	}
}

// stackRun replays one key's rotation the way the network runs it: the lists are
// the ready seats, every attempt goes through applyAccusedExclusions and the
// call site's pre-flight and threshold floor, a party in failing fails every
// session it is in, and each failed session lands exactly one statement (equal
// weights: bestAccusePerSession takes the smaller routing key, so the
// smaller-named accused). frame, when set, is a liar that is never accused and
// starves the smallest-named honest party in the session instead. Returns the
// attempt that lands (0 = none within max) and the new committee it lands with.
func stackRun(t *testing.T, seats int, notReady, failing []string, frame string, stack bool, max int) (int, []string) {
	t.Helper()
	ver := consensusversion.V0_12_0
	names := stackNames(seats)
	off := map[string]bool{}
	for _, a := range notReady {
		off[a] = true
	}
	var ready []string
	for _, n := range names {
		if !off[n] {
			ready = append(ready, n)
		}
	}
	bad := map[string]bool{}
	for _, a := range failing {
		bad[a] = true
	}
	counts := map[string]int{}
	newMin := accuseNewMin(ver, seats)
	oldThr, _ := tss_helpers.GetThreshold(seats)
	for attempt := 1; attempt <= max; attempt++ {
		list := stackParticipants(ready)
		keepOld, keepNew, _ := applyAccusedExclusions(list, list, counts, seats, 0, newMin, stack)
		newThr, _ := tss_helpers.GetThreshold(len(keepNew))
		if len(keepNew) < newThr+1 || len(keepOld) < oldThr+1 {
			continue
		}
		if consensusversion.TssReshareKeepsThresholdActive(ver) && newThr < reshareThresholdFloor(seats) {
			continue // refused, no session, nothing lands
		}
		in := map[string]bool{}
		for _, p := range keepOld {
			in[p.Account] = true
		}
		for _, p := range keepNew {
			in[p.Account] = true
		}
		var named []string
		for a := range in {
			if bad[a] {
				named = append(named, a)
			}
		}
		if frame != "" && in[frame] {
			named = nil
			for a := range in {
				if a != frame && !bad[a] {
					named = append(named, a)
				}
			}
		}
		if len(named) == 0 {
			return attempt, accountsOf(keepNew)
		}
		sort.Strings(named)
		counts[named[0]]++
	}
	return 0, nil
}

func TestAccuseStack_TwoFailingBothOutByTheThirdAttempt(t *testing.T) {
	for _, seats := range []int{7, 16, 17, 18, 19} {
		failing := []string{"n03", "n05"}
		got, _ := stackRun(t, seats, nil, failing, "", false, 40)
		assert.Zero(t, got, "seats=%d: one party per attempt never lands with two failing (it alternates)", seats)
		got, newCommittee := stackRun(t, seats, nil, failing, "", true, 40)
		assert.Equal(t, 3, got, "seats=%d", seats)
		assert.NotContains(t, newCommittee, "n03")
		assert.NotContains(t, newCommittee, "n05")
		assert.Len(t, newCommittee, seats-2)
	}
	// One failing node: both rules land on the second attempt.
	for _, stack := range []bool{false, true} {
		got, _ := stackRun(t, 16, nil, []string{"n07"}, "", stack, 40)
		assert.Equal(t, 2, got, "stack=%v", stack)
	}
	// Three failing: still more than the cap; the key waits (documented limit).
	got, _ := stackRun(t, 18, nil, []string{"n01", "n02", "n03"}, "", true, 40)
	assert.Zero(t, got)
}

// Every mainnet epoch (BTC key, 0.3 blames, epochs 1898-2203) in which two or
// more blamed members were missing from the reshare that landed: seats, members
// not in the final reshare for other reasons (not ready), members blamed. The
// test below is a simulation with these counts (synthetic names, failing members
// sorted first), not a replay of the landed rows.
var mainnetStallEpochs = []struct {
	epoch, seats, notReady, failing int
}{
	{1899, 16, 0, 4}, {1902, 17, 0, 3}, {1909, 18, 1, 2}, {1914, 18, 2, 3}, {1915, 18, 2, 3},
	{1919, 18, 0, 2}, {1982, 18, 0, 2}, {1984, 18, 0, 2}, {2039, 18, 1, 3}, {2049, 17, 0, 2},
	{2092, 17, 0, 2}, {2095, 17, 0, 3}, {2096, 17, 0, 2}, {2097, 18, 0, 2}, {2100, 18, 0, 2},
	{2101, 18, 0, 3}, {2111, 18, 1, 2}, {2112, 18, 0, 3}, {2113, 18, 0, 2}, {2114, 18, 0, 2},
	{2115, 18, 0, 2}, {2116, 18, 0, 2}, {2117, 18, 0, 2}, {2118, 18, 0, 2}, {2119, 18, 0, 3},
	{2120, 18, 0, 2}, {2121, 18, 0, 2}, {2122, 18, 0, 3}, {2123, 18, 0, 3}, {2166, 17, 0, 3},
	{2174, 17, 0, 3}, {2178, 16, 0, 3},
}

func TestAccuseStack_MainnetCountsSimulation(t *testing.T) {
	landedOld, landedStack := 0, 0
	for _, e := range mainnetStallEpochs {
		names := stackNames(e.seats)
		failing := names[:e.failing]
		notReady := names[e.seats-e.notReady:]
		if got, _ := stackRun(t, e.seats, notReady, failing, "", false, 60); got > 0 {
			landedOld++
		}
		got, _ := stackRun(t, e.seats, notReady, failing, "", true, 60)
		if got > 0 {
			landedStack++
			assert.Equal(t, e.failing+1, got, "epoch %d lands one attempt after the last failing member is accused", e.epoch)
		}
		fits := e.seats-e.notReady-e.failing >= accuseNewMin(consensusversion.V0_12_0, e.seats)
		assert.Equal(t, e.failing <= accuseMaxExclusionsStacked && fits, got > 0, "epoch %d", e.epoch)
	}
	assert.Equal(t, 0, landedOld, "one party per attempt lands none of them")
	assert.Equal(t, 18, landedStack, "stacking lands every two-member epoch")
}

// An honest party with a stale accusation (a one-off failure earlier in the
// rotation) that sorts ahead of the failing members only delays the landing: the
// failing members are named again each time they are let back in, overtake it,
// and both end up out.
func TestAccuseStack_StaleHonestAccusationOnlyDelays(t *testing.T) {
	ver := consensusversion.V0_12_0
	seats := 16
	names := stackNames(seats)
	list := stackParticipants(names)
	newMin := accuseNewMin(ver, seats)
	counts := map[string]int{"n00": 1} // honest, stale
	bad := map[string]bool{"n05": true, "n06": true}
	landed := 0
	for attempt := 1; attempt <= 12 && landed == 0; attempt++ {
		_, keepNew, _ := applyAccusedExclusions(list, list, counts, seats, 0, newMin, true)
		var in []string
		for _, a := range accountsOf(keepNew) {
			if bad[a] {
				in = append(in, a)
			}
		}
		if len(in) == 0 {
			landed = attempt
			assert.Contains(t, accountsOf(keepNew), "n00", "the stale honest party is back in once both failing members are out")
			break
		}
		counts[in[0]]++
	}
	assert.Equal(t, 5, landed, "two attempts later than without the stale accusation")
	got, _ := stackRun(t, seats, nil, []string{"n05", "n06"}, "", true, 12)
	assert.Equal(t, 3, got)
}

// The two builds (one-party rule, stacked rule) pick different parties in more
// than the two-accused case, so a mixed fleet forms different sessions whenever
// any of these holds. Pinned so the rollout note stays true.
func TestAccuseStack_WhereTheBuildsDiffer(t *testing.T) {
	names := stackNames(16)
	newMin := accuseNewMin(consensusversion.V0_12_0, 16)
	all := stackParticipants(names)
	differ := func(old, new []Participant, counts map[string]int, alreadyOut int) bool {
		_, n1, _ := applyAccusedExclusions(old, new, counts, 16, alreadyOut, newMin, false)
		_, n2, _ := applyAccusedExclusions(old, new, counts, 16, alreadyOut, newMin, true)
		return len(n1) != len(n2)
	}
	assert.False(t, differ(all, all, map[string]int{"n05": 1}, 0), "one accused ready party, nothing else live: same choice")
	assert.False(t, differ(all, all, map[string]int{}, 1), "no accusation: same choice")
	assert.True(t, differ(all, all, map[string]int{"n05": 1, "n06": 1}, 0), "two accused")
	assert.True(t, differ(all, all, map[string]int{"n05": 1}, 1), "one accused while a blamed or banned member is out")
	notReady := stackParticipants(names[1:])
	assert.True(t, differ(notReady, notReady, map[string]int{"n00": 3, "n05": 1}, 0), "the most-accused party is no longer ready")
	short := stackParticipants(names[:14]) // one new slot
	assert.False(t, differ(short, short, map[string]int{"n00": 2, "n01": 1}, 0), "only one fits the new side: both builds leave out the most-accused")
}

// Milo's concern: a member that sends its round-1 message to everyone but one
// honest party gets that party accused (everyone else then waits on it), never
// itself. Each attempt it frames another. Stacking lets it keep two honest
// parties out instead of one; this pins exactly what that costs. Signers needed
// for the key once the liar lets the reshare land, against the full committee:
var framingSigners = map[int]struct{ full, one, stacked, floor int }{
	16: {11, 10, 10, 9},
	17: {12, 11, 10, 9},
	18: {12, 12, 11, 10},
	19: {13, 12, 12, 10},
}

func TestAccuseStack_FramingBound(t *testing.T) {
	for seats, want := range framingSigners {
		fullThr, _ := tss_helpers.GetThreshold(seats)
		assert.Equal(t, want.full, fullThr+1, "seats=%d", seats)
		floorSigners := reshareThresholdFloor(seats) + 1
		assert.Equal(t, want.floor, floorSigners, "seats=%d", seats)
		for _, stack := range []bool{false, true} {
			names := stackNames(seats)
			counts := map[string]int{}
			newMin := accuseNewMin(consensusversion.V0_12_0, seats)
			list := stackParticipants(names)
			minSigners, maxOut := seats, 0
			for attempt := 0; attempt < 30; attempt++ {
				_, keepNew, out := applyAccusedExclusions(list, list, counts, seats, 0, newMin, stack)
				thr, _ := tss_helpers.GetThreshold(len(keepNew))
				minSigners = min(minSigners, thr+1)
				maxOut = max(maxOut, len(out))
				assert.False(t, out["n00"], "the liar is never accused")
				in := accountsOf(keepNew)
				sort.Strings(in)
				for _, a := range in {
					if a != "n00" {
						counts[a]++ // the liar starves the smallest-named honest party
						break
					}
				}
			}
			if stack {
				assert.Equal(t, accuseMaxExclusionsStacked, maxOut, "seats=%d", seats)
				assert.Equal(t, want.stacked, minSigners, "seats=%d stacked", seats)
			} else {
				assert.Equal(t, accuseMaxExclusions, maxOut, "seats=%d", seats)
				assert.Equal(t, want.one, minSigners, "seats=%d one", seats)
			}
			assert.GreaterOrEqual(t, minSigners, want.floor, "seats=%d never below the floor", seats)
		}
	}
	// The one-attempt simulation agrees: the liar is never named, so the key
	// never lands while it keeps this up, under either rule.
	for _, stack := range []bool{false, true} {
		got, _ := stackRun(t, 16, nil, nil, "n00", stack, 40)
		assert.Zero(t, got, "stack=%v", stack)
	}
}

// Mainnet is not a clean devnet: members fail one session and recover, drop out
// of a readiness round, and the slot leader (the only publisher) is sometimes
// one that cannot publish. Both rules run the same random history: per rotation
// a set of persistently failing members, per attempt transient failures,
// readiness drop-outs and a random leader. A failed attempt lands one accusation
// only if its leader is in the session and healthy; a successful one lands only
// then too. One rotation is one mainnet epoch (73 attempts of 100 blocks).
// Variants: sticky (the failing members never drop out of readiness: attest and
// withhold) and alreadyOut=1 (one blamed or banned member is off the lists).
func TestAccuseStack_FlakyMainnetSimulation(t *testing.T) {
	const rotations, attempts = 3000, 73
	const pTransient, pNotReady = 0.01, 0.02
	ver := consensusversion.V0_12_0
	type tally struct{ runs, landed, attemptsSum, honestOut int }
	for _, seats := range []int{16, 17, 18} {
		for _, sticky := range []bool{false, true} {
			for _, already := range []int{0, 1} {
				rng := rand.New(rand.NewSource(int64(29102026 + seats)))
				names := stackNames(seats)
				newMin := accuseNewMin(ver, seats)
				oldThr, _ := tss_helpers.GetThreshold(seats)
				res := map[bool]map[int]*tally{false: {}, true: {}}
				oldOnly, maxOut, minLanded := 0, 0, seats
				for r := 0; r < rotations; r++ {
					k := 0 // persistently failing members: mainnet-like mix
					switch x := rng.Float64(); {
					case x < 0.20:
						k = 1
					case x < 0.27:
						k = 2
					case x < 0.30:
						k = 3
					}
					perm := rng.Perm(seats)
					bad := map[string]bool{}
					for _, i := range perm[:k] {
						bad[names[i]] = true
					}
					banned := ""
					if already > 0 {
						banned = names[perm[seats-1]]
					}
					type step struct {
						off, flaky map[string]bool
						leader     string
					}
					hist := make([]step, attempts)
					for a := range hist {
						st := step{off: map[string]bool{}, flaky: map[string]bool{}, leader: names[rng.Intn(seats)]}
						for _, n := range names {
							if rng.Float64() < pNotReady && !(sticky && bad[n]) {
								st.off[n] = true
							}
							if rng.Float64() < pTransient {
								st.flaky[n] = true
							}
						}
						hist[a] = st
					}
					landedAt := map[bool]int{}
					for _, stack := range []bool{false, true} {
						counts := map[string]int{}
						honestOut := false
						for a, st := range hist {
							var ready []Participant
							for _, n := range names {
								if !st.off[n] && n != banned {
									ready = append(ready, Participant{Account: n})
								}
							}
							keepOld, keepNew, out := applyAccusedExclusions(ready, ready, counts, seats, already, newMin, stack)
							maxOut = max(maxOut, len(out))
							newThr, _ := tss_helpers.GetThreshold(len(keepNew))
							if len(keepNew) < newThr+1 || len(keepOld) < oldThr+1 || newThr < reshareThresholdFloor(seats) {
								continue // refused: no session
							}
							in := map[string]bool{}
							for _, p := range keepNew {
								in[p.Account] = true
							}
							publisher := in[st.leader] && !bad[st.leader] && !st.flaky[st.leader]
							var failing []string
							for p := range in {
								if bad[p] || st.flaky[p] {
									failing = append(failing, p)
								}
							}
							if len(failing) == 0 {
								if publisher {
									landedAt[stack] = a + 1
									minLanded = min(minLanded, len(keepNew))
									for x := range out {
										if !bad[x] {
											honestOut = true
										}
									}
									break
								}
								continue // completed but not published (TSS-COMMIT-1)
							}
							if publisher {
								sort.Strings(failing)
								counts[failing[0]]++
							}
						}
						tl := res[stack][k]
						if tl == nil {
							tl = &tally{}
							res[stack][k] = tl
						}
						tl.runs++
						if landedAt[stack] > 0 {
							tl.landed++
							tl.attemptsSum += landedAt[stack]
							if honestOut {
								tl.honestOut++
							}
						}
					}
					if landedAt[false] > 0 && landedAt[true] == 0 {
						oldOnly++
					}
				}
				tag := fmt.Sprintf("seats=%d sticky=%v alreadyOut=%d", seats, sticky, already)
				pct := func(a, b int) float64 {
					if b == 0 {
						return 0
					}
					return 100 * float64(a) / float64(b)
				}
				for k := 0; k <= 3; k++ {
					o, st := res[false][k], res[true][k]
					if o == nil || st == nil {
						continue
					}
					t.Logf("%s failing=%d (%d): one-party lands %.0f%% (honest out %.0f%%), stacked lands %.0f%% (honest out %.0f%%)",
						tag, k, o.runs, pct(o.landed, o.runs), pct(o.honestOut, o.landed), pct(st.landed, st.runs), pct(st.honestOut, st.landed))
					assert.GreaterOrEqual(t, st.landed, o.landed, "%s failing=%d: stacking never lands fewer rotations", tag, k)
					// What the cap can absorb: k + alreadyOut <= 2 lands within the epoch.
					if k+already <= accuseMaxExclusionsStacked {
						assert.GreaterOrEqual(t, pct(st.landed, st.runs), 97.0, "%s failing=%d", tag, k)
					}
					if sticky && k+already > accuseMaxExclusionsStacked {
						assert.LessOrEqual(t, pct(st.landed, st.runs), 5.0, "%s failing=%d: past the cap a sticky withholder still stalls", tag, k)
					}
					if sticky && k+already >= 2 && k >= 1 {
						assert.LessOrEqual(t, pct(o.landed, o.runs), 5.0, "%s failing=%d: the one-party rule stalls", tag, k)
					}
				}
				t.Logf("%s: only the one-party rule landed in %d of %d; smallest committee at a landing %d (minimum %d)", tag, oldOnly, rotations, minLanded, newMin)
				assert.LessOrEqual(t, oldOnly, rotations/200, tag)
				assert.LessOrEqual(t, maxOut, accuseMaxExclusionsStacked)
				assert.GreaterOrEqual(t, minLanded, newMin, tag)
			}
		}
	}
}

// For any inputs the stacked rule removes at least what the one-party rule
// removes from each list, never takes either list below its minimum when it
// started at or above it, and gives the same answer every time.
func TestAccuseStack_NeverRemovesLessAndKeepsMinimums(t *testing.T) {
	rng := rand.New(rand.NewSource(20261029))
	for iter := 0; iter < 20000; iter++ {
		seats := 3 + rng.Intn(18)
		names := stackNames(seats)
		var oldList, newList []Participant
		for _, n := range names {
			if rng.Intn(6) != 0 {
				oldList = append(oldList, Participant{Account: n})
			}
			if rng.Intn(6) != 0 {
				newList = append(newList, Participant{Account: n})
			}
		}
		counts := map[string]int{}
		for k := rng.Intn(5); k > 0; k-- {
			counts[names[rng.Intn(seats)]] += 1 + rng.Intn(3)
		}
		if rng.Intn(4) == 0 {
			counts["departed"] = 1 + rng.Intn(3)
			oldList = append(oldList, Participant{Account: "departed"})
		}
		alreadyOut := rng.Intn(3)
		var newMin int
		if rng.Intn(3) != 0 {
			newMin = accuseNewMin(consensusversion.V0_12_0, seats)
		}
		fullOld := len(oldList) + rng.Intn(3)

		o1, n1, _ := applyAccusedExclusions(oldList, newList, counts, fullOld, alreadyOut, newMin, false)
		o2, n2, out2 := applyAccusedExclusions(oldList, newList, counts, fullOld, alreadyOut, newMin, true)
		assert.Subset(t, accountsOf(o1), accountsOf(o2), "iter %d: old list", iter)
		assert.Subset(t, accountsOf(n1), accountsOf(n2), "iter %d: new list", iter)
		assert.LessOrEqual(t, len(out2)+alreadyOut, max(accuseMaxExclusionsStacked, alreadyOut), "iter %d", iter)
		oldThr, _ := tss_helpers.GetThreshold(fullOld)
		if len(oldList) >= oldThr+1 {
			assert.GreaterOrEqual(t, len(o2), oldThr+1, "iter %d: old minimum", iter)
		}
		if newMin > 0 && len(newList) >= newMin {
			assert.GreaterOrEqual(t, len(n2), newMin, "iter %d: new minimum", iter)
		}
		o3, n3, out3 := applyAccusedExclusions(oldList, newList, counts, fullOld, alreadyOut, newMin, true)
		assert.Equal(t, out2, out3)
		assert.Equal(t, accountsOf(o2), accountsOf(o3))
		assert.Equal(t, accountsOf(n2), accountsOf(n3))
		if t.Failed() {
			t.Fatalf("iter %d: seats=%d old=%v new=%v counts=%v alreadyOut=%d newMin=%d fullOld=%d", iter, seats, accountsOf(oldList), accountsOf(newList), counts, alreadyOut, newMin, fullOld)
		}
	}
}

// Below 0.12.0 nothing changes: stack=false is exactly the one-party selection.
func TestAccuseStack_GateOffIsTheOneParty(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for iter := 0; iter < 5000; iter++ {
		seats := 3 + rng.Intn(18)
		names := stackNames(seats)
		list := stackParticipants(names[:seats-rng.Intn(3)])
		counts := map[string]int{}
		for k := rng.Intn(4); k > 0; k-- {
			counts[names[rng.Intn(seats)]]++
		}
		alreadyOut := rng.Intn(2)
		oldThr, _ := tss_helpers.GetThreshold(seats)
		want := selectAccusedExclusions(counts, accountsOf(list), oldThr+1, alreadyOut)
		_, _, got := applyAccusedExclusions(list, list, counts, seats, alreadyOut, 0, false)
		assert.Equal(t, want, got, "iter %d", iter)
	}
}
