package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"vsc-node/lib/dids"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// MIGRATE-NOOP-RC-1 (testnet 11-19): only "nothing to migrate" was treated as a
// no-op, so the wait statuses migrateVault answers with before writing state
// were sent every tick while a sweep was in flight and while the old
// generation waited for gas: 50 confirmed no-op calls in 3 h, the bot's RC
// 123,935 -> 48,228.
func TestMIGRATENOOPRC1_WaitStatusesAreNotSent(t *testing.T) {
	l2 := newFakeL2()
	botKey, _ := ethCrypto.GenerateKey()
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: l2.gql(t),
		cfg: EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}}
	ret := map[string]string{}
	l2.sim = func(action string) map[string]any {
		return map[string]any{"success": true, "ret": ret[action], "rc_used": 865}
	}
	submitted := func() int { l2.mu.Lock(); defer l2.mu.Unlock(); return len(l2.submitted) }

	// Every success answer of HandleMigrateVault (account-mapping
	// mapping/migration.go) that builds nothing.
	for _, status := range []string{
		"nothing to migrate",
		"sweep in flight for generation 9",
		"gas reserve below the sweeps' gas (400000000000000 wei): migration waits for a reserve top-up",
		"generation 9 holds too little ETH for its token sweeps' gas (400000006400000 wei): top it up with a to_reserve deposit to 0xab508b8169e92fcf2f6658c981d0c14b1eb61fbc",
		"generation 9 holds usdc whose transfers reverted 2 times: an owner write-off is needed",
		"generation 9 holds no more ETH than one sweep's gas: dust, for writeOffDust",
	} {
		ret["migrateVault"] = status
		before := submitted()
		got, err := vaultOp(context.Background(), sub.gql, sub, "vsc1bridge", "migrateVault", json.RawMessage(`{}`))
		if !errors.Is(err, errVaultOpNoop) || got != status || submitted() != before {
			t.Errorf("status %q: err %v, %d submitted; want a no-op, nothing sent", status, err, submitted()-before)
		}
	}

	// A built sweep is sent: ETH, token, several at once with a status note, and
	// (as the BTC bot's F8 rule does) an answer the bot does not know, so a new
	// contract answer can cost RC but never stall a rotation.
	for _, built := range []string{
		"sweep 9-6 eth 300000007200000",
		"sweep 9-7 usdc 1000000",
		"sweep 9-7 eth 1957999998343000; sweep 9-8 usdc 1000000; generation 9 holds too little ETH for its token sweeps' gas (1 wei): top it up with a to_reserve deposit to 0xab50",
		"migrated generation 9 some new way",
	} {
		ret["migrateVault"] = built
		before := submitted()
		if got, err := vaultOp(context.Background(), sub.gql, sub, "vsc1bridge", "migrateVault", json.RawMessage(`{}`)); err != nil || got != built || submitted() != before+1 {
			t.Errorf("built %q: err %v, %d submitted; want it sent", built, err, submitted()-before)
		}
	}
}

// The dust answer is no longer sent, but its follow-up still runs: a
// generation left with only dust is written off.
func TestMIGRATENOOPRC1_DustStatusStillWritesOff(t *testing.T) {
	l2 := newFakeL2()
	botKey, _ := ethCrypto.GenerateKey()
	cfg := EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: l2.gql(t), cfg: cfg}
	l2.sim = func(action string) map[string]any {
		if action == "writeOffDust" {
			return map[string]any{"success": true, "ret": "wrote off 300000007200000 wei of generation 9", "rc_used": 700}
		}
		return map[string]any{"success": true, "ret": "nothing to migrate", "rc_used": 500}
	}
	migrateStatus(context.Background(), sub.gql, sub, cfg, "generation 9 holds no more ETH than one sweep's gas: dust, for writeOffDust")
	l2.mu.Lock()
	n := len(l2.submitted)
	l2.mu.Unlock()
	if n != 1 {
		t.Fatalf("dust status: %d L2 calls sent, want 1 (writeOffDust)", n)
	}
}
