package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// I-20, testnet 11-04 10:12-11:21: the bot had 13,659-20,000 RC and every
// attestation declared a flat 20,000, so the node refused them all ("not enough
// RCS available") and the USDC deposit whose header they would store waited
// ~1 h, for an attestHeaders that uses ~3,700 RC a header (real wasm 3,717).
// Attestations now declare twice
// their dry run's rc_used, between 1,000 and 20,000.
func TestI20_AttestationsAreSizedByTheirDryRun(t *testing.T) {
	for _, c := range []struct {
		rcUsed int64 // 0: dry run unavailable
		want   uint64
	}{{551, 1_102}, {100, 1_000}, {15_000, 20_000}, {0, 20_000}} {
		l2, sub := poolWorld(t)
		if c.rcUsed > 0 {
			rc := c.rcUsed
			l2.sim = func(string) map[string]any { return map[string]any{"success": true, "ret": "", "rc_used": rc} }
		}
		if got := attestRC(context.Background(), sub, "vsc1verifier", "attestHeaders", []byte(`{}`)); got != c.want {
			t.Fatalf("rc_used %d: rc_limit %d, want %d", c.rcUsed, got, c.want)
		}
	}
	// a refused dry run declares the ceiling (the node is the judge)
	l2, sub := poolWorld(t)
	l2.sim = func(string) map[string]any {
		return map[string]any{"success": false, "err": "x", "rc_used": int64(500)}
	}
	if got := attestRC(context.Background(), sub, "vsc1verifier", "attestHeaders", []byte(`{}`)); got != attestRcCeiling {
		t.Fatalf("refused dry run: %d, want %d", got, attestRcCeiling)
	}
}

// End to end: an attestation run from a stored root declares the sized limit.
func TestI20_AttestFromRootDeclaresTheSizedLimit(t *testing.T) {
	l2, sub := poolWorld(t)
	l2.sim = func(string) map[string]any { return map[string]any{"success": true, "ret": "", "rc_used": int64(551)} }
	l2.set("vsc1verifier", "hr-1000", strings.Repeat("r", 32)) // the root is attested: only attestHeaders
	cp := &Checkpoint{AttestCache: map[uint64]*attestEntry{1000: {Headers: map[uint64]attestedBlock{990: {RLP: "c0", StorageProof: []string{"0x01"}}}}}}
	attestFromRoot(context.Background(), l2.gql(t), sub, cp, sub.cfg, "vsc1verifier", 1000, []uint64{990}, nil)
	calls := submittedCalls(t, l2)
	if len(calls) != 1 || calls[0].Action != "attestHeaders" || calls[0].RcLimit != 1_102 {
		t.Fatalf("calls %+v, want one attestHeaders declaring 1,102", calls)
	}
}

// I-20, testnet 11-04 07:19-07:27: migrateVault was refused "not enough RCS
// available: 13059 < 41914" for as long as the bot's RC stayed under what it
// needs (~12 h projected), and the only alarm said RC was below 20,000. The
// health check now names the refused op and what it needs, until a vault op
// goes through.
func TestI20_RotationOpRefusedForRCIsAnAlarm(t *testing.T) {
	t.Cleanup(func() { noteRotationRC("", nil) })
	l2, sub := poolWorld(t)
	l2.sim = func(string) map[string]any {
		return map[string]any{"success": true, "ret": "sweep 1-0 eth 5", "rc_used": int64(20_957)}
	}
	l2.pool.refuse = 1
	if _, err := vaultOp(context.Background(), sub.gql, sub, "vsc1bridge", "migrateVault", json.RawMessage(`{}`)); err == nil {
		t.Fatal("the node's RC refusal did not reach the driver")
	}
	alarm := rotationRCAlarm(vaultOpNow())
	if !strings.Contains(alarm, "migrateVault") || !strings.Contains(alarm, "41914 needed") {
		t.Fatalf("alarm %q, want migrateVault and the 41,914 it needs", alarm)
	}
	_, _, gql, rpc := healthWorld(t, 10)
	h := newBotHealth(12 * time.Second)
	now := time.Now()
	h.recordTick(now, nil, emptyCP())
	h.check(context.Background(), now, gql, rpc, "vsc1bridge", []string{"vsc1bridge-primary"}, "did:bot", []string{healthVault}, nil, 0, nil)
	if r := h.report(now); r.Status == "ok" || !hasIssue(r, "I-20: migrateVault is refused for RC") {
		t.Fatalf("health %s %v, want the I-20 alarm", r.Status, r.Issues)
	}
	if a := rotationRCAlarm(vaultOpNow().Add(rotationRCAlarmFor + time.Minute)); a != "" {
		t.Fatalf("a refusal %s old still alarms: %q", rotationRCAlarmFor+time.Minute, a)
	}
	// funded: the next vault op goes through and the alarm clears
	if _, err := vaultOp(context.Background(), sub.gql, sub, "vsc1bridge", "migrateVault", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if a := rotationRCAlarm(vaultOpNow()); a != "" {
		t.Fatalf("alarm %q after the op went through", a)
	}
	// another error is not an RC refusal
	noteRotationRC("migrateVault", errNonceConsumed)
	if a := rotationRCAlarm(vaultOpNow()); a != "" {
		t.Fatalf("alarm %q for a non-RC error", a)
	}
}

// The root not yet attested: attestHistoryRoot declares its sized limit too.
func TestI20_AttestHistoryRootDeclaresTheSizedLimit(t *testing.T) {
	l2, sub := poolWorld(t)
	l2.sim = func(string) map[string]any {
		return map[string]any{"success": true, "ret": "", "rc_used": int64(3_000)}
	}
	cp := &Checkpoint{AttestCache: map[uint64]*attestEntry{1000: {AccountProof: []string{"0x01"}, Headers: map[uint64]attestedBlock{990: {RLP: "c0", StorageProof: []string{"0x01"}}}}}}
	attestFromRoot(context.Background(), l2.gql(t), sub, cp, sub.cfg, "vsc1verifier", 1000, []uint64{990}, nil)
	calls := submittedCalls(t, l2)
	if len(calls) != 2 || calls[0].Action != "attestHistoryRoot" || calls[0].RcLimit != 6_000 || calls[1].RcLimit != 6_000 {
		t.Fatalf("calls %+v, want attestHistoryRoot then attestHeaders, each declaring 6,000", calls)
	}
}

// I-17, testnet 11-04 06:57-08:17: through a rotation the bot sent a
// drainWithdrawalQueue a minute that issued nothing (ret "0"): the superseded
// generation's in-flight withdrawals hold the nonces (wnh) and the bridge
// issues no queued withdrawal until they confirm. No drain then; once they
// have confirmed the drain goes, as it is the call that hands the nonces over.
func TestI17_NoDrainWhileASupersededGenerationHoldsTheNonces(t *testing.T) {
	l2, sub := poolWorld(t)
	for k, v := range map[string]string{"wqh": "3", "wqt": "5", "n": "10", "np": "12", "wnh": "0", "zkverifier": "vsc1verifier"} {
		l2.set("vsc1bridge", k, v)
	}
	l2.set("vsc1verifier", "h", "500")
	l2.set("vsc1verifier", "b-500", tipHeader(time.Now().Add(-5*time.Minute)))
	lastDrainAt = time.Time{}
	drainIfQueued(context.Background(), l2.gql(t), sub, sub.cfg)
	if calls := submittedCalls(t, l2); len(calls) != 0 {
		t.Fatalf("drained while gen 0 holds nonces 10-11: %+v", calls)
	}
	l2.set("vsc1bridge", "n", "12") // gen 0's withdrawals confirmed: the drain hands the nonces over
	drainIfQueued(context.Background(), l2.gql(t), sub, sub.cfg)
	if calls := submittedCalls(t, l2); len(calls) != 1 || calls[0].Action != "drainWithdrawalQueue" {
		t.Fatalf("calls %+v, want the drain that hands the nonces to the active generation", calls)
	}
}

// I-20 follow-up, testnet 11-05: when the root is not stored yet,
// attestHistoryRoot goes out in the same tick, and attestHeaders' dry run alone
// ran before it, failed on the missing root and declared the 20,000 ceiling
// (14 of 14 live attestHeaders). It is now dry-run behind attestHistoryRoot.
func TestI20_AttestHeadersSizedWhenTheRootGoesInTheSameTick(t *testing.T) {
	l2, sub := poolWorld(t)
	var asked [][]string
	l2.simCalls = func(actions []string) []map[string]any {
		asked = append(asked, actions)
		var out []map[string]any
		rooted := false // the state as the contract sees it: hr-1000 is not stored
		for _, a := range actions {
			switch {
			case a == "attestHistoryRoot":
				rooted = true
				out = append(out, map[string]any{"success": true, "ret": "", "rc_used": int64(810)})
			case a == "attestHeaders" && rooted:
				out = append(out, map[string]any{"success": true, "ret": "", "rc_used": int64(551)})
			default:
				out = append(out, map[string]any{"success": false, "err": "ErrTransaction", "err_msg": "history root 1000 is not attested", "rc_used": int64(200)})
			}
		}
		return out
	}
	cp := &Checkpoint{AttestCache: map[uint64]*attestEntry{1000: {AccountProof: []string{"0x02"}, Headers: map[uint64]attestedBlock{990: {RLP: "c0", StorageProof: []string{"0x01"}}}}}}
	attestFromRoot(context.Background(), l2.gql(t), sub, cp, sub.cfg, "vsc1verifier", 1000, []uint64{990}, nil)
	calls := submittedCalls(t, l2)
	if len(calls) != 2 || calls[0].Action != "attestHistoryRoot" || calls[1].Action != "attestHeaders" {
		t.Fatalf("calls %+v, want attestHistoryRoot then attestHeaders", calls)
	}
	if calls[0].RcLimit != 1_620 || calls[1].RcLimit != 1_102 {
		t.Fatalf("rc_limits %d / %d, want 1,620 / 1,102 (attestHeaders' dry run behind the root)", calls[0].RcLimit, calls[1].RcLimit)
	}
	if len(asked) != 2 || strings.Join(asked[1], ",") != "attestHistoryRoot,attestHeaders" {
		t.Fatalf("dry runs %v, want the headers' run behind attestHistoryRoot", asked)
	}
}
