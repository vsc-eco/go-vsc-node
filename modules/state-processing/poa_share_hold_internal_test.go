package state_engine

import (
	"encoding/base64"
	"errors"
	"math/big"
	"strings"
	"testing"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/poaseats"
	tss_db "vsc-node/modules/db/vsc/tss"
)

// POA-1: a reshare keeps the key, so every past committee's share of a funded
// generation still signs. The bond behind such a share stays locked until the
// generation is rotated out and drained (THORChain's retiring-vault rule).

func bits(idx ...int) string {
	b := new(big.Int)
	for _, i := range idx {
		b.SetBit(b, i, 1)
	}
	return base64.RawURLEncoding.EncodeToString(b.Bytes())
}

func vaultKey(gen uint32) string { return "vsc1BTC-" + btcvault.VaultKeyName(gen) }

func holdCheck(t *testing.T, vaults []btcvault.Vault, rows map[string][]tss_db.TssCommitment, elecs map[uint64][]string, account string) (bool, error) {
	t.Helper()
	return holdCheckWith(t, vaults, rows, elecs, nil, account)
}

func holdCheckWith(t *testing.T, vaults []btcvault.Vault, rows map[string][]tss_db.TssCommitment, elecs map[uint64][]string, current []string, account string) (bool, error) {
	t.Helper()
	return accountInFundedVaultCommittees(vaults, "vsc1BTC", account, current,
		func(keyId string) ([]tss_db.TssCommitment, error) {
			if keyId == "boom" {
				return nil, errors.New("transient")
			}
			return rows[keyId], nil
		},
		func(epoch uint64) (*elections.ElectionResult, error) {
			accts, ok := elecs[epoch]
			if !ok {
				return nil, nil
			}
			e := &elections.ElectionResult{}
			for _, a := range accts {
				e.Members = append(e.Members, elections.ElectionMember{Account: a})
			}
			return e, nil
		})
}

func TestPoa1_ShareOfAnyEpochOfAFundedGenerationLocksTheBond(t *testing.T) {
	vaults := []btcvault.Vault{
		{Generation: 0, Status: btcvault.VaultStatusRetiring},
		{Generation: 1, Status: btcvault.VaultStatusActive},
		{Generation: 2, Status: btcvault.VaultStatusPending},
	}
	rows := map[string][]tss_db.TssCommitment{
		vaultKey(0): {{Epoch: 3, Commitment: bits(0, 1, 2)}, {Epoch: 5, Commitment: bits(0, 1)}},
		vaultKey(1): {{Epoch: 8, Commitment: bits(0, 1, 2)}, {Epoch: 9, Commitment: bits(0, 1)}},
		vaultKey(2): {{Epoch: 10, Commitment: bits(0)}},
	}
	elecs := map[uint64][]string{
		3:  {"a", "b", "old0"}, // old0: only in an early epoch of the RETIRING gen
		5:  {"a", "b"},
		8:  {"a", "b", "hive:Gone"}, // gone: left the ACTIVE gen after epoch 8
		9:  {"a", "b"},
		10: {"pend"}, // pend: only in a Pending (not fund-holding) gen
	}
	for _, tc := range []struct {
		acct string
		want bool
	}{
		{"a", true},     // current member
		{"gone", true},  // an earlier epoch of the active gen (prefix and case normalised)
		{"old0", true},  // an earlier epoch of a retiring gen: not released at rotation
		{"pend", false}, // Pending gen holds no funds
		{"nobody", false},
	} {
		got, err := holdCheck(t, vaults, rows, elecs, tc.acct)
		if err != nil || got != tc.want {
			t.Fatalf("%s: held=%v err=%v, want %v", tc.acct, got, err, tc.want)
		}
	}
	// Once the generations are drained (Purged), nothing holds.
	purged := []btcvault.Vault{{Generation: 0, Status: btcvault.VaultStatusPurged}, {Generation: 1, Status: btcvault.VaultStatusPurged}}
	if got, _ := holdCheck(t, purged, rows, elecs, "gone"); got {
		t.Fatal("a purged generation still locks its former holders")
	}
}

func TestPoa1_ReadErrorIsReturnedNotDecided(t *testing.T) {
	got, err := accountInFundedVaultCommittees([]btcvault.Vault{{Generation: 1, Status: btcvault.VaultStatusActive}}, "vsc1BTC", "a", nil,
		func(string) ([]tss_db.TssCommitment, error) { return nil, errors.New("transient") },
		func(uint64) (*elections.ElectionResult, error) { return nil, nil })
	if err == nil || !got {
		t.Fatalf("held=%v err=%v: a read error must be returned (and hold)", got, err)
	}
}

// Wiring through ExecuteTx (the registry read is replaced via the test seam).
func TestPoa1_UnstakeIsRefusedWhileTheBondedNodeHoldsAFundedShareAt090(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ver      uint64
		from, to string
		held     bool
		wantOK   bool
		wantText string
	}{
		{"self, holds a share, 0.9.0", 9, "hive:plain", "hive:plain", true, false, "this node is electable, recently active as a witness, in the current committee, or holds a share"},
		{"delegator, node holds a share, 0.9.0", 9, "hive:alt", "hive:plainnode", true, false, "the node you delegated to is electable, recently active as a witness, in the current committee, or holds a share"},
		{"no share, 0.9.0", 9, "hive:plain", "hive:plain", false, true, ""},
		{"holds a share, below 0.9.0", 7, "hive:plain", "hive:plain", true, true, ""},
	} {
		se, _, _ := poaEnv(t, tc.ver)
		var asked string
		se.heldShareCheck = func(account string, height uint64) (bool, error) {
			asked = account
			return tc.held, nil
		}
		led := &recordingLedger{}
		res := delegatedUnstake(tc.from, tc.to, 200).ExecuteTx(se, led, nil, nil, "")
		if res.Success != tc.wantOK || (tc.wantOK && led.unstakes != 1) || (!tc.wantOK && led.unstakes != 0) {
			t.Fatalf("%s: success=%v ledger calls=%d ret=%q", tc.name, res.Success, led.unstakes, res.Ret)
		}
		if tc.wantText != "" && !strings.Contains(res.Ret, tc.wantText) {
			t.Fatalf("%s: refusal %q, want it to say %q", tc.name, res.Ret, tc.wantText)
		}
		if tc.ver >= 9 && asked != tc.to {
			t.Fatalf("%s: checked %q, want the bonded node %q", tc.name, asked, tc.to)
		}
	}
}

func TestPoa1_TransientReadRetriesInsteadOfDeciding(t *testing.T) {
	se, _, _ := poaEnv(t, 9)
	calls := 0
	se.heldShareCheck = func(string, uint64) (bool, error) {
		calls++
		if calls < 3 {
			return true, errors.New("transient")
		}
		return false, nil
	}
	led := &recordingLedger{}
	res := delegatedUnstake("hive:plain", "hive:plain", 200).ExecuteTx(se, led, nil, nil, "")
	if !res.Success || calls != 3 {
		t.Fatalf("success=%v calls=%d: a transient read must be retried until it succeeds", res.Success, calls)
	}
}

// Regression: a member of the current election unstaked before the
// election's reshare landed, so the lock (read at submission) did not see it,
// and the bond paid out later while its new share signed. Current members are
// now locked while any generation holds funds.
func TestPoa1_CurrentElectionMemberLockedBeforeItsReshareLands(t *testing.T) {
	active := []btcvault.Vault{{Generation: 1, Status: btcvault.VaultStatusActive}}
	rows := map[string][]tss_db.TssCommitment{vaultKey(1): {{Epoch: 8, Commitment: bits(0)}}}
	elecs := map[uint64][]string{8: {"a"}}
	current := []string{"a", "hive:NewMember"}

	got, err := holdCheckWith(t, active, rows, elecs, current, "newmember")
	if err != nil || !got {
		t.Fatalf("a current member with no commitment yet must be locked: got %v %v", got, err)
	}
	got, _ = holdCheckWith(t, active, rows, elecs, nil, "newmember")
	if got {
		t.Fatalf("without the current election the same account is free (the bypass)")
	}
	pending := []btcvault.Vault{{Generation: 2, Status: btcvault.VaultStatusPending}}
	got, _ = holdCheckWith(t, pending, rows, elecs, current, "newmember")
	if got {
		t.Fatalf("no generation holds funds: current membership alone must not lock")
	}
}

// Regression: an election's members are fixed at its anchor, before it
// lands, so "member of the current election" misses a node elected at the
// anchor that unstakes before the landing. The seat exit-halt already closes
// that window for seats (electable now, or witness activity within the exit
// window); POA-1 now applies the same guard to every bonded account.
func TestPoa1_ElectableOrRecentlyActiveWitnessLocked(t *testing.T) {
	se, seats, wits := poaEnv(t, 9)
	seats.seats["topup"] = poaseats.Seat{Account: "topup"} // listed as a witness by the fake
	window := se.sconf.ConsensusParams().EffectivePoaExitHalt()
	const h = uint64(1_000_000)

	if held, err := se.electableOrRecentlyActive("topup", h); err != nil || !held {
		t.Fatalf("an enabled witness can be elected at the next anchor: must hold, got %v %v", held, err)
	}
	wits.disable("topup", h-10)
	if held, _ := se.electableOrRecentlyActive("topup", h); !held {
		t.Fatalf("disabled 10 blocks ago: an election anchored before that may not have landed: must hold")
	}
	wits.disable("topup", h-window-1)
	if held, _ := se.electableOrRecentlyActive("topup", h); held {
		t.Fatalf("witness-silent for a full window: must be free")
	}
	if held, _ := se.electableOrRecentlyActive("neverwitness", h); held {
		t.Fatalf("never a witness: must be free")
	}
}
