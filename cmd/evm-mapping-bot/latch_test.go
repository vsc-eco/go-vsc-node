package main

import (
	"errors"
	"testing"
	"time"
)

// P8 (BTC bot BOT-LATCH-1): a "not the vault operator" refusal stops the
// driver's operator ops, and the latch expires after notOperatorRecheck, so a
// bot appointed operator after it started resumes without a restart.
func TestP8_NotOperatorLatchExpires(t *testing.T) {
	cur := time.Unix(1_000_000, 0)
	oldNow := vaultOpNow
	vaultOpNow = func() time.Time { return cur }
	t.Cleanup(func() {
		vaultOpNow = oldNow
		vaultOpMu.Lock()
		notOperatorAt, lastVaultOpAt = time.Time{}, map[string]time.Time{}
		vaultOpMu.Unlock()
	})
	vaultOpMu.Lock()
	notOperatorAt, lastVaultOpAt = time.Time{}, map[string]time.Time{}
	vaultOpMu.Unlock()

	noteRefusal(errors.New("some other contract error"))
	if !vaultOpDue("migrate") {
		t.Fatal("an unrelated error latched the driver")
	}
	noteRefusal(errors.New("contract error: action must be performed by the owner or the vault operator"))
	if vaultOpDue("migrate") {
		t.Fatal("a not-operator refusal did not latch the driver")
	}
	cur = cur.Add(notOperatorRecheck - time.Second)
	if vaultOpDue("retire") {
		t.Fatal("the latch ended before notOperatorRecheck")
	}
	cur = cur.Add(2 * time.Second)
	if !vaultOpDue("retire") {
		t.Fatal("the latch still holds after notOperatorRecheck: an appointed bot would stay idle until a restart")
	}
}
