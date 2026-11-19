package main

import (
	"strconv"
	"testing"
)

// B-7: the head withdrawal that has not settled withdrawalRollWindow L1 blocks
// after it was queued is an alarm (the bot neither rolls nor re-prices it);
// one inside the window, or no withdrawal in flight, is not.
func TestB7_UnsettledHeadWithdrawalIsAnAlarm(t *testing.T) {
	head := `{"nonce":3,"amount":500000000000000000,"to":"0x00000000000000000000000000000000000000e3","asset":"eth","unsigned_tx_hex":"02","block_height":100}`
	for _, c := range []struct {
		name  string
		np    string
		tip   uint64
		alarm bool
	}{
		{"inside the roll window", "4", 100 + withdrawalRollWindow - 1, false},
		{"at the roll window", "4", 100 + withdrawalRollWindow, true},
		{"well past it", "4", 100 + 5*withdrawalRollWindow, true},
		{"nothing in flight", "3", 100 + 5*withdrawalRollWindow, false},
	} {
		_, l2, gql, rpc := healthWorld(t, 10)
		l2.set("vsc1bridge", "n", "3")
		l2.set("vsc1bridge", "np", c.np)
		l2.set("vsc1bridge", "d-3", head)
		l2.set("vsc1bridge", "h", strconv.FormatUint(c.tip, 10))
		r := checked(t, gql, rpc, emptyCP())
		if got := hasIssue(r, "B-7: withdrawal nonce 3"); got != c.alarm {
			t.Fatalf("%s: B-7 alarm %v, want %v (%+v)", c.name, got, c.alarm, r)
		}
		if c.alarm && !hasIssue(r, "rollWithdrawal") {
			t.Fatalf("%s: the alarm does not say what can move it: %+v", c.name, r)
		}
	}
}

// A head withdrawal whose record cannot be read is an alarm too (it used to
// read as "nothing to report").
func TestB7_UnreadableHeadRecordIsAnAlarm(t *testing.T) {
	for _, rec := range []string{"", "{not json", `{"nonce":3,"amount":0,"unsigned_tx_hex":"02"}`} {
		_, l2, gql, rpc := healthWorld(t, 10)
		l2.set("vsc1bridge", "n", "3")
		l2.set("vsc1bridge", "np", "4")
		l2.set("vsc1bridge", "d-3", rec)
		l2.set("vsc1bridge", "h", "100")
		r := checked(t, gql, rpc, emptyCP())
		if !hasIssue(r, "B-7: the head withdrawal nonce 3 has no readable record") {
			t.Fatalf("head record %q: %+v; want the unreadable-record alarm", rec, r)
		}
	}
}
