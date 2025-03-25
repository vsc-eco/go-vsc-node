package gateway_test

import (
	"bytes"
	"crypto"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
	"vsc-node/lib/hive"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/vsc-eco/hivego"
)

var MS_TEST_KEY = ""

func KeyPairFromBytes(privKey []byte) *hivego.KeyPair {
	prvKey, pubKey := secp256k1.PrivKeyFromBytes(privKey)

	return &hivego.KeyPair{prvKey, pubKey}
}

func hash256(data []byte) []byte {
	h := crypto.SHA256.New()
	h.Write(data)
	return h.Sum(nil)
}
func hashKey(key string, account string) struct {
	Owner   hivego.KeyPair
	Active  hivego.KeyPair
	Posting hivego.KeyPair
} {
	owner := hash256([]byte(key + account + "owner"))
	active := hash256([]byte(key + account + "active"))
	posting := hash256([]byte(key + account + "posting"))

	ownerKey := KeyPairFromBytes(owner)
	activeKey := KeyPairFromBytes(active)
	postingKey := KeyPairFromBytes(posting)

	return struct {
		Owner   hivego.KeyPair
		Active  hivego.KeyPair
		Posting hivego.KeyPair
	}{
		Owner:   *ownerKey,
		Active:  *activeKey,
		Posting: *postingKey,
	}
}

func getHiveOpIds() map[string]uint64 {
	hiveOpsIds := make(map[string]uint64)
	hiveOpsIds["vote_operation"] = 0
	hiveOpsIds["comment_operation"] = 1

	hiveOpsIds["transfer_operation"] = 2
	hiveOpsIds["transfer_to_vesting_operation"] = 3
	hiveOpsIds["withdraw_vesting_operation"] = 4

	hiveOpsIds["limit_order_create_operation"] = 5
	hiveOpsIds["limit_order_cancel_operation"] = 6

	hiveOpsIds["feed_publish_operation"] = 7
	hiveOpsIds["convert_operation"] = 8

	hiveOpsIds["account_create_operation"] = 9
	hiveOpsIds["account_update_operation"] = 10

	hiveOpsIds["witness_update_operation"] = 11
	hiveOpsIds["account_witness_vote_operation"] = 12
	hiveOpsIds["account_witness_proxy_operation"] = 13

	hiveOpsIds["pow_operation"] = 14

	hiveOpsIds["custom_operation"] = 15

	hiveOpsIds["report_over_production_operation"] = 16

	hiveOpsIds["delete_comment_operation"] = 17
	hiveOpsIds["custom_json_operation"] = 18
	hiveOpsIds["comment_options_operation"] = 19
	hiveOpsIds["set_withdraw_vesting_route_operation"] = 20
	hiveOpsIds["limit_order_create2_operation"] = 21
	hiveOpsIds["claim_account_operation"] = 22
	hiveOpsIds["create_claimed_account_operation"] = 23
	hiveOpsIds["request_account_recovery_operation"] = 24
	hiveOpsIds["recover_account_operation"] = 25
	hiveOpsIds["change_recovery_account_operation"] = 26
	hiveOpsIds["escrow_transfer_operation"] = 27
	hiveOpsIds["escrow_dispute_operation"] = 28
	hiveOpsIds["escrow_release_operation"] = 29
	hiveOpsIds["pow2_operation"] = 30
	hiveOpsIds["escrow_approve_operation"] = 31
	hiveOpsIds["transfer_to_savings_operation"] = 32
	hiveOpsIds["transfer_from_savings_operation"] = 33
	hiveOpsIds["cancel_transfer_from_savings_operation"] = 34
	hiveOpsIds["custom_binary_operation"] = 35
	hiveOpsIds["decline_voting_rights_operation"] = 36
	hiveOpsIds["reset_account_operation"] = 37
	hiveOpsIds["set_reset_account_operation"] = 38
	hiveOpsIds["claim_reward_balance_operation"] = 39
	hiveOpsIds["delegate_vesting_shares_operation"] = 40
	hiveOpsIds["account_create_with_delegation_operation"] = 41
	hiveOpsIds["witness_set_properties_operation"] = 42
	hiveOpsIds["account_update2_operation"] = 43
	hiveOpsIds["create_proposal_operation"] = 44
	hiveOpsIds["update_proposal_votes_operation"] = 45
	hiveOpsIds["remove_proposal_operation"] = 46
	hiveOpsIds["update_proposal_operation"] = 47
	hiveOpsIds["collateralized_convert_operation"] = 48
	hiveOpsIds["recurrent_transfer_operation"] = 49

	return hiveOpsIds
}

func getHiveOpId(op string) uint64 {
	op = op + "_operation"
	hiveOpsIds := getHiveOpIds()
	return hiveOpsIds[op]
}
func opIdB(opName string) byte {
	id := getHiveOpId(opName)
	return byte(id)
}

func refBlockNumB(refBlockNumber uint16) []byte {
	buf := make([]byte, 2)
	binary.LittleEndian.PutUint16(buf, refBlockNumber)
	return buf
}

func refBlockPrefixB(refBlockPrefix uint32) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, refBlockPrefix)
	return buf
}

func expTimeB(expTime string) ([]byte, error) {

	exp, err := time.Parse("2006-01-02T15:04:05", expTime)
	if err != nil {
		return nil, err
	}

	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, uint32(exp.Unix()))
	return buf, nil
}

func countOpsB(ops []hivego.HiveOperation) []byte {
	b := make([]byte, 5)
	l := binary.PutUvarint(b, uint64(len(ops)))
	return b[0:l]
}

func extensionsB() byte {
	return byte(0x00)
}

func appendVString(s string, b *bytes.Buffer) *bytes.Buffer {
	vBuf := make([]byte, 5)
	vLen := binary.PutUvarint(vBuf, uint64(len(s)))
	b.Write(vBuf[0:vLen])

	b.WriteString(s)
	return b
}

func appendVStringArray(a []string, b *bytes.Buffer) *bytes.Buffer {
	b.Write([]byte{byte(len(a))})
	for _, s := range a {
		appendVString(s, b)
	}
	return b
}

func appendVAsset(asset string, b *bytes.Buffer) error {
	parts := strings.Split(asset, " ")
	if len(parts) != 2 {
		return errors.New("invalid asset format: " + asset)
	}

	amountStr, symbol := parts[0], parts[1]

	// all tokens have precision 3 except for VESTS
	precision := 3

	if symbol == "VESTS" {
		precision = 6
	}

	// convert to their old names for compatibility
	switch symbol {
	case "HIVE":
		symbol = "STEEM"
	case "HBD":
		symbol = "SBD"
	}

	// convert to float and multiply by 10^precision
	amount, err := strconv.ParseFloat(amountStr, 64)

	if err != nil {
		return err
	}

	amount = amount * math.Pow10(precision)

	// write the amount as int64
	err = binary.Write(b, binary.LittleEndian, int64(amount))

	if err != nil {
		return err
	}

	// write the precision
	b.WriteByte(byte(precision))

	// write the symbol NUL padded to 8 bits
	for i := 0; i < 7; i++ {
		if i < len(symbol) {
			b.WriteByte(symbol[i])
		} else {
			b.WriteByte(byte(0))
		}
	}

	return nil
}

func serializeOps(ops []hivego.HiveOperation) ([]byte, error) {
	var opsBuf bytes.Buffer
	opsBuf.Write(countOpsB(ops))
	for _, op := range ops {
		b, err := op.SerializeOp()
		if err != nil {
			return nil, err
		}
		opsBuf.Write(b)
	}
	return opsBuf.Bytes(), nil
}

type HiveTransaction struct {
	RefBlockNum    uint16                 `json:"ref_block_num"`
	RefBlockPrefix uint32                 `json:"ref_block_prefix"`
	Expiration     string                 `json:"expiration"`
	Operations     []hivego.HiveOperation `json:"-"`
	OperationsJs   [][2]interface{}       `json:"operations"`
	Extensions     []string               `json:"extensions"`
	Signatures     []string               `json:"signatures"`
}

func SerializeTx(tx HiveTransaction) ([]byte, error) {
	var buf bytes.Buffer
	buf.Write(refBlockNumB(tx.RefBlockNum))
	buf.Write(refBlockPrefixB(tx.RefBlockPrefix))
	expTime, err := expTimeB(tx.Expiration)
	if err != nil {
		return nil, err
	}
	buf.Write(expTime)

	opsB, err := serializeOps(tx.Operations)
	if err != nil {
		return nil, err
	}
	buf.Write(opsB)
	buf.Write([]byte{extensionsB()})
	return buf.Bytes(), nil
}

func TestMultisigKeys(t *testing.T) {

	// keys := hashKey(MS_TEST_KEY, "vsc.gateway")

	// val := hivego.Transaction{
	// 	Operations: []hivego.Operation{
	// 		{
	// 			Type: "custom_json",
	// 			Value: map[string]interface{}{
	// 				"id":                     "vsc.bridge",
	// 				"required_auths":         []string{"vsc.go-testnet"},
	// 				"required_posting_auths": []string{},
	// 				"json":                   `{"type":"ref_bridge"}`,
	// 			},
	// 		},
	// 	},
	// }

	client := hivego.NewHiveRpc("https://api.hive.blog")

	accounts, _ := client.GetAccount([]string{"vsc.go-testnet"})
	account := accounts[0]

	fmt.Println("account.Owner", account.Owner.KeyAuths)
	for _, key := range account.Owner.KeyAuths {
		fmt.Println(key[0])
	}

	kp, _ := hivego.KeyPairFromWif("5JpboCuFbypjdBzSgWwWm3ZjGpQoPbSc7bcoXYg26TWQZ7uwFRM")

	fmt.Println("TO SIGN KEY", *hivego.GetPublicKeyString(kp.PublicKey))
	hiveCreator := hive.LiveTransactionCreator{
		TransactionCrafter: hive.TransactionCrafter{},
		TransactionBroadcaster: hive.TransactionBroadcaster{
			Client:  client,
			KeyPair: kp,
		},
	}

	keyAuths := [][2]interface{}{}

	privKeys := []hivego.KeyPair{}
	for x := 0; x < 3; x++ {
		keys := hashKey(MS_TEST_KEY, "vsc.gateway#"+strconv.Itoa(x))
		stmPub := hivego.GetPublicKeyString(keys.Owner.PublicKey)
		fmt.Println("keys.Owner.PublicKey", *stmPub)
		keyAuths = append(keyAuths, [2]interface{}{*stmPub, 1})
		privKeys = append(privKeys, keys.Owner)
	}

	masterKey := "STM4vYpxdpfinTbBY8ywhRx6tLr9BZ34w9zLAzttJvMjspEVLsLz7"
	keyAuths = append(keyAuths, [2]interface{}{masterKey, 3})

	fmt.Println("keyAuths", keyAuths)

	op := hiveCreator.UpdateAccount("vsc.go-testnet", &hivego.Auths{
		WeightThreshold: 3,
		KeyAuths:        keyAuths,
		AccountAuths:    [][2]interface{}{},
	}, nil, nil, "", account.MemoKey)

	tx := hiveCreator.MakeTransaction([]hivego.HiveOperation{op})

	hiveCreator.PopulateSigningProps(&tx, nil)

	for _, key := range privKeys {
		hiveCreator.KeyPair = &key
		sig, _ := hiveCreator.Sign(tx)
		tx.AddSig(sig)

	}

	id, err := hiveCreator.Broadcast(tx)

	fmt.Println("id, err", id, err)

	// c := hivego.NewHiveRpc("https://api.hive.blog")
	// b := HiveTransaction{
	// 	RefBlockNum:    0,
	// 	RefBlockPrefix: 0,
	// 	Expiration:     time.Now().Add(time.Hour).Format("2006-01-02T15:04:05"),
	// }
	// txBytes, _ := SerializeTx(b)
	// fmt.Println("Bytes", txBytes)
	// txHash := hivego.HashTxForSig(txBytes)
	// sig, _ := keys.Active.PrivateKey.Sign(txHash)
	// sig.Serialize()

}
