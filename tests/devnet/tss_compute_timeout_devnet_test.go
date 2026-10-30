package devnet

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"
)

// TestTSSSlowNodeDoesNotTimeItselfOut: a node still computing a reshare round
// must not time itself out of the session. tss-lib runs a round's proofs inside
// the update that completes the previous round, holding the party lock, so while
// a node computes it processes no message and its session timer sees no
// activity. Before the fix such a node gave up mid-round, dropped out of a
// reshare its peers would have finished, and the peers then timed out waiting
// for it: the rotation was lost.
//
// magi-3 runs with the devnet's 20s reshare timeout, its peers with the suite's
// 2m, and once the key exists magi-3 is held to one CPU, so its own rounds
// (24-33s already on an unthrottled 5-node devnet) outlast its timeout while the
// peers' stay inside theirs. The run reports how often magi-3 held its timeout
// off; when it never had to, the run proves nothing about the fix.
func TestTSSSlowNodeDoesNotTimeItselfOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	const slow = 3
	cfg := tssTestConfig()
	own := *cfg.SysConfigOverrides.TssParams
	own.ReshareTimeout = 20 * time.Second
	cfg.NodeSysConfigOverrides = map[int]*systemconfig.SysConfigOverrides{slow: {TssParams: &own}}

	d, ctx := startDevnet(t, cfg, 30*time.Minute)

	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	t.Logf("keygen: epoch=%d block=%d", keygen.Epoch, keygen.BlockHeight)
	if out, err := exec.CommandContext(ctx, "docker", "update", "--cpus", "1", d.containerName(slow)).CombinedOutput(); err != nil {
		t.Fatalf("throttling magi-%d: %v: %s", slow, err, out)
	}
	t.Logf("magi-%d held to 1 CPU", slow)

	reshare := waitForCommitmentAfter(t, d.MongoURI(), "reshare", keygen.BlockHeight, 8*time.Minute)
	t.Logf("first reshare after keygen: epoch=%d block=%d", reshare.Epoch, reshare.BlockHeight)

	var holds int
	for n := 1; n <= cfg.Nodes; n++ {
		logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", n))
		if err != nil {
			t.Fatalf("magi-%d logs: %v", n, err)
		}
		var timeouts, completed int
		for _, line := range strings.Split(logs, "\n") {
			if !strings.Contains(line, "sessionId=reshare-") {
				continue
			}
			switch {
			case strings.Contains(line, "] session timeout "): // the monitor's own line, once per timeout
				timeouts++
			case strings.Contains(line, "reshare completed successfully"):
				completed++
			case strings.Contains(line, "still processing a message; not timing out yet"):
				if n == slow {
					holds++
				}
			}
		}
		t.Logf("magi-%d: reshare session timeouts=%d completed=%d", n, timeouts, completed)
		if timeouts > 0 {
			t.Errorf("magi-%d timed out of %d reshare session(s): a rotation was lost", n, timeouts)
		}
		if n == slow && completed == 0 {
			t.Errorf("magi-%d completed no reshare", n)
		}
	}
	t.Logf("magi-%d held its timeout off %d time(s) while still processing", slow, holds)
	if holds == 0 {
		t.Logf("NOTE: magi-%d never had to hold its timeout off: its rounds finished inside %s, so this run did not exercise the slow path", slow, own.ReshareTimeout)
	}
}
