package e2e

import (
	"crypto"
	"errors"
	"fmt"
	"time"
	"vsc-node/modules/db/vsc/transactions"

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

func StepWaitStart() Step {
	return Step{
		Name: "Wait To Start",
	}
}

func StepElectionStart() Step {
	return Step{
		Name: "Creating Election",
	}
}

func StepWait() Step {
	return Step{
		Name: "Wait",
	}
}

type TxStatusAssert struct {
	TxId           string
	ExpectedStatus transactions.TransactionStatus
}

func TxStatusAssertion(txns []TxStatusAssert, waitTimeSec uint) EvaluateFunc {
	return func(ctx StepCtx) error {
		time.Sleep(time.Duration(waitTimeSec) * time.Second)

		runner := ctx.Container.Runner()

		for _, txn := range txns {
			getTransaction := runner.TxDb.GetTransaction(txn.TxId)
			if getTransaction == nil {
				return errors.New("non-existent transaction")
			}
			tx := *getTransaction
			if tx.Status != string(txn.ExpectedStatus) {
				return fmt.Errorf("incorrect status should be %s status is: %s", txn.ExpectedStatus, tx.Status)
			}
		}

		return nil
	}
}
