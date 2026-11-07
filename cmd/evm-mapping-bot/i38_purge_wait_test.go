package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"vsc-node/lib/btcvault"
)

// I-38: an Inactive generation's purge waits only for the stored L1 height.
// Once the bot has scanned past the purge height (Inactive height + grace +
// map window; 64 + 64 off mainnet), the needs file asks for a proof until the
// stored height reaches it; not before L1 is that far, and not after.
func TestI38_APurgeWaitingOnTheStoredHeightAsksForAProof(t *testing.T) {
	bridgeChainID.Lock()
	bridgeChainID.id = 0
	bridgeChainID.Unlock()
	l2 := newFakeL2()
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1bridge", "chainid", "11155111")
	gql := l2.gql(t)
	path := t.TempDir() + "/needs.json"
	t.Setenv("NEEDS_FILE", path)
	cfg := EVMBotConfig{ContractID: "vsc1bridge"}
	read := func() (bool, []string) {
		var m struct {
			Want    bool     `json:"want_proof"`
			Reasons []string `json:"reasons"`
		}
		b, _ := os.ReadFile(path)
		_ = json.Unmarshal(b, &m)
		return m.Want, m.Reasons
	}
	view := vaultView{Registry: true, Gens: []vaultGen{
		{Gen: 1, Status: btcvault.VaultStatusInactive, InactiveHeight: 1000, Address: "0x01"},
		{Gen: 2, Status: btcvault.VaultStatusActive, Address: "0x02"},
	}}
	view.Active = &view.Gens[1]
	cp := &Checkpoint{HeaderNeeds: map[uint64]int64{}, LastScannedBlock: 1100}
	ctx := context.Background()

	notePurgeWait(ctx, gql, cp, cfg, view) // L1 not yet past 1000 + 128
	writeNeedsFile(ctx, gql, cp, cfg, 1050, false)
	if want, r := read(); want {
		t.Fatalf("asked for a proof before L1 reached the purge height: %v", r)
	}
	cp.LastScannedBlock = 1130
	notePurgeWait(ctx, gql, cp, cfg, view)
	writeNeedsFile(ctx, gql, cp, cfg, 1050, false)
	if want, r := read(); !want || len(r) != 1 || !strings.Contains(r[0], "purge waits for the stored L1 height to reach 1128") {
		t.Fatalf("scanned past the purge height, stored height behind: want_proof %v %v", want, r)
	}
	writeNeedsFile(ctx, gql, cp, cfg, 1130, false) // a proof moved the stored height past it
	if want, r := read(); want {
		t.Fatalf("still asking once the stored height reached the purge height: %v", r)
	}
	view.Gens[0].Status = btcvault.VaultStatusPurged
	notePurgeWait(ctx, gql, cp, cfg, view)
	if cp.PurgeWantTip != 0 {
		t.Fatalf("PurgeWantTip %d after the purge, want 0", cp.PurgeWantTip)
	}
	if purgeWaitBlocks(1) != 223_200 {
		t.Fatalf("mainnet purge wait %d, want 216,000 + 7,200", purgeWaitBlocks(1))
	}
}
