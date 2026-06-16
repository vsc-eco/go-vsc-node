package sdk

import (
	"encoding/hex"
	"strconv"
)

// Aborts the contract execution
func Abort(msg string) {
	ln := int32(0)
	abort(&msg, nil, &ln, &ln)
	panic(msg)
}

// Reverts the transaction and abort execution in the same way as Abort().
func Revert(msg string, symbol string) {
	revert(&msg, &symbol)
}

// Set a value by key in the contract state
func StateSetObject(key string, value string) {
	stateSetObject(&key, &value)
}

// Get a value by key from the contract state
func StateGetObject(key string) *string {
	return stateGetObject(&key)
}

// Delete or unset a value by key in the contract state
func StateDeleteObject(key string) {
	stateDeleteObject(&key)
}

// Set a value by key in the ephemeral contract state
func EphemStateSetObject(key string, value string) {
	ephemStateSetObject(&key, &value)
}

// Get a value by key from the ephemeral contract state
func EphemStateGetObject(contractId string, key string) *string {
	return ephemStateGetObject(&contractId, &key)
}

// Delete or unset a value by key in the ephemeral contract state
func EphemStateDeleteObject(key string) {
	ephemStateDeleteObject(&key)
}

// Get current execution environment variables as json string
func GetEnvStr() string {
	return *getEnv(nil)
}

// Get current execution environment variable by a key
func GetEnvKey(key string) *string {
	return getEnvKey(&key)
}

// Get a value by key from the contract state of another contract
func ContractStateGet(contractId string, key string) *string {
	return contractRead(&contractId, &key)
}

// TryContractCall calls another contract in try/catch mode. If the callee
// reverts, the caller is NOT trapped: the returned *string is a JSON outcome
// {"ok":false,...} and the callee's state/ledger writes are rolled back. On
// success it is {"ok":true,"result":<callee return>}. RC and gas are charged
// either way.
func TryContractCall(contractId string, method string, payload string) *string {
	opts := `{"try":true}`
	return contractCall(&contractId, &method, &payload, &opts)
}

func TssCreateKey(keyId string, algo string, epochs uint64) string {
	if algo != "ecdsa" && algo != "eddsa" {
		Abort("algo must be ecdsa or eddsa")
	}
	epochsStr := strconv.FormatUint(epochs, 10)
	return *tssCreateKey(&keyId, &algo, &epochsStr)
}

// TssRenewKey extends the expiry of a key owned by this contract by additionalEpochs.
// The key must be active or deprecated. Returns "renewed" on success.
func TssRenewKey(keyId string, additionalEpochs uint64) string {
	epochsStr := strconv.FormatUint(additionalEpochs, 10)
	return *tssRenewKey(&keyId, &epochsStr)
}

func TssGetKey(keyId string) string {
	return *tssGetKey(&keyId)
}

func TssSignKey(keyId string, bytes []byte) {
	byteStr := hex.EncodeToString(bytes)

	tssSignKey(&keyId, &byteStr)
}

// PendulumApplySwapFees invokes system.pendulum_apply_swap_fees with the
// JSON input {asset_in,asset_out,x,x_reserve,y_reserve} and returns the
// JSON output {user_output,...,lp_share_output,node_share_output,...}.
// The host applies the whitelist check, the fee/stabilizer math, the
// node-vs-LP split (with the LP minimum-floor when active), and the
// node-bucket accrual; this is a thin marshalling wrapper.
func PendulumApplySwapFees(inputJSON string) string {
	return *pendulumApplySwapFees(&inputJSON)
}

// HiveDraw pulls `amount` (base units, decimal string) of `asset` from the
// calling transaction's transfer.allow intents into this contract's balance.
func HiveDraw(amount string, asset string) {
	hiveDraw(&amount, &asset)
}

// HiveGetBalance returns this/any account's balance for an asset as a
// decimal string of base units.
func HiveGetBalance(account string, asset string) string {
	return *getBalance(&account, &asset)
}
