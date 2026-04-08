package params

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// A transaction consuming 1000 RC (1 HBD equivalent) would generate ~0.002 HBD interest for the protocol
// At 100K gas/RC and 100 RC minimum cost it would take at least 10M gas for a tx to consume more
const CYCLE_GAS_PER_RC = 100_000

// Areweave does $10.5 per GB we can use less b/c we charge for reads and modifications as well
// 19 RCs per new written byte ($4/GB)
// 1 RC per read or modified byte ($0.21/GB)
const WRITE_IO_GAS_RC_COST = 19
const READ_IO_GAS_RC_COST = 1

const EPHEM_IO_GAS = 100

// 2,000 HIVE
var CONSENSUS_MINIMUM = int64(2_000_000)

var MAINNET_ID = "vsc-mainnet"

var GATEWAY_WALLET = "vsc.gateway"

var FR_VIRTUAL_ACCOUNT = "system:fr_balance"

var DAO_WALLET = "hive:vsc.dao"

var RC_RETURN_PERIOD uint64 = 120 * 60 * 20 // 5 day cool down period for RCs
var RC_HIVE_FREE_AMOUNT int64 = 5_000       // 5 HBD worth of RCs for Hive accounts
var MINIMUM_RC_LIMIT uint64 = 50

var CONTRACT_DEPLOYMENT_FEE int64 = 10_000 // 10 HBD per contract
var CONTRACT_DEPLOYMENT_FEE_START_HEIGHT uint64 = 99410000
var CONTRACT_UPDATE_HEIGHT uint64 = 102100000
var CONTRACT_CALL_MAX_RECURSION_DEPTH = 20

// Mainnet TSS key indexing
var TSS_INDEX_HEIGHT uint64 = 102_083_000

// Election once every 6 hours on mainnet
var ELECTION_INTERVAL = uint64(6 * 60 * 20)

type ConsensusParams struct {
	MinStake         int64  `json:"minStake,omitempty"`
	MinMembers       int    `json:"minMembers,omitempty"`
	MinSpSigners     int    `json:"minSpSigners,omitempty"`
	MinRcLimit       uint64 `json:"minRcLimit,omitempty"`
	TssIndexHeight   uint64 `json:"tssIndexHeight,omitempty"`
	ElectionInterval uint64 `json:"electionInterval,omitempty"`
}

type TssParams struct {
	ReshareSyncDelay      time.Duration `json:"reshareSyncDelay,omitempty"`
	ReshareTimeout        time.Duration `json:"reshareTimeout,omitempty"`
	DefaultTimeout        time.Duration `json:"defaultTimeout,omitempty"`
	MessageRetryDelay     time.Duration `json:"messageRetryDelay,omitempty"`
	BufferedMessageMaxAge time.Duration `json:"bufferedMessageMaxAge,omitempty"`
	RpcTimeout            time.Duration `json:"rpcTimeout,omitempty"`
	CommitDelay           time.Duration `json:"commitDelay,omitempty"`
	WaitForSigsTimeout    time.Duration `json:"waitForSigsTimeout,omitempty"`
	RotateInterval        uint64        `json:"rotateInterval,omitempty"`
}

// MarshalJSON serializes TssParams with durations as human-readable
// strings (e.g. "5s", "2m") using the json tags on TssParams itself.
// Non-duration fields (like RotateInterval) are serialized as-is.
func (t TssParams) MarshalJSON() ([]byte, error) {
	m := make(map[string]interface{})
	v := reflect.ValueOf(t)
	rt := v.Type()
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		key, _, _ := strings.Cut(tag, ",")
		field := v.Field(i)
		if field.Type() == reflect.TypeOf(time.Duration(0)) {
			dur := field.Interface().(time.Duration)
			if dur != 0 {
				m[key] = dur.String()
			}
		} else {
			if !field.IsZero() {
				m[key] = field.Interface()
			}
		}
	}
	return json.Marshal(m)
}

// UnmarshalJSON deserializes TssParams from a JSON object where
// durations are human-readable strings. Non-duration fields are
// parsed from their JSON number representation. Only fields present
// in the JSON are overwritten.
func (t *TssParams) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	v := reflect.ValueOf(t).Elem()
	rt := v.Type()
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		key, _, _ := strings.Cut(tag, ",")
		raw, ok := m[key]
		if !ok {
			continue
		}
		field := v.Field(i)
		if field.Type() == reflect.TypeOf(time.Duration(0)) {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("field %s: expected duration string: %w", key, err)
			}
			d, err := time.ParseDuration(s)
			if err != nil {
				return fmt.Errorf("field %s: %w", key, err)
			}
			field.Set(reflect.ValueOf(d))
		} else {
			if err := json.Unmarshal(raw, field.Addr().Interface()); err != nil {
				return fmt.Errorf("field %s: %w", key, err)
			}
		}
	}
	return nil
}

var DefaultTssParams = TssParams{
	ReshareSyncDelay:      5 * time.Second,
	ReshareTimeout:        2 * time.Minute,
	DefaultTimeout:        1 * time.Minute,
	MessageRetryDelay:     1 * time.Second,
	BufferedMessageMaxAge: 1 * time.Minute,
	RpcTimeout:            30 * time.Second,
	CommitDelay:           5 * time.Second,
	WaitForSigsTimeout:    6 * time.Second,
}

var MocknetTssParams = TssParams{
	ReshareSyncDelay:      1 * time.Second,
	ReshareTimeout:        2 * time.Minute,
	DefaultTimeout:        1 * time.Minute,
	MessageRetryDelay:     500 * time.Millisecond,
	BufferedMessageMaxAge: 30 * time.Second,
	RpcTimeout:            10 * time.Second,
	CommitDelay:           1 * time.Second,
	WaitForSigsTimeout:    6 * time.Second,
}

type OracleParams struct {
	// ChainContracts maps chain symbols (e.g. "BTC") to their
	// relay mapping contract IDs.
	ChainContracts map[string]string `json:"chainContracts,omitempty"`

	// Deprecated: use ChainContracts["BTC"] instead.
	BtcContractId string `json:"btcContractId,omitempty"`
}

// ContractId returns the relay contract ID for the given chain symbol.
// Falls back to the legacy BtcContractId field for BTC.
func (o OracleParams) ContractId(symbol string) string {
	if o.ChainContracts != nil {
		if id, ok := o.ChainContracts[symbol]; ok {
			return id
		}
	}
	if symbol == "BTC" {
		return o.BtcContractId
	}
	return ""
}
