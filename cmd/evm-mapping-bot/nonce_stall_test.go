package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// stallWorld is poolWorld with a settable clock.
func stallWorld(t *testing.T) (*fakeL2, *l2Submitter, *time.Time) {
	t.Helper()
	l2, sub := poolWorld(t)
	cur := time.Unix(1_000_000, 0)
	sub.now = func() time.Time { return cur }
	return l2, sub, &cur
}

func mapCall(t *testing.T, sub *l2Submitter, d int) (string, error) {
	t.Helper()
	return sub.callWithRetry(context.Background(), "vsc1bridge", "map", json.RawMessage(`{"d":`+string(rune('0'+d))+`}`), 1)
}

// B-R1b, devnet FS2: the bot's node holds its nonce-0 call, but the nodes a
// block needs never received it, so the account nonce never moves. Past
// nonceStallGrace the bot goes out again at the account nonce (the BTC bot's
// rule), that call lands, and the calls stacked behind it follow.
func TestBR1b_StuckNonceIsResentAtTheAccountNonce(t *testing.T) {
	l2, sub, cur := stallWorld(t)
	a, err := mapCall(t, sub, 1)
	if err != nil {
		t.Fatal(err)
	}
	l2.pool.stuck = map[string]bool{a: true}
	l2.pool.produce()
	if _, err := mapCall(t, sub, 2); err != nil { // pipelined at 1
		t.Fatal(err)
	}
	l2.pool.produce()
	if l2.pool.confirmed != 0 {
		t.Fatalf("account nonce %d moved past a stuck call", l2.pool.confirmed)
	}
	*cur = cur.Add(nonceStallGrace + time.Second)
	if _, err := mapCall(t, sub, 3); err != nil {
		t.Fatal(err)
	}
	if got := submittedNonces(t, l2); !reflect.DeepEqual(got, []uint64{0, 1, 0}) {
		t.Fatalf("nonces %v, want [0 1 0]: the third call must go out again at the account nonce", got)
	}
	l2.pool.produce()
	if l2.pool.confirmed != 2 {
		t.Fatalf("account nonce %d after the resend, want 2 (resend at 0, then the stacked 1)", l2.pool.confirmed)
	}
	if s := status(l2, a); s != "gone" {
		t.Fatalf("the lost call is %s, want deleted once its nonce was used", s)
	}
}

func TestBR1b_InsideTheGraceThePipelineContinues(t *testing.T) {
	l2, sub, cur := stallWorld(t)
	a, err := mapCall(t, sub, 1)
	if err != nil {
		t.Fatal(err)
	}
	l2.pool.stuck = map[string]bool{a: true}
	*cur = cur.Add(nonceStallGrace - time.Second)
	if _, err := mapCall(t, sub, 2); err != nil {
		t.Fatal(err)
	}
	if got := submittedNonces(t, l2); !reflect.DeepEqual(got, []uint64{0, 1}) {
		t.Fatalf("nonces %v, want [0 1]: inside the grace the bot keeps its pipeline", got)
	}
}

// The node caps an account at 100 pending nonces; a refusal for it resets the
// bot to the account nonce at once.
func TestBR1b_TooFastResetsAtOnce(t *testing.T) {
	old := maxPendingL2
	maxPendingL2 = 1000 // let the bot reach the node's own cap
	t.Cleanup(func() { maxPendingL2 = old })
	l2, sub, _ := stallWorld(t)
	sub.seeded, sub.haveNext, sub.next = true, true, 150
	if _, err := mapCall(t, sub, 1); err == nil || !isNonceTooFastErr(err) {
		t.Fatalf("err %v, want the node's too-fast refusal", err)
	}
	if _, err := mapCall(t, sub, 2); err != nil {
		t.Fatal(err)
	}
	// the fake records every attempt: 150 (refused), then 0
	if got := submittedNonces(t, l2); !reflect.DeepEqual(got, []uint64{150, 0}) {
		t.Fatalf("nonces %v, want [150 0]: refused at 150, then the account nonce", got)
	}
	if l2.pool.pending(0) == nil {
		t.Fatal("the call at the account nonce is not pending")
	}
}

// The bot keeps at most maxPendingL2 of its calls above the account nonce and
// leaves the rest for a later tick, unsubmitted.
func TestBR1b_PipelineCap(t *testing.T) {
	old := maxPendingL2
	maxPendingL2 = 2
	t.Cleanup(func() { maxPendingL2 = old })
	l2, sub, _ := stallWorld(t)
	for d := 1; d <= 2; d++ {
		if _, err := mapCall(t, sub, d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mapCall(t, sub, 3); !errors.Is(err, errL2PipelineFull) {
		t.Fatalf("err %v, want errL2PipelineFull", err)
	}
	if n := len(submittedNonces(t, l2)); n != 2 {
		t.Fatalf("%d submitted, want 2 (the third waits)", n)
	}
	l2.pool.produce()
	if _, err := mapCall(t, sub, 4); err != nil {
		t.Fatal(err)
	}
	if got := submittedNonces(t, l2); got[len(got)-1] != 2 {
		t.Fatalf("nonces %v, want the next call at 2 after the block", got)
	}
}

// A chain that keeps taking the bot's calls is never a stall, however long the
// bot has been running: the grace counts from the last time the account nonce
// moved.
func TestBR1b_AMovingAccountNonceKeepsThePipeline(t *testing.T) {
	l2, sub, cur := stallWorld(t)
	if _, err := mapCall(t, sub, 1); err != nil { // nonce 0
		t.Fatal(err)
	}
	l2.pool.produce() // account nonce 1
	*cur = cur.Add(nonceStallGrace - time.Second)
	if _, err := mapCall(t, sub, 2); err != nil { // nonce 1, sees the move
		t.Fatal(err)
	}
	*cur = cur.Add(2 * time.Second) // past the grace since the start, not since the move
	if _, err := mapCall(t, sub, 3); err != nil {
		t.Fatal(err)
	}
	if got := submittedNonces(t, l2); !reflect.DeepEqual(got, []uint64{0, 1, 2}) {
		t.Fatalf("nonces %v, want [0 1 2]: the account nonce moved, so no reset", got)
	}
}

// I-19, testnet 11-04 (07:18 attestHeaders@76, 12:10 attestHeaders@192): the
// account nonce moved, the bot's next call was refused (RC), and nothing went
// out for longer than the grace. The next two calls are one tick: the first
// takes the account nonce, and the second must take the one after it, not see
// "no move for nonceStallGrace" and go out again at the first one's nonce.
func TestI19_AQuietPeriodIsNotAStall(t *testing.T) {
	l2, sub, cur := stallWorld(t)
	if _, err := mapCall(t, sub, 1); err != nil { // nonce 0
		t.Fatal(err)
	}
	l2.pool.produce() // account nonce 1
	l2.pool.refuse = 1
	if _, err := mapCall(t, sub, 2); err == nil { // sees account nonce 1, refused
		t.Fatal("the node's RC refusal did not reach the caller")
	}
	*cur = cur.Add(nonceStallGrace + time.Minute) // quiet: nothing of the bot's pending
	for d := 3; d <= 4; d++ {
		if _, err := mapCall(t, sub, d); err != nil {
			t.Fatal(err)
		}
	}
	// the fake records every attempt: 0, 1 (refused), then the two calls
	if got := submittedNonces(t, l2); !reflect.DeepEqual(got, []uint64{0, 1, 1, 2}) {
		t.Fatalf("nonces %v, want [0 1 1 2]: the two calls after the quiet period need two nonces", got)
	}
	l2.pool.produce()
	if l2.pool.confirmed != 3 {
		t.Fatalf("account nonce %d, want 3: both calls after the quiet period land", l2.pool.confirmed)
	}
}

// The clock the I-19 fix restarts still catches a stuck call: it counts from
// the call that went out with nothing pending, so a call stuck after a quiet
// period is resent nonceStallGrace after it was sent.
func TestI19_AStuckCallAfterAQuietPeriodIsStillResent(t *testing.T) {
	l2, sub, cur := stallWorld(t)
	if _, err := mapCall(t, sub, 1); err != nil { // nonce 0
		t.Fatal(err)
	}
	l2.pool.produce()
	l2.pool.refuse = 1
	if _, err := mapCall(t, sub, 2); err == nil {
		t.Fatal("the node's RC refusal did not reach the caller")
	}
	*cur = cur.Add(nonceStallGrace + time.Minute)
	b, err := mapCall(t, sub, 3) // nonce 1, the nodes a block needs never get it
	if err != nil {
		t.Fatal(err)
	}
	l2.pool.stuck = map[string]bool{b: true}
	if _, err := mapCall(t, sub, 4); err != nil { // nonce 2, inside the grace
		t.Fatal(err)
	}
	l2.pool.produce()
	*cur = cur.Add(nonceStallGrace + time.Second)
	if _, err := mapCall(t, sub, 5); err != nil {
		t.Fatal(err)
	}
	if got := submittedNonces(t, l2); !reflect.DeepEqual(got, []uint64{0, 1, 1, 2, 1}) {
		t.Fatalf("nonces %v, want [0 1 1 2 1]: past the grace from the stuck call, the bot resends at the account nonce", got)
	}
	l2.pool.produce()
	if l2.pool.confirmed != 3 {
		t.Fatalf("account nonce %d after the resend, want 3", l2.pool.confirmed)
	}
}
