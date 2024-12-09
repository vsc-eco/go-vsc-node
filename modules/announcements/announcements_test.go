package announcements_test

import (
	"context"
	"fmt"
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
		// public memo key, ok to be public/exposed
		MemoKey: "STM6n4WcwyiC63udKYR8jDFuzG9T48dhy2Qb5sVmQ9MyNuKM7xE29",
		// the rest of the fields are not important for this test/the announcer in general
	}
	return []hivego.AccountData{dummyAcc}, nil
}

func (m *mockHiveRpcClient) UpdateAccount(account string, owner *hivego.Auths, active *hivego.Auths, posting *hivego.Auths, jsonMetadata string, memoKey string, wif *string) (string, error) {
	m.updateAccountCallCount++
	return "", nil // indicates success, despite looking unimplemented
}

// ===== tests =====

func TestImmediateExecution(t *testing.T) {
	// create new clients
	hiveRpcClient := &mockHiveRpcClient{}
	conf := announcements.NewAnnouncementsConfig()
	anouncementsManager, err := announcements.New(hiveRpcClient, conf, time.Second*15)
	assert.NoError(t, err)

	agg := aggregate.New([]aggregate.Plugin{
		conf,
		anouncementsManager,
	})
	test_utils.RunPlugin(t, agg)

	// wait for the cron job timer to start and immediate cron to run in a routine
	time.Sleep(1 * time.Second)

	// ensure it only ran once using the initial execution
	assert.Equal(t, 1, hiveRpcClient.getAccountCallCount)
	assert.Equal(t, 1, hiveRpcClient.updateAccountCallCount)
}

func TestCronExecutions(t *testing.T) {
	// create new clients
	hiveRpcClient := &mockHiveRpcClient{}
	conf := announcements.NewAnnouncementsConfig()
	anouncementsManager, err := announcements.New(hiveRpcClient, conf, time.Second*2)
	assert.NoError(t, err)
	agg := aggregate.New([]aggregate.Plugin{
		conf,
		anouncementsManager,
	})
	test_utils.RunPlugin(t, agg)

	// wait for the cron job timer to start and immediate cron to run in a routine
	// as well as a few more cron executions
	time.Sleep(8 * time.Second)

	fmt.Println(hiveRpcClient.getAccountCallCount)

	// ensure both getAccount and updateAccount were called between 4 and 6 times,
	// meaning the cron job is running as expected
	assert.True(t, hiveRpcClient.getAccountCallCount <= 6)
	assert.True(t, hiveRpcClient.getAccountCallCount >= 4)
	assert.True(t, hiveRpcClient.updateAccountCallCount <= 6)
	assert.True(t, hiveRpcClient.updateAccountCallCount >= 4)
}

func TestStopAnnouncer(t *testing.T) {
	// create new clients
	hiveRpcClient := &mockHiveRpcClient{}
	conf := announcements.NewAnnouncementsConfig()
	anouncementsManager, err := announcements.New(hiveRpcClient, conf, time.Second*2)
	assert.NoError(t, err)
	agg := aggregate.New([]aggregate.Plugin{
		conf,
		anouncementsManager,
	})
	// init and start
	assert.NoError(t, agg.Init())
	_, err = agg.Start().Await(context.Background())
	assert.NoError(t, err)

	// wait a bit to allow some cron executions
	time.Sleep(5 * time.Second)

	// get the number of times the getAccount method was called
	ranNTimes := hiveRpcClient.getAccountCallCount

	// stop, and then wait a bit to ensure no more cron executions happen after stopping
	agg.Stop()
	time.Sleep(5 * time.Second)
	assert.Equal(t, ranNTimes, hiveRpcClient.getAccountCallCount)
}

func TestInvalidAnnouncementsFrequencySetup(t *testing.T) {
	// create new clients
	hiveRpcClient := &mockHiveRpcClient{}
	conf := announcements.NewAnnouncementsConfig()
	_, err := announcements.New(hiveRpcClient, conf, time.Second*0)
	assert.Error(t, err)
	_, err = announcements.New(hiveRpcClient, conf, time.Second*-1)
	assert.Error(t, err)
	_, err = announcements.New(hiveRpcClient, conf, time.Second*-2)
	assert.Error(t, err)
	_, err = announcements.New(hiveRpcClient, conf, time.Second*3)
	assert.NoError(t, err)
}

func TestInvalidRpcClient(t *testing.T) {
	conf := announcements.NewAnnouncementsConfig()
	_, err := announcements.New(nil, conf, time.Second*2)
	assert.Error(t, err)
}
