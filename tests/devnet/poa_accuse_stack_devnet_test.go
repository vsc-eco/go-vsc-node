package devnet

import (
	"context"
	"fmt"
	"math/rand"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// adversaryWindowStart waits for the baseline reshare with every seat online and
// the next epoch change after it, and returns the first rotate boundary at least
// `lead` blocks ahead.
func adversaryWindowStart(t *testing.T, d *Devnet, ctx context.Context, nodes, lead int) int {
	t.Helper()
	for n := 1; n <= nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 2, 10*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	waitForCommitment(t, d.MongoURI(), "keygen", 20*time.Minute)
	head0, _ := getHeadBlock(d.HiveRPCEndpoint())
	base, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(head0)}}, 12*time.Minute)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: no baseline reshare with all %d online: %v", nodes, err)
	}
	t.Logf("baseline reshare with all %d at %d (epoch %d)", nodes, base.BlockHeight, base.Epoch)
	for tries := 0; ; tries++ {
		if tries > 400 {
			t.Fatalf("PRECONDITION FAILED: no epoch change after the baseline")
		}
		head, _ := getHeadBlock(d.HiveRPCEndpoint())
		ep, eh := latestElection(t, d, ctx, 1)
		b := nextReshareBoundary(head)
		if ep > base.Epoch && b-head >= lead && eh <= uint64(head) {
			t.Logf("election epoch %d > key epoch %d at head %d; adversary starts at boundary %d", ep, base.Epoch, head, b)
			return b
		}
		devnetSleep(3 * time.Second)
	}
}

var exclusionsLine = regexp.MustCompile(`excluded="?map\[([^\]]*)\]`)

// maxAccusationExclusions is the largest set any node logged for "reshare
// accusation exclusions" (tss=trace logs the excluded map), and whether `who`
// was ever in one.
func maxAccusationExclusions(ctx context.Context, d *Devnet, nodes int, who string) (int, bool) {
	most, hit := 0, false
	for n := 1; n <= nodes; n++ {
		out, _ := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf("docker logs %s 2>&1 | grep -a 'reshare accusation exclusions'", d.containerName(n))).CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			m := exclusionsLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			k := 0
			for _, f := range strings.Fields(m[1]) {
				if strings.HasSuffix(f, ":true") {
					k++
					if strings.TrimSuffix(f, ":true") == who {
						hit = true
					}
				}
			}
			most = max(most, k)
		}
	}
	return most, hit
}

// TestPoaTwoSeatsAttestAndWithholdEverySession (ACCUSE-STACK, 0.12.0): two of
// seven seats are online for every readiness round and silent for every reshare
// session. With one accusation exclusion per reshare (below 0.12.0) every
// attempt keeps one of them in, the exclusion alternates and nothing lands. From
// 0.12.0 the two accusations stack and a reshare without both lands inside the
// window. A/B: POA_DEVNET_FLOOR=11 (expected red), POA_DEVNET_FLOOR=12 (green).
func TestPoaTwoSeatsAttestAndWithholdEverySession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := poaTssConfig(7)
	d, ctx := startDevnet(t, cfg, 110*time.Minute)
	silent := []int{3, 4}
	silentAcct := map[string]bool{}
	for _, n := range silent {
		silentAcct[d.witnessAccount(n)] = true
	}
	t.Cleanup(func() {
		for _, n := range silent {
			_ = d.Reconnect(context.Background(), n)
		}
		dctx, cancel := devnetWithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		all, _ := d.GetCommitments(dctx, 1, bson.M{})
		for _, c := range all {
			t.Logf("commitment %-15s block=%d epoch=%d bits=%s", c.Type, c.BlockHeight, c.Epoch, decodeBitset(t, c.Commitment).Text(2))
		}
		tssDumpLogs(t, d, cfg.Nodes, "timeout result|reshare accusations|accusation exclusions|waitForSigs (OK|failed)|would lower the key threshold|insufficient new participants")
	})

	first := adversaryWindowStart(t, d, ctx, cfg.Nodes, 8)
	const sessions = 10
	var cycles []int
	for b := first; len(cycles) < sessions; b += testRotateInterval {
		if b != first {
			waitForBlock(t, d.HiveRPCEndpoint(), b-10, 3*time.Minute)
			for _, n := range silent {
				if err := d.Reconnect(ctx, n); err != nil {
					t.Logf("reconnect magi-%d before %d: %v", n, b, err)
				}
			}
		}
		waitForBlock(t, d.HiveRPCEndpoint(), b-3, 3*time.Minute)
		for _, n := range silent {
			if err := d.Disconnect(ctx, n); err != nil {
				t.Fatalf("disconnect magi-%d before %d: %v", n, b, err)
			}
		}
		cycles = append(cycles, b)
		t.Logf("cycle %d: seats %v online for readiness, silent from %d", b, silent, b-3)
	}
	last := cycles[len(cycles)-1]
	waitForBlock(t, d.HiveRPCEndpoint(), last+45, 5*time.Minute)

	win := bson.M{"block_height": bson.M{"$gte": uint64(first), "$lte": uint64(last)}}
	docs, _ := d.GetCommitments(ctx, 1, win)
	reshares, accusations, withSilent := 0, map[string]int{}, 0
	for _, c := range docs {
		named := namedIn(t, d, ctx, c)
		t.Logf("in window: %s at %d epoch %d -> %v", c.Type, c.BlockHeight, c.Epoch, named)
		switch c.Type {
		case "reshare":
			reshares++
			for _, a := range named {
				if silentAcct[a] {
					withSilent++
				}
			}
		case "reshare_accuse":
			for _, a := range named {
				accusations[a]++
			}
		}
	}
	most, _ := maxAccusationExclusions(ctx, d, cfg.Nodes, "")
	t.Logf("adversary window %d..%d (%d sessions, floor 0.%d): reshares=%d accusations=%v largest accusation exclusion set=%d", first, last, len(cycles), poaDevnetFloor(), reshares, accusations, most)

	if withSilent > 0 {
		t.Errorf("a reshare inside the window lists a silent seat (%d times): it cannot have completed", withSilent)
	}
	for a, k := range accusations {
		if !silentAcct[a] {
			t.Errorf("an honest seat was accused %d time(s): %s", k, a)
		}
	}
	if most > 2 {
		t.Errorf("an accusation exclusion set of %d: the cap is 2", most)
	}
	if reshares == 0 {
		t.Errorf("two seats that attest ready and then withhold stalled key rotation for all %d sessions (%d..%d) at floor 0.%d", len(cycles), first, last, poaDevnetFloor())
	} else {
		t.Logf("PASS: %d reshare(s) without both silent seats landed inside the window", reshares)
	}

	// Positive control: both stop attesting (stay disconnected) -> a reshare without them lands.
	rec, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(last)}}, 8*time.Minute)
	if err != nil {
		t.Errorf("CONTROL FAILED: no reshare within 8 min after both seats stopped attesting: %v", err)
	} else {
		t.Logf("control: reshare at %d once both stopped attesting", rec.BlockHeight)
	}
}

// starveOne drops node's large (TSS-sized) packets to victim only, the framing
// shape: everyone else gets the node's round-1 message, the victim does not, so
// the victim waits on it and everyone else waits on the victim.
func (d *Devnet) starveOne(ctx context.Context, node, victim int) error {
	ip, err := d.containerIP(ctx, d.containerName(victim))
	if err != nil {
		return fmt.Errorf("IP of magi-%d: %w", victim, err)
	}
	return d.iptables(ctx, d.containerName(node), "-A", "OUTPUT", "-d", ip, "-m", "length", "--length", "1000:65535", "-j", "DROP")
}

// TestPoaFramingStarvesADifferentHonestSeat (ACCUSE-STACK, Milo's review): seat
// 7 sends its reshare messages to everyone but one honest seat, a different one
// each session. The starved seat is the one everyone else waits on, so it is the
// one accused; seat 7 never is. Stacking lets at most two honest seats out of a
// reshare at once, never three, and once seat 7 stops, a reshare lands at or
// above the floor.
func TestPoaFramingStarvesADifferentHonestSeat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := poaTssConfig(7)
	d, ctx := startDevnet(t, cfg, 110*time.Minute)
	const liar = 7
	liarAcct := d.witnessAccount(liar)
	t.Cleanup(func() {
		_ = d.Reconnect(context.Background(), liar)
		tssDumpLogs(t, d, cfg.Nodes, "timeout result|reshare accusations|accusation exclusions|waitForSigs (OK|failed)|would lower the key threshold")
	})

	first := adversaryWindowStart(t, d, ctx, cfg.Nodes, 8)
	victims := []int{1, 2, 3, 4, 5, 6}
	var cycles []int
	for i, b := 0, first; i < 10; i, b = i+1, b+testRotateInterval {
		waitForBlock(t, d.HiveRPCEndpoint(), b-10, 3*time.Minute)
		_ = d.Reconnect(ctx, liar) // clears the previous cycle's rules
		v := victims[i%len(victims)]
		if err := d.starveOne(ctx, liar, v); err != nil {
			t.Fatalf("starve magi-%d before %d: %v", v, b, err)
		}
		cycles = append(cycles, b)
		t.Logf("cycle %d: %s starves %s", b, liarAcct, d.witnessAccount(v))
	}
	last := cycles[len(cycles)-1]
	waitForBlock(t, d.HiveRPCEndpoint(), last+45, 5*time.Minute)
	_ = d.Reconnect(ctx, liar)

	win := bson.M{"block_height": bson.M{"$gte": uint64(first), "$lte": uint64(last + 45)}}
	docs, _ := d.GetCommitments(ctx, 1, win)
	accused := map[string]int{}
	for _, c := range docs {
		named := namedIn(t, d, ctx, c)
		t.Logf("in window: %s at %d epoch %d -> %v", c.Type, c.BlockHeight, c.Epoch, named)
		if c.Type == "reshare_accuse" {
			for _, a := range named {
				accused[a]++
			}
		}
	}
	most, liarOut := maxAccusationExclusions(ctx, d, cfg.Nodes, liarAcct)
	t.Logf("framing window %d..%d (floor 0.%d): accused=%v largest exclusion set=%d liar ever excluded=%v", first, last, poaDevnetFloor(), accused, most, liarOut)
	if accused[liarAcct] > 0 {
		t.Logf("note: the liar was accused %d time(s) (its large-packet drop did not hide it every time)", accused[liarAcct])
	}
	if len(accused) == 0 {
		t.Errorf("no accusation landed: the framing shape was not reproduced")
	}
	if most > 2 {
		t.Errorf("an accusation exclusion set of %d: the cap is 2", most)
	}

	// Control: the liar stops; a reshare lands and keeps at least the floor.
	rec, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(last + 45)}}, 10*time.Minute)
	if err != nil {
		t.Errorf("CONTROL FAILED: no reshare within 10 min after the liar stopped: %v", err)
		return
	}
	parties := namedIn(t, d, ctx, *rec)
	t.Logf("control: reshare at %d with %d parties %v", rec.BlockHeight, len(parties), parties)
	if len(parties) < 5 {
		t.Errorf("control reshare with %d parties: below the 7-seat minimum of 5", len(parties))
	}
}

// devnetAccuseNewMin mirrors accuseNewMin (modules/tss/accuse.go) at 0.12: the
// new committee a reshare needs, the commit quorum or the keep-threshold
// floor's party count, whichever is higher.
func devnetAccuseNewMin(n int) int {
	thr := func(k int) int { return (2*k+2)/3 - 1 } // ceil(2k/3)-1 (tss_helpers.GetThreshold)
	floor := min(n/2, thr(n))
	minParties := floor + 1
	for thr(minParties) < floor {
		minParties++
	}
	return max(thr(n)+1, minParties)
}

// withholdersExcludedByAccusation reports whether any node logged a "reshare
// accusation exclusions" line whose excluded set holds every account in want,
// i.e. they were ready (attested) and left out by accusation, not readiness.
func withholdersExcludedByAccusation(ctx context.Context, d *Devnet, nodes int, want []string) bool {
	for n := 1; n <= nodes; n++ {
		out, _ := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf("docker logs %s 2>&1 | grep -a 'reshare accusation exclusions'", d.containerName(n))).CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			m := exclusionsLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			all := true
			for _, w := range want {
				if !strings.Contains(m[1], w+":true") {
					all = false
				}
			}
			if all {
				return true
			}
		}
	}
	return false
}

// TestAccuseStackMainnetWeather (ACCUSE-STACK under mainnet conditions): 16
// seats (mainnet's size), mainnet TSS timing (a reshare attempt every 100
// blocks, 2-minute timeout, 5 s commit delay, 6 s signature window, readiness
// 30 blocks ahead) and several attempts per epoch (400-block elections: 4
// attempts each; mainnet has 72; accusations carry across epochs until a
// reshare lands, as on mainnet). Two seats attest ready and withhold every
// session (sticky). On top, each session a random honest seat may drop out
// mid-session (a transient failure, so it can be accused once) and another may
// miss the readiness round; the weather is seeded, so A and B see the same
// sequence. Readiness is only gossiped while the key needs a reshare: once one
// lands, the remaining cycles of that epoch are not attempts.
// A/B: POA_DEVNET_FLOOR=11 (one-party rule, expected to stall), 12 (stacked,
// expected to land with both withholders left out BY ACCUSATION).
func TestAccuseStackMainnetWeather(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	const seats, rotate, sessions, election = 16, 100, 9, 400
	cfg := poaTssConfig(seats)
	// 16 GraphQL ports from 18080 would run into the Hive port (18091); move them clear.
	cfg.GQLBasePort = 18100 + devnetPortOffset()
	cfg.MongoMemory = "4G" // one MongoDB for 16 node databases
	cfg.StartBatch = 4     // 16 concurrent index builds crashed MongoDB 8.0.17
	cfg.SysConfigOverrides.ConsensusParams.ElectionInterval = election
	tp := cfg.SysConfigOverrides.TssParams
	tp.RotateInterval = rotate
	tp.SignInterval = 50
	tp.ReadinessOffset = 30
	tp.ReshareTimeout = 2 * time.Minute
	tp.DefaultTimeout = time.Minute
	tp.CommitDelay = 5 * time.Second
	tp.WaitForSigsTimeout = 6 * time.Second
	tp.ReshareSyncDelay = 5 * time.Second
	tp.PreParamsTimeout = 15 * time.Minute
	d, ctx := startDevnet(t, cfg, 260*time.Minute)
	stacked := poaDevnetFloor() >= 12

	withholders := []int{3, 4}
	isW := map[int]bool{3: true, 4: true}
	wNames := []string{d.witnessAccount(3), d.witnessAccount(4)}
	wAcct := map[string]bool{wNames[0]: true, wNames[1]: true}
	var honest []int
	for n := 1; n <= seats; n++ {
		if !isW[n] {
			honest = append(honest, n)
		}
	}
	t.Cleanup(func() {
		for n := 1; n <= seats; n++ {
			_ = d.Reconnect(context.Background(), n)
		}
		tssDumpLogs(t, d, seats, "accusation exclusions|would lower the key threshold|insufficient new participants")
	})

	for n := 1; n <= seats; n++ {
		// Epoch 0 lands near block 150, so epoch 2 is ~2 elections (~800 blocks) later.
		if err := d.waitForElectionEpoch(ctx, n, 2, 100*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	waitForCommitment(t, d.MongoURI(), "keygen", 40*time.Minute)
	head0, _ := getHeadBlock(d.HiveRPCEndpoint())
	base, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(head0)}}, 60*time.Minute)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: no baseline reshare with all %d online: %v", seats, err)
	}
	t.Logf("baseline reshare with all %d at %d (epoch %d)", seats, base.BlockHeight, base.Epoch)
	var first int
	for tries := 0; first == 0; tries++ {
		if tries > 2400 {
			t.Fatalf("PRECONDITION FAILED: no epoch change after the baseline")
		}
		head, _ := getHeadBlock(d.HiveRPCEndpoint())
		ep, eh := latestElection(t, d, ctx, 1)
		b := (head/rotate + 1) * rotate
		if ep > base.Epoch && b-head >= 45 && eh <= uint64(head) {
			first = b
			t.Logf("election epoch %d (at %d) > key epoch %d at head %d; weather starts at boundary %d", ep, eh, base.Epoch, head, b)
		}
		devnetSleep(3 * time.Second)
	}

	rng := rand.New(rand.NewSource(29102026))
	var cycles []int
	for i, b := 0, first; i < sessions; i, b = i+1, b+rotate {
		waitForBlock(t, d.HiveRPCEndpoint(), b-40, 10*time.Minute)
		for n := 1; n <= seats; n++ {
			_ = d.Reconnect(ctx, n) // everyone back for the readiness round
		}
		transient, missReady := 0, 0
		if rng.Float64() < 0.35 {
			transient = honest[rng.Intn(len(honest))]
		}
		if rng.Float64() < 0.20 {
			missReady = honest[rng.Intn(len(honest))]
			if missReady == transient {
				missReady = 0
			}
		}
		if missReady != 0 {
			waitForBlock(t, d.HiveRPCEndpoint(), b-36, 5*time.Minute)
			if err := d.Disconnect(ctx, missReady); err != nil {
				t.Fatalf("disconnect magi-%d: %v", missReady, err)
			}
		}
		waitForBlock(t, d.HiveRPCEndpoint(), b-3, 5*time.Minute)
		for _, n := range withholders {
			if err := d.Disconnect(ctx, n); err != nil {
				t.Fatalf("disconnect magi-%d before %d: %v", n, b, err)
			}
		}
		if transient != 0 {
			if err := d.Disconnect(ctx, transient); err != nil {
				t.Fatalf("disconnect magi-%d: %v", transient, err)
			}
		}
		cycles = append(cycles, b)
		t.Logf("cycle %d: withholders %v silent; transient fault magi-%d; missed readiness magi-%d (election boundary inside b-30..b: %v)",
			b, withholders, transient, missReady, (b-30)/election != b/election || b%election == 0)
	}
	last := cycles[len(cycles)-1]
	waitForBlock(t, d.HiveRPCEndpoint(), last+60, 10*time.Minute)

	win := bson.M{"block_height": bson.M{"$gte": uint64(first), "$lte": uint64(last)}}
	docs, _ := d.GetCommitments(ctx, 1, win)
	reshares, withW, small := 0, 0, 0
	accused := map[string]int{}
	newMin := devnetAccuseNewMin(seats)
	for _, c := range docs {
		named := namedIn(t, d, ctx, c)
		t.Logf("in window: %s at %d epoch %d -> %d %v", c.Type, c.BlockHeight, c.Epoch, len(named), named)
		switch c.Type {
		case "reshare":
			reshares++
			if len(named) < newMin {
				small++
			}
			for _, a := range named {
				if wAcct[a] {
					withW++
				}
			}
		case "reshare_accuse":
			for _, a := range named {
				accused[a]++
			}
		}
	}
	most, _ := maxAccusationExclusions(ctx, d, seats, "")
	byAccusation := withholdersExcludedByAccusation(ctx, d, seats, wNames)
	t.Logf("weather window %d..%d (%d cycles, floor 0.%d): reshares=%d accused=%v largest exclusion set=%d both withholders excluded by accusation in one session=%v (cycles after a landing in the same epoch are not attempts)",
		first, last, len(cycles), poaDevnetFloor(), reshares, accused, most, byAccusation)
	if withW > 0 {
		t.Errorf("a landed reshare lists a withholder (%d)", withW)
	}
	if small > 0 {
		t.Errorf("%d reshare(s) landed below the %d-seat minimum of %d", small, seats, newMin)
	}
	if most > 2 {
		t.Errorf("an accusation exclusion set of %d: the cap is 2", most)
	}
	if stacked {
		if accused[wNames[0]] == 0 || accused[wNames[1]] == 0 {
			t.Errorf("INCONCLUSIVE: a withholder was never accused (%v): it fell out of readiness instead, so stacking was not exercised", accused)
		}
		if !byAccusation {
			t.Errorf("no session left out both withholders by accusation: stacking was not what let a reshare land")
		}
		if reshares == 0 {
			t.Errorf("two sticky withholders in mainnet weather stalled key rotation for all %d cycles (%d..%d) at floor 0.%d", len(cycles), first, last, poaDevnetFloor())
		} else {
			t.Logf("PASS: %d reshare(s) without either withholder landed in mainnet weather", reshares)
		}
	} else {
		if reshares == 0 {
			t.Errorf("EXPECTED RED (one-party rule): two sticky withholders stalled key rotation for all %d cycles (%d..%d)", len(cycles), first, last)
		} else {
			t.Errorf("INCONCLUSIVE A: %d reshare(s) landed under the one-party rule; a withholder must have fallen out of readiness (weather), not been excluded by stacking", reshares)
		}
	}

	// Control: every honest seat back for a full readiness window, withholders
	// stay out (not attesting) -> a reshare without them lands.
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	nb := (head/rotate + 1) * rotate
	if nb-head < 40 {
		nb += rotate
	}
	waitForBlock(t, d.HiveRPCEndpoint(), nb-40, 10*time.Minute)
	for _, n := range honest {
		_ = d.Reconnect(ctx, n)
	}
	for _, n := range withholders {
		_ = d.Disconnect(ctx, n)
	}
	rec, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gte": uint64(nb)}}, 40*time.Minute)
	if err != nil {
		t.Errorf("CONTROL FAILED: no reshare within 40 min after the withholders stopped attesting: %v", err)
	} else {
		t.Logf("control: reshare at %d once the withholders stopped attesting", rec.BlockHeight)
	}
}

// The mirror of accuseNewMin must not drift (values from TestP8_AccuseNewMin and
// the mainnet sizes). Runs in -short.
func TestDevnetAccuseNewMinMirror(t *testing.T) {
	for n, want := range map[int]int{3: 2, 4: 4, 5: 4, 7: 5, 16: 13, 17: 13, 18: 14, 19: 14} {
		if got := devnetAccuseNewMin(n); got != want {
			t.Errorf("devnetAccuseNewMin(%d) = %d, want %d", n, got, want)
		}
	}
}
