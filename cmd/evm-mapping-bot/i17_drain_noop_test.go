package main

import (
	"context"
	"encoding/base64"
	"testing"
	"time"
)

// I-17, devnet E1 arm B 11-05: from the cut-over until confirmSweep moved the
// books, generation 1 covered no queued withdrawal and every drain issued
// nothing (17 in 18 min, the fixed bot; the wnh check does not see this hold).
// A drain whose dry run would do nothing at all is not sent; anything the dry
// run shows it would do, or no usable dry run, sends it as before.
func TestI17_DrainNotSentWhileItWouldDoNothing(t *testing.T) {
	bridge := func(diff map[string]any) map[string]any { return map[string]any{"vsc1bridge": diff} }
	empty := map[string]any{"Deletions": map[string]bool{}, "KeyDiff": map[string]any{}}
	// kd is a state_diff KeyDiff as the node answers it: Current in base64,
	// Previous null for a key the call only read.
	kd := func(kv ...string) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = map[string]any{"Previous": nil, "Current": base64.StdEncoding.EncodeToString([]byte(kv[i+1]))}
		}
		return map[string]any{"Deletions": map[string]bool{}, "KeyDiff": m}
	}
	readsOnly := map[string]any{"vsc1bridge": kd("wqh", "3", "wqt", "4", "n", "10", "np", "10"), "vsc1verifier": kd("h", "500")}
	for _, c := range []struct {
		name string
		sim  map[string]any // nil: dry run unavailable
		sent bool
	}{
		{"would issue nothing (the keys it read, as the node lists them)", map[string]any{"success": true, "ret": "0", "logs": map[string]any{"vsc1bridge": map[string]any{"Logs": []string{}, "TssOps": nil}}, "state_diff": readsOnly}, false},
		{"would issue nothing (empty diff)", map[string]any{"success": true, "ret": "0", "logs": map[string]any{"vsc1bridge": map[string]any{"Logs": []string{}, "TssOps": []any{}}}, "state_diff": bridge(empty)}, false},
		{"a key it read would change (a write)", map[string]any{"success": true, "ret": "0", "state_diff": map[string]any{"vsc1bridge": kd("wqh", "4", "wqt", "4"), "vsc1verifier": kd("h", "500")}}, true},
		{"a verifier key would change", map[string]any{"success": true, "ret": "0", "state_diff": map[string]any{"vsc1bridge": kd("wqh", "3"), "vsc1verifier": kd("h", "501")}}, true},
		{"issues one", map[string]any{"success": true, "ret": "1", "logs": map[string]any{"vsc1bridge": map[string]any{"Logs": []string{}, "TssOps": []any{map[string]any{"type": "sign"}}}}, "state_diff": bridge(kd("np", "11"))}, true},
		{"refunds one (QFREEZE-1)", map[string]any{"success": true, "ret": "0", "logs": map[string]any{"vsc1bridge": map[string]any{"Logs": []string{"unmapq|refund|q=3"}}}, "state_diff": bridge(kd("wqh", "4"))}, true},
		{"issued count alone", map[string]any{"success": true, "ret": "1", "state_diff": bridge(empty)}, true},
		{"logs only", map[string]any{"success": true, "ret": "0", "logs": map[string]any{"vsc1bridge": map[string]any{"Logs": []string{"unmapq|wait"}}}, "state_diff": bridge(empty)}, true},
		{"steps the debit recount (a new key)", map[string]any{"success": true, "ret": "0", "state_diff": bridge(kd("qdr", "3|4"))}, true},
		{"deletes a key", map[string]any{"success": true, "ret": "0", "state_diff": bridge(map[string]any{"Deletions": map[string]bool{"wnh": true}})}, true},
		{"no state_diff for the bridge", map[string]any{"success": true, "ret": "0"}, true},
		{"refused dry run", map[string]any{"success": false, "err": "x", "ret": "0"}, true},
		{"dry run unavailable", nil, true},
	} {
		l2, sub := poolWorld(t)
		for k, v := range map[string]string{"wqh": "3", "wqt": "4", "n": "10", "np": "10", "zkverifier": "vsc1verifier"} {
			l2.set("vsc1bridge", k, v)
		}
		l2.set("vsc1verifier", "h", "500")
		l2.set("vsc1verifier", "b-500", tipHeader(time.Now().Add(-5*time.Minute)))
		if c.sim != nil {
			sim := c.sim
			l2.sim = func(string) map[string]any { return sim }
		}
		lastDrainAt, drainSkipping = time.Time{}, false
		drainIfQueued(context.Background(), l2.gql(t), sub, sub.cfg)
		sent := false
		for _, call := range submittedCalls(t, l2) {
			sent = sent || call.Action == "drainWithdrawalQueue"
		}
		if sent != c.sent {
			t.Fatalf("%s: drain sent %v, want %v", c.name, sent, c.sent)
		}
	}
	lastDrainAt, drainSkipping = time.Time{}, false
}
