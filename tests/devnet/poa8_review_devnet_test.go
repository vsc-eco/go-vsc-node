package devnet

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/big"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// Devnet scenarios for the review fixes to the per-accused reshare statements
// (POA-8). Each runs at the 0.9.0 floor; the same file on a tree without the
// fixes shows the finding.

// tssLeaderAt is the TSS leader for a block: the account of the schedule slot
// that starts at or below it (tss.go BlockTick: CalculateSlotInfo(bh).StartHeight).
func tssLeaderAt(t *testing.T, d *Devnet, ctx context.Context, bh int) string {
	t.Helper()
	const q = `query($h:Uint64!){witnessSchedule(height:$h){account bn}}`
	var out struct {
		WitnessSchedule []struct {
			Account string `json:"account"`
			Bn      uint64 `json:"bn"`
		} `json:"witnessSchedule"`
	}
	for tries := 0; ; tries++ {
		err := d.gqlQuery(ctx, 1, q, map[string]any{"h": uint64(bh)}, &out)
		if err == nil && len(out.WitnessSchedule) > 0 {
			break
		}
		if tries > 10 {
			t.Fatalf("witnessSchedule(%d): %v (%d slots)", bh, err, len(out.WitnessSchedule))
		}
		time.Sleep(2 * time.Second)
	}
	leader, best := "", uint64(0)
	for _, s := range out.WitnessSchedule {
		if s.Bn <= uint64(bh) && s.Bn >= best {
			leader, best = s.Account, s.Bn
		}
	}
	return strings.TrimPrefix(leader, "hive:")
}

// nodeOfAccount maps a witness account back to its devnet node number.
func nodeOfAccount(d *Devnet, account string) int {
	for n := 1; n <= d.cfg.Nodes; n++ {
		if d.witnessAccount(n) == strings.TrimPrefix(account, "hive:") {
			return n
		}
	}
	return 0
}

// feedOnly makes node send its data only to keep: its packets of 1000 bytes and
// more to every other magi peer are dropped (rules on node's own container), so
// its TSS messages (split into full-size segments) never arrive, while it still
// hears everyone and acknowledgements and connection handshakes still flow. A withholder cut
// off in both directions cannot follow the rounds at all, so the leader ends
// up waiting on it too and names it; this one-way cut is the attack in the
// review (feed only the leader, keep up with everyone). Reconnect(node) undoes it.
func (d *Devnet) feedOnly(ctx context.Context, node, keep int) error {
	name := d.containerName(node)
	for peer := 1; peer <= d.cfg.Nodes; peer++ {
		if peer == node || peer == keep {
			continue
		}
		ip, err := d.containerIP(ctx, d.containerName(peer))
		if err != nil {
			return fmt.Errorf("IP of magi-%d: %w", peer, err)
		}
		if err := d.iptables(ctx, name, "-A", "OUTPUT", "-d", ip, "-m", "length", "--length", "1000:65535", "-j", "DROP"); err != nil {
			return err
		}
	}
	return nil
}

// leaderAccused is the accused set the leader's own node logged for the
// session at bh ("reshare accusations"), and whether it logged one.
func leaderAccused(ctx context.Context, d *Devnet, node, bh int) ([]string, bool) {
	out, _ := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf(
		"docker logs %s 2>&1 | grep 'reshare accusations' | grep -o 'sessionId=reshare-%d-0-test-key-main[^ ]* .*accused=\"\\?\\[[^]]*\\]' | head -1", d.containerName(node), bh)).CombinedOutput()
	return parseAccused(string(out))
}

// parseAccused reads the accused list off a "reshare accusations" line. The
// logger quotes the value only when it has a space, so a list of one name is
// logged as accused=[magi.test7] and a longer one as accused="[a b]".
func parseAccused(line string) ([]string, bool) {
	i := strings.Index(line, "accused=")
	if i < 0 {
		return nil, false
	}
	v := strings.TrimPrefix(line[i+len("accused="):], "\"")
	if !strings.HasPrefix(v, "[") {
		return nil, false
	}
	end := strings.Index(v, "]")
	if end < 0 {
		return nil, false
	}
	return strings.Fields(v[1:end]), true
}

// ledSession reports whether node broadcast the commitments of the session at
// bh, i.e. it really was that session's leader.
func ledSession(ctx context.Context, d *Devnet, node, bh int) bool {
	out, _ := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf(
		"docker logs %s 2>&1 | grep -cE 'broadcasting commitment to Hive.*blockHeight=%d( |$)'", d.containerName(node), bh)).CombinedOutput()
	return strings.TrimSpace(string(out)) != "0"
}

// namedIn decodes a commitment's bitset against its own election.
func namedIn(t *testing.T, d *Devnet, ctx context.Context, c TssCommitmentDoc) []string {
	t.Helper()
	mem, _ := d.GetElectionMembers(ctx, 1, c.Epoch)
	bits := decodeBitset(t, c.Commitment)
	var named []string
	for j, m := range mem {
		if bits.Bit(j) == 1 {
			named = append(named, strings.TrimPrefix(m, "hive:"))
		}
	}
	return named
}

// assertAccusationsInOwnTx: every landed reshare_accuse row came in a Hive
// transaction of its own, never together with a session commitment (#253
// review fix: a refused accusation op must not take the session commitments
// with it). Returns how many accusation rows it checked.
func assertAccusationsInOwnTx(t *testing.T, docs []TssCommitmentDoc) int {
	t.Helper()
	txTypes := make(map[string]map[string]bool)
	for _, c := range docs {
		if txTypes[c.TxId] == nil {
			txTypes[c.TxId] = make(map[string]bool)
		}
		txTypes[c.TxId][c.Type] = true
	}
	checked := 0
	for _, c := range docs {
		if c.Type != "reshare_accuse" {
			continue
		}
		checked++
		for typ := range txTypes[c.TxId] {
			if typ != "reshare_accuse" {
				t.Errorf("accusation at %d shares Hive tx %s with a %s commitment", c.BlockHeight, c.TxId, typ)
			}
		}
	}
	return checked
}

func tssDumpLogs(t *testing.T, d *Devnet, nodes int, pattern string) {
	dctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for n := 1; n <= nodes; n++ {
		out, _ := exec.CommandContext(dctx, "bash", "-c", fmt.Sprintf("docker logs %s 2>&1 | grep -E '%s' | tail -60", d.containerName(n), pattern)).CombinedOutput()
		t.Logf("magi-%d:\n%s", n, string(out))
	}
}

func poaTssConfig(nodes int) *Config {
	cfg := tssTestConfig()
	cfg.Nodes = nodes
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = 1
	return cfg
}

// TestPoaWithholderFeedsOnlyTheLeader (#253 review, point 3): a seat attests
// ready, then sends its reshare messages only to the leader of each session
// (the leader is predictable from the schedule). The leader then waits on every
// other party and the others wait on the withholder. Before the fix the leader
// asked only about the parties it waited on itself, so the statement every
// other node would sign (naming the withholder) was never asked for and
// rotation stalled. With 7 seats the 5 other nodes carry it (50 of 70).
func TestPoaWithholderFeedsOnlyTheLeader(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := poaTssConfig(7)
	d, ctx := startDevnet(t, cfg, 100*time.Minute)
	const w = 7
	wAcct := d.witnessAccount(w)
	t.Cleanup(func() {
		_ = d.Reconnect(context.Background(), w)
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		all, _ := d.GetCommitments(dctx, 1, bson.M{})
		for _, c := range all {
			t.Logf("commitment %-15s block=%d epoch=%d tx=%s bits=%s", c.Type, c.BlockHeight, c.Epoch, c.TxId, decodeBitset(t, c.Commitment).Text(2))
		}
		tssDumpLogs(t, d, cfg.Nodes, "timeout result|reshare accusations|accusation exclusions|waitForSigs (OK|failed)|Hive broadcast")
	})

	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 2, 10*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	waitForCommitment(t, d.MongoURI(), "keygen", 20*time.Minute)
	head0, _ := getHeadBlock(d.HiveRPCEndpoint())
	base, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(head0)}}, 12*time.Minute)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: no baseline reshare with all %d online: %v", cfg.Nodes, err)
	}
	t.Logf("baseline reshare with all %d at %d (epoch %d)", cfg.Nodes, base.BlockHeight, base.Epoch)

	var first int
	for tries := 0; first == 0; tries++ {
		if tries > 400 {
			t.Fatalf("PRECONDITION FAILED: no epoch change after the baseline")
		}
		head, _ := getHeadBlock(d.HiveRPCEndpoint())
		ep, eh := latestElection(t, d, ctx, 1)
		b := nextReshareBoundary(head)
		if ep > base.Epoch && b-head >= 8 && eh <= uint64(head) {
			first = b
		}
		time.Sleep(3 * time.Second)
	}

	type cycle struct {
		b      int
		leader string
		fed    bool
	}
	var cycles []cycle
	for b := first; len(cycles) < 12; b += testRotateInterval {
		if b != first {
			waitForBlock(t, d.HiveRPCEndpoint(), b-10, 3*time.Minute)
			if err := d.Reconnect(ctx, w); err != nil {
				t.Logf("reconnect before %d: %v", b, err)
			}
		}
		waitForBlock(t, d.HiveRPCEndpoint(), b-3, 3*time.Minute)
		leader := tssLeaderAt(t, d, ctx, b)
		ln := nodeOfAccount(d, leader)
		if ln == 0 || ln == w {
			// The withholder leads this one: plain silence, not part of the count.
			if err := d.Disconnect(ctx, w); err != nil {
				t.Fatalf("disconnect before %d: %v", b, err)
			}
			cycles = append(cycles, cycle{b, leader, false})
			t.Logf("cycle %d: leader %s is the withholder (or unknown): silent, not counted", b, leader)
			continue
		}
		if err := d.feedOnly(ctx, w, ln); err != nil {
			t.Fatalf("feedOnly before %d: %v", b, err)
		}
		cycles = append(cycles, cycle{b, leader, true})
		t.Logf("cycle %d: %s attested, now talks only to the leader %s (magi-%d)", b, wAcct, leader, ln)
	}
	last := cycles[len(cycles)-1].b
	waitForBlock(t, d.HiveRPCEndpoint(), last+45, 6*time.Minute)
	_ = d.Reconnect(ctx, w)

	// A leader-fed session counts only if the leader itself did not name the
	// withholder (the setup worked: the leader got all of its messages).
	fedAt := make(map[uint64]bool)
	fedN := 0
	for _, c := range cycles {
		if !c.fed {
			continue
		}
		if !ledSession(ctx, d, nodeOfAccount(d, c.leader), c.b) {
			t.Logf("cycle %d: %s did not broadcast this session's commitments (not its leader): not counted", c.b, c.leader)
			continue
		}
		acc, logged := leaderAccused(ctx, d, nodeOfAccount(d, c.leader), c.b)
		if !logged {
			// No accused list from the node we fed: it may not have led this
			// session (the schedule can shift when an election lands).
			t.Logf("cycle %d: %s logged no accused list: not counted", c.b, c.leader)
			continue
		}
		if sliceHas(acc, wAcct) {
			t.Logf("cycle %d: the leader %s named %s itself (%v): not a leader-fed session, not counted", c.b, c.leader, wAcct, acc)
			continue
		}
		t.Logf("cycle %d: the leader %s did not name %s (its accused: %v)", c.b, c.leader, wAcct, acc)
		fedAt[uint64(c.b)] = true
		fedN++
	}
	docs, _ := d.GetCommitments(ctx, 1, bson.M{"block_height": bson.M{"$gte": uint64(first), "$lte": uint64(last)}})
	reshares, namedFed, namedAny := 0, 0, 0
	for _, c := range docs {
		named := namedIn(t, d, ctx, c)
		t.Logf("in window: %s at %d epoch %d tx %s -> %v", c.Type, c.BlockHeight, c.Epoch, c.TxId, named)
		switch c.Type {
		case "reshare":
			reshares++
		case "reshare_accuse":
			for _, a := range named {
				if a == wAcct {
					namedAny++
					if fedAt[c.BlockHeight] {
						namedFed++
					}
				}
			}
		}
	}
	all, _ := d.GetCommitments(ctx, 1, bson.M{})
	own := assertAccusationsInOwnTx(t, all)
	t.Logf("window %d..%d: %d leader-fed sessions, reshares=%d, statements naming %s: %d (in leader-fed sessions: %d), accusation rows checked for own tx: %d",
		first, last, fedN, reshares, wAcct, namedAny, namedFed, own)
	if fedN == 0 {
		t.Fatalf("INCONCLUSIVE: no session where the withholder fed only the leader and the leader did not name it")
	}
	if namedFed == 0 {
		t.Errorf("a withholder that feeds only the leader was never named in %d leader-fed sessions: the leader never asked about it", fedN)
	}
	if reshares == 0 {
		t.Errorf("no reshare landed in the window: rotation stalled")
	}
}

// TestPoaAccusedOldMemberAtMinimumExcludesNobody (#253 review, point 5): the
// most-named accused party is an old-committee member that cannot be left out
// (the old committee that runs is exactly threshold+1). Before the fix the
// selection fell through to the next, less-named party and left it out
// instead; now nobody is left out. The accusation counts are written directly
// into every node's database (the same rows on all nodes), because arranging
// them through real failures is not reproducible.
//
// Old key: reshare R1 without magi-5 (so magi-5 is new-only). At the next
// session magi-4 is not ready: the old committee that runs is {1,2,3} =
// threshold(4)+1, the new committee is {1,2,3,5}. Counts: magi-1 x2 (old),
// magi-5 x1 (new-only).
func TestPoaAccusedOldMemberAtMinimumExcludesNobody(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := poaTssConfig(5)
	d, ctx := startDevnet(t, cfg, 75*time.Minute)
	t.Cleanup(func() {
		_ = d.Reconnect(context.Background(), 4)
		_ = d.Reconnect(context.Background(), 5)
		tssDumpLogs(t, d, cfg.Nodes, "reshare participant selection|accusation exclusions|excluding")
	})
	a, b := d.witnessAccount(1), d.witnessAccount(5)

	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 2, 10*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	t.Logf("keygen at %d", keygen.BlockHeight)

	// R1 without magi-5.
	if err := d.Disconnect(ctx, 5); err != nil {
		t.Fatalf("disconnect magi-5: %v", err)
	}
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	var r1 *TssCommitmentDoc
	deadline := time.Now().Add(15 * time.Minute)
	for r1 == nil && time.Now().Before(deadline) {
		c, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(head + 8)}}, 5*time.Minute)
		if err != nil {
			continue
		}
		named := namedIn(t, d, ctx, *c)
		if len(named) == 4 && !sliceHas(named, b) {
			r1 = c
			break
		}
		head = int(c.BlockHeight)
		t.Logf("reshare at %d named %v; waiting for one without %s", c.BlockHeight, named, b)
	}
	if r1 == nil {
		t.Fatalf("PRECONDITION FAILED: no reshare without %s", b)
	}
	t.Logf("R1 at %d epoch %d without %s", r1.BlockHeight, r1.Epoch, b)

	// Next session: magi-4 not ready, magi-5 back.
	if err := d.Disconnect(ctx, 4); err != nil {
		t.Fatalf("disconnect magi-4: %v", err)
	}
	if err := d.Reconnect(ctx, 5); err != nil {
		t.Fatalf("reconnect magi-5: %v", err)
	}
	ep, _ := latestElection(t, d, ctx, 1)
	mem, err := d.GetElectionMembers(ctx, 1, ep)
	if err != nil {
		t.Fatalf("members of epoch %d: %v", ep, err)
	}
	idx := func(acct string) int {
		for i, m := range mem {
			if strings.TrimPrefix(m, "hive:") == acct {
				return i
			}
		}
		t.Fatalf("%s not in election %d (%v)", acct, ep, mem)
		return -1
	}
	enc := func(i int) string {
		return base64.RawURLEncoding.EncodeToString(new(big.Int).SetBit(new(big.Int), i, 1).Bytes())
	}
	rows := []bson.M{
		{"type": "reshare_accuse", "key_id": "test-key-main", "block_height": r1.BlockHeight + 1, "epoch": ep, "commitment": enc(idx(a)), "tx_id": "devnet-synthetic-1"},
		{"type": "reshare_accuse", "key_id": "test-key-main", "block_height": r1.BlockHeight + 2, "epoch": ep, "commitment": enc(idx(a)), "tx_id": "devnet-synthetic-2"},
		{"type": "reshare_accuse", "key_id": "test-key-main", "block_height": r1.BlockHeight + 3, "epoch": ep, "commitment": enc(idx(b)), "tx_id": "devnet-synthetic-3"},
	}
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	for n := 1; n <= cfg.Nodes; n++ {
		for _, r := range rows {
			if _, err := client.Database(d.nodeDbName(n)).Collection("tss_commitments").InsertOne(ctx, r); err != nil {
				t.Fatalf("insert on magi-%d: %v", n, err)
			}
		}
	}
	client.Disconnect(ctx)
	t.Logf("counts written on all nodes (epoch %d): %s x2 (old member), %s x1 (new-only)", ep, a, b)

	r2, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": r1.BlockHeight}}, 15*time.Minute)
	if err != nil {
		// With magi-4 cut off, a session that also leaves out magi-5 has only 3
		// parties, and 3 of 5 cannot reach the 2/3 signing quorum, so rotation
		// stalls: the fall-through exclusion's effect in this setup.
		t.Fatalf("no reshare after R1 (%v): the session left out %s in place of %s, so too few parties could sign its result and rotation stalled", err, b, a)
	}
	named := namedIn(t, d, ctx, *r2)
	t.Logf("R2 at %d epoch %d named %v", r2.BlockHeight, r2.Epoch, named)
	if sliceHas(named, d.witnessAccount(4)) {
		t.Fatalf("INCONCLUSIVE: magi-4 took part in R2 (it was meant to be not ready)")
	}
	if !sliceHas(named, b) {
		t.Errorf("%s (less named, new-only) was left out in place of the most-named old member %s that could not be", b, a)
	}
}

func sliceHas(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// TestParseAccused: a one-name list is logged without quotes. Before, only the
// quoted form was read, so a leader that named one party looked like a leader
// that logged nothing and the session was dropped from the count.
func TestParseAccused(t *testing.T) {
	cases := []struct {
		line   string
		want   []string
		logged bool
	}{
		{`sessionId=reshare-380-0-test-key-main-266f keyId=test-key-main accused=[magi.test7]`, []string{"magi.test7"}, true},
		{`sessionId=reshare-340-0-test-key-main-266f keyId=test-key-main accused="[magi.test1 magi.test7]`, []string{"magi.test1", "magi.test7"}, true},
		{`sessionId=reshare-340-0-test-key-main-266f keyId=test-key-main accused=[]`, nil, true},
		{"", nil, false},
	}
	for _, c := range cases {
		got, logged := parseAccused(c.line)
		if logged != c.logged || strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("parseAccused(%q) = %v, %v; want %v, %v", c.line, got, logged, c.want, c.logged)
		}
	}
}
