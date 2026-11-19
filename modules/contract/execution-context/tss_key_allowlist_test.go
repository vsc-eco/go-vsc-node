package contract_execution_context_test

import (
	"testing"

	"vsc-node/lib/test_utils"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	contract_session "vsc-node/modules/contract/session"
	tss_db "vsc-node/modules/db/vsc/tss"
)

// KEY-SPAM-1 (18j).
//
// tss.create_key validated only that the key name was alphanumeric and the type
// was eddsa/ecdsa, for 1,000,000 gas. Any contract could mint unlimited keys,
// every "created" key becomes a keygen at each rotate block with no cap and no
// expiry, and each keygen waits for pre-parameters while holding tssMgr.lock —
// so N junk keys hold that lock for about N minutes and every reshare and sign
// tick in between is dropped. Cheap, permissionless, and it degrades the
// custody layer for every key rather than just the spamming contract.
//
// The allowlist is enforced in TssCreateKey rather than in the two SDK
// functions because the `tss` and `tss_v2` namespaces both funnel through it,
// so neither namespace can be left behind.

const (
	btcMappingContract = "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d" // the only key owner on mainnet
	spamContract       = "vsc1SpamSpamSpamSpamSpamSpamSpamSpamSpam"
)

func TestContractMayCreateTssKey(t *testing.T) {
	mainnetList := []string{btcMappingContract}

	t.Run("a listed contract is permitted", func(t *testing.T) {
		if !contract_execution_context.ContractMayCreateTssKey(mainnetList, btcMappingContract) {
			t.Fatal("the mainnet key owner was refused")
		}
	})

	t.Run("an unlisted contract is refused", func(t *testing.T) {
		if contract_execution_context.ContractMayCreateTssKey(mainnetList, spamContract) {
			t.Fatal("an unlisted contract was permitted — KEY-SPAM-1 is not fixed")
		}
	})

	// The pre-activation and test-network state. These two cases are what make
	// the gate fork-safe: the state engine passes nil until consensus 0.13.0, so
	// a node running this binary below that version must accept exactly the same
	// create_key calls as one that is not running it at all.
	t.Run("nil is unrestricted", func(t *testing.T) {
		if !contract_execution_context.ContractMayCreateTssKey(nil, spamContract) {
			t.Fatal("a nil allowlist must not restrict anything — the gate would not be fork-safe")
		}
	})

	t.Run("empty is unrestricted", func(t *testing.T) {
		if !contract_execution_context.ContractMayCreateTssKey([]string{}, spamContract) {
			t.Fatal("an empty allowlist must not restrict anything — that is how test networks are configured")
		}
	})

	// Membership is exact. A prefix match in either direction would be a hole:
	// contract ids are attacker-chosen at deploy time.
	t.Run("membership is exact, not a prefix match", func(t *testing.T) {
		list := []string{"vsc1AAA", btcMappingContract, "vsc1ZZZ"}
		if !contract_execution_context.ContractMayCreateTssKey(list, btcMappingContract) {
			t.Fatal("a listed contract in the middle of the list was refused")
		}
		if contract_execution_context.ContractMayCreateTssKey(list, btcMappingContract+"x") {
			t.Fatal("a contract id that merely EXTENDS a listed id was permitted")
		}
		if contract_execution_context.ContractMayCreateTssKey(list, "vsc1AA") {
			t.Fatal("a contract id that is a PREFIX of a listed id was permitted")
		}
		if contract_execution_context.ContractMayCreateTssKey(list, "") {
			t.Fatal("an empty contract id was permitted")
		}
	})
}

// And end to end through TssCreateKey itself, for the refusal — which returns
// before any session write, so it needs no datalayer. (The success path's
// AppendTssLog does, and is covered by every existing create_key test, all of
// which run with no allowlist and must therefore keep passing unchanged.)
func TestTssCreateKey_RefusesUnlistedContract(t *testing.T) {
	keys := &test_utils.MockTssKeysDb{Keys: map[string]tss_db.TssKey{}}
	ctx := contract_execution_context.New(
		contract_execution_context.Environment{ContractId: spamContract},
		1000, 100000, 100000, nil,
		contract_session.NewCallSession(nil, nil, &test_utils.MockContractStateDb{}, keys, nil, 0, nil), 0,
		contract_execution_context.WithTssKeyAllowlist([]string{btcMappingContract}),
	)

	res := ctx.TssCreateKey("mainv2", "ecdsa", tss_db.MaxKeyEpochs)
	if res.IsOk() {
		t.Fatalf("an unlisted contract created a TSS key (%q) — KEY-SPAM-1 is not fixed", res.Unwrap())
	}
	t.Logf("refused: %v", res.UnwrapErr())
}
