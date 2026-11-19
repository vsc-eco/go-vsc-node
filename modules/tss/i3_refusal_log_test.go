package tss

import (
	"strconv"
	"testing"
	"time"
)

func resetPolicyRefusalLog() {
	policyRefusalLog.Lock()
	policyRefusalLog.at = map[string]time.Time{}
	policyRefusalLog.Unlock()
}

// I-3: the same refused request (key and message) is logged once per
// policyRefusalLogEvery, not every sign interval; another key, the same key
// after the interval, and a NEW message on the same key (an output-scoping
// refusal is a security signal) are logged.
func TestI3_PolicyRefusalLogIsRateLimitedPerRequest(t *testing.T) {
	resetPolicyRefusalLog()
	if !policyRefusalLogDue("request:k1:aa") {
		t.Fatal("first refusal of k1/aa not logged")
	}
	if policyRefusalLogDue("request:k1:aa") {
		t.Fatal("the same request refused again inside the interval logged again")
	}
	if !policyRefusalLogDue("request:k1:bb") {
		t.Fatal("a new message refused on the same key was not logged")
	}
	if !policyRefusalLogDue("request:k2:aa") {
		t.Fatal("another key's refusal not logged")
	}
	policyRefusalLog.Lock()
	policyRefusalLog.at["request:k1:aa"] = time.Now().Add(-policyRefusalLogEvery - time.Second)
	policyRefusalLog.Unlock()
	if !policyRefusalLogDue("request:k1:aa") {
		t.Fatal("k1/aa not logged again after the interval")
	}
}

// Both call sites key a refusal by key and message (policyRefusalKey): two
// messages on one key are two refusals, each logged.
func TestI3_ANewMessageOnAKeyIsANewRefusal(t *testing.T) {
	resetPolicyRefusalLog()
	a, b := policyRefusalKey("scope", "k1", []byte{0xaa}), policyRefusalKey("scope", "k1", []byte{0xbb})
	if a == b {
		t.Fatalf("two messages on one key share a refusal key %q", a)
	}
	if !policyRefusalLogDue(a) || !policyRefusalLogDue(b) {
		t.Fatal("a new refused message on a logged key was muted")
	}
	if policyRefusalLogDue(a) {
		t.Fatal("the same refused message logged twice inside the interval")
	}
}

// The remembered refusals stay bounded: past policyRefusalLogPruneAt the ones
// older than the interval are dropped; recent ones are kept (still muted).
func TestI3_PolicyRefusalLogIsBounded(t *testing.T) {
	resetPolicyRefusalLog()
	old := time.Now().Add(-policyRefusalLogEvery - time.Minute)
	policyRefusalLog.Lock()
	for i := 0; i < policyRefusalLogPruneAt; i++ {
		policyRefusalLog.at["request:old:"+strconv.Itoa(i)] = old
	}
	policyRefusalLog.at["request:recent"] = time.Now()
	policyRefusalLog.Unlock()
	if !policyRefusalLogDue("request:new") {
		t.Fatal("new refusal not logged")
	}
	policyRefusalLog.Lock()
	n := len(policyRefusalLog.at)
	policyRefusalLog.Unlock()
	if n != 2 { // "request:recent" + "request:new"
		t.Fatalf("%d remembered after the prune, want 2", n)
	}
	if policyRefusalLogDue("request:recent") {
		t.Fatal("a recent refusal was forgotten by the prune")
	}
}
