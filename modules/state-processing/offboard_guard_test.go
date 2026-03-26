package state_engine_test

import (
	"math"
	"testing"
	"vsc-node/lib/logger"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	ledgerSystem "vsc-node/modules/ledger-system"
	rcSystem "vsc-node/modules/rc-system"

	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

// --- Mock StateEngine ---

type mockStateEngine struct {
	election elections.ElectionResult
	sysConf  systemconfig.SystemConfig
}

func (m *mockStateEngine) Log() logger.Logger {
	return logger.PrefixedLogger{Prefix: "test"}
}

func (m *mockStateEngine) DataLayer() common_types.DataLayer {
	return nil
}

func (m *mockStateEngine) GetContractInfo(id string, height uint64) (contracts.Contract, bool) {
	return contracts.Contract{}, false
}

func (m *mockStateEngine) GetElectionInfo(height ...uint64) elections.ElectionResult {
	return m.election
}

func (m *mockStateEngine) SystemConfig() systemconfig.SystemConfig {
	return m.sysConf
}

// --- Mock LedgerSession ---

type mockLedgerSession struct {
	balances map[string]int64 // key: "account|asset"
}

func newMockLedgerSession() *mockLedgerSession {
	return &mockLedgerSession{
		balances: make(map[string]int64),
	}
}

func (m *mockLedgerSession) setBalance(account string, asset string, amount int64) {
	m.balances[account+"|"+asset] = amount
}

func (m *mockLedgerSession) GetBalance(account string, blockHeight uint64, asset string) int64 {
	return m.balances[account+"|"+asset]
}

func (m *mockLedgerSession) ExecuteTransfer(op ledgerSystem.OpLogEvent, options ...ledgerSystem.TransferOptions) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{Ok: true}
}

func (m *mockLedgerSession) Withdraw(w ledgerSystem.WithdrawParams) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{Ok: true}
}

func (m *mockLedgerSession) Stake(op ledgerSystem.StakeOp, options ...ledgerSystem.TransferOptions) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{Ok: true}
}

func (m *mockLedgerSession) Unstake(op ledgerSystem.StakeOp) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{Ok: true}
}

func (m *mockLedgerSession) ConsensusStake(p ledgerSystem.ConsensusParams) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{Ok: true}
}

func (m *mockLedgerSession) ConsensusUnstake(p ledgerSystem.ConsensusParams) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{Ok: true, Msg: "success"}
}

func (m *mockLedgerSession) Done() []string {
	return nil
}

func (m *mockLedgerSession) Revert() {}

// --- Mock RcSession ---

type mockRcSession struct{}

func (m *mockRcSession) Consume(account string, blockHeight uint64, rcAmt int64) (bool, int64) {
	return true, rcAmt
}

func (m *mockRcSession) CanConsume(account string, blockHeight uint64, rcAmt int64) (bool, int64, int64) {
	return true, 0, 0
}

func (m *mockRcSession) Done() rcSystem.RcMapResult {
	return rcSystem.RcMapResult{RcMap: make(map[string]int64)}
}

func (m *mockRcSession) Revert() {}

// --- Helpers ---

func makeElection(memberAccounts []string, etype string, epoch uint64) elections.ElectionResult {
	members := make([]elections.ElectionMember, len(memberAccounts))
	weights := make([]uint64, len(memberAccounts))
	for i, account := range memberAccounts {
		members[i] = elections.ElectionMember{
			Key:     "did:key:test" + account,
			Account: account,
		}
		weights[i] = 10
	}
	return elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{
			Epoch: epoch,
			Type:  etype,
		},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: members,
			Weights: weights,
		},
	}
}

func makeTxUnstake(from string, amount string, blockHeight uint64) stateEngine.TxConsensusUnstake {
	return stateEngine.TxConsensusUnstake{
		Self: stateEngine.TxSelf{
			RequiredAuths: []string{from},
			BlockHeight:   blockHeight,
			TxId:          "test-tx",
			OpIndex:       0,
		},
		From:  from,
		To:    from,
		Amount: amount,
		Asset: "HIVE",
		NetId: "vsc-mocknet",
	}
}

// MinStake in milliHIVE. SafeParseHiveFloat("2000.000") = 2000000.
// Real mainnet MinStake is 2_000_000 (= 2000 HIVE in milliHIVE).
const testMinStake = int64(2_000_000)

// --- Tests ---

func TestOffboardGuard_AllowsUnstakeWhenSafe(t *testing.T) {
	// 10 members all staked. Unstake 1 should succeed.
	// Remaining: 9. Required: ceil(20/3) = 7. 9 >= 7: OK.
	accounts := []string{"w0", "w1", "w2", "w3", "w4", "w5", "w6", "w7", "w8", "w9"}
	se := &mockStateEngine{
		election: makeElection(accounts, "staked", 10),
		sysConf:  &mockSysConfig{minStake: testMinStake, netId: "vsc-mocknet"},
	}

	ls := newMockLedgerSession()
	for _, acct := range accounts {
		ls.setBalance("hive:"+acct, "hive_consensus", testMinStake) // 2_000_000 milliHIVE = 2000 HIVE
	}

	tx := makeTxUnstake("hive:w0", "2000.000", 100)
	result := tx.ExecuteTx(se, ls, &mockRcSession{}, nil, "hive:w0")

	assert.True(t, result.Success, "unstake should succeed when 9 of 10 remain: %s", result.Ret)
}

func TestOffboardGuard_RejectsWhenThresholdBreached(t *testing.T) {
	// 7 members. 2 already have 0 stake. Third tries to unstake → rejected.
	// Remaining would be: 4. Required: ceil(14/3) = 5. 4 < 5: REJECTED.
	accounts := []string{"w0", "w1", "w2", "w3", "w4", "w5", "w6"}
	se := &mockStateEngine{
		election: makeElection(accounts, "staked", 10),
		sysConf:  &mockSysConfig{minStake: testMinStake, netId: "vsc-mocknet"},
	}

	ls := newMockLedgerSession()
	// 5 members staked, 2 already at 0
	ls.setBalance("hive:w0", "hive_consensus", testMinStake)
	ls.setBalance("hive:w1", "hive_consensus", testMinStake)
	ls.setBalance("hive:w2", "hive_consensus", testMinStake)
	ls.setBalance("hive:w3", "hive_consensus", testMinStake)
	ls.setBalance("hive:w4", "hive_consensus", testMinStake)
	ls.setBalance("hive:w5", "hive_consensus", 0) // already unstaked
	ls.setBalance("hive:w6", "hive_consensus", 0) // already unstaked

	tx := makeTxUnstake("hive:w0", "2000.000", 100)
	result := tx.ExecuteTx(se, ls, &mockRcSession{}, nil, "hive:w0")

	assert.False(t, result.Success, "unstake should be rejected")
	assert.Contains(t, result.Ret, "2/3")
}

func TestOffboardGuard_AllowsPartialUnstake(t *testing.T) {
	// Member unstakes half their balance, stays above MinStake.
	// Guard should not fire regardless of member count.
	accounts := []string{"w0", "w1", "w2", "w3", "w4", "w5", "w6"}
	se := &mockStateEngine{
		election: makeElection(accounts, "staked", 10),
		sysConf:  &mockSysConfig{minStake: testMinStake, netId: "vsc-mocknet"},
	}

	ls := newMockLedgerSession()
	for _, acct := range accounts {
		ls.setBalance("hive:"+acct, "hive_consensus", testMinStake*2) // double MinStake
	}

	// Unstake half — still above MinStake
	tx := makeTxUnstake("hive:w0", "2000.000", 100)
	result := tx.ExecuteTx(se, ls, &mockRcSession{}, nil, "hive:w0")

	assert.True(t, result.Success, "partial unstake keeping above MinStake should succeed: %s", result.Ret)
}

func TestOffboardGuard_AllowsNonMemberUnstake(t *testing.T) {
	// Someone not in the election unstakes — should always succeed.
	accounts := []string{"w0", "w1", "w2", "w3", "w4", "w5", "w6"}
	se := &mockStateEngine{
		election: makeElection(accounts, "staked", 10),
		sysConf:  &mockSysConfig{minStake: testMinStake, netId: "vsc-mocknet"},
	}

	ls := newMockLedgerSession()
	for _, acct := range accounts {
		ls.setBalance("hive:"+acct, "hive_consensus", testMinStake) // 2_000_000 milliHIVE = 2000 HIVE
	}
	ls.setBalance("hive:outsider", "hive_consensus", testMinStake)

	tx := makeTxUnstake("hive:outsider", "2000.000", 100)
	result := tx.ExecuteTx(se, ls, &mockRcSession{}, nil, "hive:outsider")

	assert.True(t, result.Success, "non-member unstake should succeed: %s", result.Ret)
}

func TestOffboardGuard_SkipsInitialElection(t *testing.T) {
	// Election type "initial" — guard should be skipped entirely.
	accounts := []string{"w0", "w1", "w2"}
	se := &mockStateEngine{
		election: makeElection(accounts, "initial", 0),
		sysConf:  &mockSysConfig{minStake: testMinStake, netId: "vsc-mocknet"},
	}

	ls := newMockLedgerSession()
	for _, acct := range accounts {
		ls.setBalance("hive:"+acct, "hive_consensus", testMinStake) // 2_000_000 milliHIVE = 2000 HIVE
	}

	// Even with only 3 members, unstake should succeed in initial mode
	tx := makeTxUnstake("hive:w0", "2000.000", 100)
	result := tx.ExecuteTx(se, ls, &mockRcSession{}, nil, "hive:w0")

	assert.True(t, result.Success, "unstake in initial election should succeed: %s", result.Ret)
}

func TestOffboardGuard_ThresholdMathVerification(t *testing.T) {
	// Verify the math for various committee sizes
	tests := []struct {
		total    int
		required int // ceil(total * 2/3)
		canLeave int // total - required
	}{
		{7, 5, 2},
		{10, 7, 3},
		{15, 10, 5},
		{20, 14, 6},
		{21, 14, 7},
		{30, 20, 10},
	}

	for _, tt := range tests {
		required := int(math.Ceil(float64(tt.total) * 2.0 / 3.0))
		assert.Equal(t, tt.required, required, "threshold for %d members", tt.total)
		assert.Equal(t, tt.canLeave, tt.total-required, "can leave for %d members", tt.total)
	}
}

// --- Mock SystemConfig for tests ---

type mockSysConfig struct {
	minStake int64
	netId    string
}

func (m *mockSysConfig) OnMainnet() bool                      { return false }
func (m *mockSysConfig) OnMocknet() bool                      { return true }
func (m *mockSysConfig) BootstrapPeers() []string             { return nil }
func (m *mockSysConfig) NetId() string                        { return m.netId }
func (m *mockSysConfig) GatewayWallet() string                { return "" }
func (m *mockSysConfig) ConsensusParams() params.ConsensusParams {
	return params.ConsensusParams{
		MinStake:   m.minStake,
		MinRcLimit: 100,
	}
}
