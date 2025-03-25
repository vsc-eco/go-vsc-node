package hive

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"time"

	"github.com/vsc-eco/hivego"
)

type globalProps struct {
	HeadBlockNumber int    `json:"head_block_number"`
	HeadBlockId     string `json:"head_block_id"`
	Time            string `json:"time"`
}

type HiveTransactionCreator interface {
	CustomJson(requiredAuths []string, requiredPostingAuths []string, id string, json string) hivego.HiveOperation
	Transfer(from string, to string, amount string, asset string, memo string) hivego.HiveOperation
	TransferToSavings(from string, to string, amount string, asset string, memo string) hivego.HiveOperation
	TransferFromSavings(from string, to string, amount string, asset string, memo string, requestId int) hivego.HiveOperation

	UpdateAccount(username string, owner *hivego.Auths, active *hivego.Auths, posting *hivego.Auths, jsonMetadata string, memoKey string) hivego.HiveOperation

	MakeTransaction(ops []hivego.HiveOperation) hivego.HiveTransaction

	//Use prior to signing
	PopulateSigningProps(tx *hivego.HiveTransaction, bh []int) error
	Sign(tx hivego.HiveTransaction) (string, error)
	Broadcast(tx hivego.HiveTransaction) (string, error)
}

type TransactionBroadcaster struct {
	Client  *hivego.HiveRpcNode
	KeyPair func() (*hivego.KeyPair, error)
}

func (t *TransactionBroadcaster) Broadcast(tx hivego.HiveTransaction) (string, error) {
	// err := t.PopulateSigningProps(&tx)

	// if err != nil {
	// 	return "", err
	// }

	return t.Client.BroadcastRaw(tx)
}

func (t *TransactionBroadcaster) PopulateSigningProps(tx *hivego.HiveTransaction, bh []int) error {
	byteProps, err := t.Client.GetDynamicGlobalProps()
	if err != nil {
		return err
	}

	var props globalProps
	err = json.Unmarshal(byteProps, &props)
	if err != nil {
		return err
	}

	refBlockNum := uint16(props.HeadBlockNumber & 0xffff)
	hbidB, err := hex.DecodeString(props.HeadBlockId)
	if err != nil {
		return err
	}
	refBlockPrefix := binary.LittleEndian.Uint32(hbidB[4:])

	exp, err := time.Parse("2006-01-02T15:04:05", props.Time)
	if err != nil {
		return err
	}
	exp = exp.Add(30 * time.Second)
	expStr := exp.Format("2006-01-02T15:04:05")

	tx.Expiration = expStr
	tx.RefBlockNum = refBlockNum
	tx.RefBlockPrefix = refBlockPrefix

	return nil
}

type TransactionCrafter struct {
}

func (t *TransactionCrafter) CustomJson(requiredAuths []string, requiredPostingAuths []string, id string, json string) hivego.HiveOperation {
	op := hivego.CustomJsonOperation{
		RequiredAuths:        requiredAuths,
		RequiredPostingAuths: requiredPostingAuths,
		Id:                   id,
		Json:                 json,
	}
	return op
}

func (t *TransactionCrafter) Transfer(from string, to string, amount string, asset string, memo string) hivego.HiveOperation {
	op := hivego.TransferOperation{
		From:   from,
		To:     to,
		Amount: amount + " " + asset,
		Memo:   memo,
	}
	return op
}

func (t *TransactionCrafter) UpdateAccount(username string, owner *hivego.Auths, active *hivego.Auths, posting *hivego.Auths, jsonMetadata string, memoKey string) hivego.HiveOperation {
	op := hivego.AccountUpdateOperation{
		Account:      username,
		Owner:        owner,
		Active:       active,
		Posting:      posting,
		MemoKey:      memoKey,
		JsonMetadata: jsonMetadata,
	}
	return op
}

func (t *TransactionCrafter) TransferToSavings(from string, to string, amount string, asset string, memo string) hivego.HiveOperation {
	op := hivego.TransferToSavings{
		From:   from,
		To:     to,
		Amount: amount + " " + asset,
		Memo:   memo,
	}
	return op
}

func (t *TransactionCrafter) TransferFromSavings(from string, to string, amount string, asset string, memo string, requestId int) hivego.HiveOperation {
	op := hivego.TransferFromSavings{
		From:      from,
		To:        to,
		Amount:    amount + " " + asset,
		Memo:      memo,
		RequestId: requestId,
	}
	return op
}

func (t *TransactionCrafter) CancelTransferFromSavings(from string, requestId int) hivego.HiveOperation {
	op := hivego.CancelTransferFromSavings{
		From:      from,
		RequestId: requestId,
	}
	return op
}

func (t *TransactionBroadcaster) Sign(tx hivego.HiveTransaction) (string, error) {
	kp, err := t.KeyPair()
	if err != nil {
		return "", err
	}
	return tx.Sign(*kp)
}

func (t *TransactionCrafter) MakeTransaction(ops []hivego.HiveOperation) hivego.HiveTransaction {
	tx := hivego.HiveTransaction{
		Operations: ops,
		Signatures: []string{},
	}
	return tx
}

type LiveTransactionCreator struct {
	TransactionBroadcaster
	TransactionCrafter
}

type MockTransactionBroadcaster struct {
	Callback func(tx hivego.HiveTransaction) error
	KeyPair  *hivego.KeyPair
}

func (mtb *MockTransactionBroadcaster) Broadcast(tx hivego.HiveTransaction) (string, error) {
	if mtb.Callback != nil {
		err := mtb.Callback(tx)

		if err != nil {
			return "", nil
		}
	}

	return tx.GenerateTrxId()
}

func (mtb *MockTransactionBroadcaster) Sign(tx hivego.HiveTransaction) (string, error) {
	return tx.Sign(*mtb.KeyPair)
}

func (mtb *MockTransactionBroadcaster) PopulateSigningProps(tx *hivego.HiveTransaction, bh []int) error {
	tx.Expiration = "2030-01-01T00:00:01"
	var a = rand.Uint32()

	tx.RefBlockNum = uint16(a % 32767)
	return nil
}

type MockTransactionCreator struct {
	MockTransactionBroadcaster
	TransactionCrafter
}

var _ HiveTransactionCreator = &LiveTransactionCreator{}
