package mapper

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/lib/btcvault"

	"github.com/hasura/go-graphql-client"
)

// fetchStateHex reads raw contract state keys and returns the decoded bytes for
// each key that exists. Missing keys are simply absent from the result.
func (b *Bot) fetchStateHex(ctx context.Context, keys []string) (map[string][]byte, error) {
	if len(keys) == 0 {
		return map[string][]byte{}, nil
	}
	vars := map[string]interface{}{
		"contractId": b.BotConfig.ContractId(),
		"keys":       keys,
		"encoding":   "hex",
	}

	var result json.RawMessage
	err := b.gqlClientDo(ctx, func(client *graphql.Client) error {
		var q GetContractStateQuery
		if err := client.Query(ctx, &q, vars, graphql.OperationName("GetContractState")); err != nil {
			return err
		}
		result = q.GetStateByKeys
		return nil
	})
	if err != nil {
		return nil, err
	}

	var stateMap map[string]json.RawMessage
	if err := json.Unmarshal(result, &stateMap); err != nil {
		return nil, err
	}

	out := make(map[string][]byte, len(stateMap))
	for k, raw := range stateMap {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			continue // key absent (null) or empty
		}
		decoded, err := hex.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("decoding state key %s: %w", k, err)
		}
		out[k] = decoded
	}
	return out, nil
}

// FetchVaultRegistry reads the vault-generation registry ("v"). An absent or
// empty registry is NOT an error: a non-vault mapping contract (dash/ltc/...) and
// a pre-rotation BTC deploy both legitimately have none, and the rotation driver
// must no-op on them rather than log-spam.
func (b *Bot) FetchVaultRegistry(ctx context.Context) ([]btcvault.Vault, error) {
	st, err := b.fetchStateHex(ctx, []string{contractinterface.VaultRegistryKey})
	if err != nil {
		return nil, err
	}
	raw, ok := st[contractinterface.VaultRegistryKey]
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	vaults, err := btcvault.UnmarshalVaultRegistry(raw)
	if err != nil {
		// Fail CLOSED, exactly like the node's signing gate: a registry we cannot
		// decode must never be treated as "no superseded generation" (that would
		// silently skip the drain).
		return nil, fmt.Errorf("undecodable vault registry: %w", err)
	}
	return vaults, nil
}

// FetchGenUtxoCounts returns, per generation, how many UTXOs the contract's
// registry still attributes to it. A generation with zero UTXOs is drained —
// this is the same predicate the contract's own fund-gate uses to allow
// retireVault to advance a generation (fundedGenerations()).
func (b *Bot) FetchGenUtxoCounts(ctx context.Context) (map[uint32]int, error) {
	st, err := b.fetchStateHex(ctx, []string{contractinterface.UtxoRegistryKey})
	if err != nil {
		return nil, err
	}
	reg := st[contractinterface.UtxoRegistryKey]
	if len(reg) == 0 {
		return map[uint32]int{}, nil
	}

	// The registry is packed 8-byte entries: id (uint16 BE) + amount (6 bytes BE).
	const entrySize = 8
	if len(reg)%entrySize != 0 {
		return nil, fmt.Errorf("utxo registry length %d is not a multiple of %d", len(reg), entrySize)
	}
	keys := make([]string, 0, len(reg)/entrySize)
	for off := 0; off+entrySize <= len(reg); off += entrySize {
		id := uint32(reg[off])<<8 | uint32(reg[off+1])
		keys = append(keys, contractinterface.UtxoPrefix+fmt.Sprintf("%x", id))
	}

	// One batched read for every UTXO object — not one query per UTXO.
	objs, err := b.fetchStateHex(ctx, keys)
	if err != nil {
		return nil, err
	}

	counts := make(map[uint32]int)
	for _, k := range keys {
		raw, ok := objs[k]
		if !ok || len(raw) < 4 {
			continue
		}
		// The generation is the trailing uint32 (big-endian) of the UTXO record.
		gen := uint32(raw[len(raw)-4])<<24 | uint32(raw[len(raw)-3])<<16 |
			uint32(raw[len(raw)-2])<<8 | uint32(raw[len(raw)-1])
		counts[gen]++
	}
	return counts, nil
}

// InFlightSweepTxIds returns the pending-spend txids that are migration sweeps
// (an "ms-<txid>" record is still present, i.e. not yet settled). The driver
// serialises on a non-empty result: building a second tranche while one is
// unconfirmed would draw down the fee reserve twice and race the same generation.
// It also drives the stuck-sweep re-drive off this set.
func (b *Bot) InFlightSweepTxIds(ctx context.Context, pendingTxIds []string) ([]string, error) {
	if len(pendingTxIds) == 0 {
		return nil, nil
	}
	keys := make([]string, len(pendingTxIds))
	keyToTx := make(map[string]string, len(pendingTxIds))
	for i, txId := range pendingTxIds {
		keys[i] = contractinterface.MigrationSweepPrefix + txId
		keyToTx[keys[i]] = txId
	}
	st, err := b.fetchStateHex(ctx, keys)
	if err != nil {
		return nil, err
	}
	sweeps := make([]string, 0, len(st))
	for k := range st {
		sweeps = append(sweeps, keyToTx[k])
	}
	return sweeps, nil
}

// FetchContractHeight reads the contract's committed last BTC height ("h"). The
// stuck-sweep re-drive clock is measured against this, mirroring the contract's
// own staleness gate (which compares its LastHeight to the sweep's BuildHeight).
func (b *Bot) FetchContractHeight(ctx context.Context) (uint64, error) {
	s, err := b.gql().FetchLastHeight(ctx)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(s, 10, 64)
}
