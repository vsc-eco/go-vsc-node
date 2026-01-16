package announcements_test

import (
	"context"
	"fmt"
	"testing"
	"time"
	"vsc-node/lib/hive"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/announcements"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/witnesses"
	p2pInterface "vsc-node/modules/p2p"

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

func mockPeer(sysConf systemconfig.SystemConfig, idConf common.IdentityConfig) *p2pInterface.P2PServer {
	wits := witnesses.NewEmptyWitnesses()
	p2p := p2pInterface.New(wits, idConf, sysConf, nil)
	return p2p
}

// ===== tests =====

func TestImmediateExecution(t *testing.T) {
	// create new clients
	hiveRpcClient := &mockHiveRpcClient{}
	sysConfig := systemconfig.MocknetConfig()
	conf := common.NewIdentityConfig()

	txCreator := hive.LiveTransactionCreator{
		TransactionBroadcaster: hive.TransactionBroadcaster{
			KeyPair: conf.HiveActiveKeyPair,
			Client:  nil,
		},
		TransactionCrafter: hive.TransactionCrafter{},
	}
	p2p := mockPeer(sysConfig, conf)

	anouncementsManager, err := announcements.New(hiveRpcClient, conf, sysConfig, time.Second*15, &txCreator, p2p)
	assert.NoError(t, err)

	agg := aggregate.New([]aggregate.Plugin{
		conf,
		p2p,
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
	sysConfig := systemconfig.MocknetConfig()
	conf := common.NewIdentityConfig()

	txCreator := hive.LiveTransactionCreator{
		TransactionBroadcaster: hive.TransactionBroadcaster{
			KeyPair: conf.HiveActiveKeyPair,
			Client:  nil,
		},
		TransactionCrafter: hive.TransactionCrafter{},
	}
	anouncementsManager, err := announcements.New(hiveRpcClient, conf, sysConfig, time.Second*2, &txCreator, mockPeer(sysConfig, conf))
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
	sysConfig := systemconfig.MocknetConfig()
	conf := common.NewIdentityConfig()

	txCreator := hive.LiveTransactionCreator{
		TransactionBroadcaster: hive.TransactionBroadcaster{
			KeyPair: nil,
			Client:  nil,
		},
		TransactionCrafter: hive.TransactionCrafter{},
	}
	p2p := mockPeer(sysConfig, conf)
	anouncementsManager, err := announcements.New(hiveRpcClient, conf, sysConfig, time.Second*2, &txCreator, p2p)
	assert.NoError(t, err)
	agg := aggregate.New([]aggregate.Plugin{
		conf,
		p2p,
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
	sysConfig := systemconfig.MocknetConfig()
	conf := common.NewIdentityConfig()
	peerGetter := mockPeer(sysConfig, conf)
	_, err := announcements.New(hiveRpcClient, conf, sysConfig, time.Second*0, nil, peerGetter)
	assert.Error(t, err)
	_, err = announcements.New(hiveRpcClient, conf, sysConfig, time.Second*-1, nil, peerGetter)
	assert.Error(t, err)
	_, err = announcements.New(hiveRpcClient, conf, sysConfig, time.Second*-2, nil, peerGetter)
	assert.Error(t, err)
	_, err = announcements.New(hiveRpcClient, conf, sysConfig, time.Second*3, nil, peerGetter)
	assert.NoError(t, err)
}

func TestInvalidRpcClient(t *testing.T) {
	sysConfig := systemconfig.MocknetConfig()
	conf := common.NewIdentityConfig()
	_, err := announcements.New(nil, conf, sysConfig, time.Second*2, nil, mockPeer(sysConfig, conf))
	assert.Error(t, err)
}
