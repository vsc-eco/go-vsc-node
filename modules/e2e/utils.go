package e2e

import (
	"crypto"
	"time"

	"github.com/vsc-eco/hivego"
)

type MockHiveRpcClient struct {
}

func (m *MockHiveRpcClient) GetAccount(accountNames []string) ([]hivego.AccountData, error) {
	account := hivego.AccountData{
		MemoKey: "STM6n4WcwyiC63udKYR8jDFuzG9T48dhy2Qb5sVmQ9MyNuKM7xE29",
	}

	return []hivego.AccountData{account}, nil
}

func (m *MockHiveRpcClient) UpdateAccount(account string, owner *hivego.Auths, active *hivego.Auths, posting *hivego.Auths, jsonMetadata string, memoKey string, wif *string) (string, error) {
	return "mock-dont-use-this", nil
}

func HashSeed(seed []byte) *hivego.KeyPair {
	h := crypto.SHA256.New()
	h.Write(seed)
	hSeed := h.Sum(nil)
	return hivego.KeyPairFromBytes(hSeed)
}

func TimeToBlocks(time time.Duration) uint64 {
	return uint64(time.Seconds() / 3)
}
