package announcements_test

import (
	"testing"
	"time"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/announcements"

	"github.com/stretchr/testify/assert"
	"github.com/vsc-eco/hivego"
)

// ===== mocks =====

type mockHiveRpcClient struct {
	getAccountCallCount    int
	updateAccountCallCount int
}

func (m *mockHiveRpcClient) GetAccount(accountNames []string) ([]hivego.AccountData, error) {
	m.getAccountCallCount++
	dummyAcc := hivego.AccountData{
		MemoKey: "STM6n4WcwyiC63udKYR8jDFuzG9T48dhy2Qb5sVmQ9MyNuKM7xE29",
	}
	return []hivego.AccountData{dummyAcc}, nil
}

func (m *mockHiveRpcClient) UpdateAccount(account string, owner *hivego.Auths, active *hivego.Auths, posting *hivego.Auths, jsonMetadata string, memoKey string, wif *string) (string, error) {
	m.updateAccountCallCount++
	return "", nil // indicates success, despite looking unimplemented
}

// ===== tests =====

func TestImmediateExecution(t *testing.T) {
	hiveRpcClient := &mockHiveRpcClient{}
	conf := announcements.NewAnnouncementsConfig()
	agg := aggregate.New([]aggregate.Plugin{
		conf,
		announcements.New(hiveRpcClient, conf, time.Second*15),
	})
	test_utils.RunPlugin(t, agg)

	// wait for the cron job timer to start and immediate cron to run in a routine
	time.Sleep(1 * time.Second)

	assert.Equal(t, 1, hiveRpcClient.getAccountCallCount)
	assert.Equal(t, 1, hiveRpcClient.updateAccountCallCount)
}

func TestCronExecutions(t *testing.T) {
	hiveRpcClient := &mockHiveRpcClient{}
	conf := announcements.NewAnnouncementsConfig()
	agg := aggregate.New([]aggregate.Plugin{
		conf,
		announcements.New(hiveRpcClient, conf, time.Second*2),
	})
	test_utils.RunPlugin(t, agg)

	// wait for the cron job timer to start and immediate cron to run in a routine
	// as well as a few more cron executions
	time.Sleep(8 * time.Second)

	assert.True(t, hiveRpcClient.getAccountCallCount <= 6)
	assert.True(t, hiveRpcClient.updateAccountCallCount >= 4)
}
