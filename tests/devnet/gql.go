package devnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// gql.go — a minimal GraphQL client for the devnet harness. The harness
// otherwise only exposes GQLEndpoint(node) as a string; the regression test
// needs to read ledger balances, tx status, contract state, elections and node
// sync status to assert cross-node consistency. Custom scalars Uint64/Int64
// marshal as bare JSON numbers (model/Uint64.go), so they decode straight into
// Go uint64/int64.

// gqlQuery POSTs a GraphQL query to a node and decodes the "data" field into out.
func (d *Devnet) gqlQuery(ctx context.Context, node int, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("marshaling gql request: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, d.GQLEndpoint(node),
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("gql POST to magi-%d: %w", node, err)
	}
	defer resp.Body.Close()

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decoding gql response from magi-%d: %w", node, err)
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, len(envelope.Errors))
		for i, e := range envelope.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("gql errors from magi-%d: %s", node, strings.Join(msgs, "; "))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("unmarshaling gql data from magi-%d: %w", node, err)
	}
	return nil
}

// BalanceRecord is the subset of the GQL BalanceRecord we assert on.
type BalanceRecord struct {
	Hive               int64  `json:"hive"`
	Hbd                int64  `json:"hbd"`
	HbdSavings         int64  `json:"hbd_savings"`
	HiveConsensus      int64  `json:"hive_consensus"`
	ConsensusUnstaking int64  `json:"consensus_unstaking"`
	BlockHeight        uint64 `json:"block_height"`
}

// GetAccountBalance reads getAccountBalance for an account ("hive:name") from a node.
func (d *Devnet) GetAccountBalance(ctx context.Context, node int, account string) (*BalanceRecord, error) {
	const q = `query($a:String!){getAccountBalance(account:$a){hive hbd hbd_savings hive_consensus consensus_unstaking block_height}}`
	var out struct {
		GetAccountBalance *BalanceRecord `json:"getAccountBalance"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"a": account}, &out); err != nil {
		return nil, err
	}
	if out.GetAccountBalance == nil {
		return &BalanceRecord{}, nil
	}
	return out.GetAccountBalance, nil
}

// FindTransactionStatus returns the status string for a tx id (e.g. PROCESSED,
// CONFIRMED, FAILED), or "" if the tx is not yet indexed.
func (d *Devnet) FindTransactionStatus(ctx context.Context, node int, txId string) (string, error) {
	const q = `query($id:String!){findTransaction(filterOptions:{byId:$id}){id status}}`
	var out struct {
		FindTransaction []struct {
			Id     string `json:"id"`
			Status string `json:"status"`
		} `json:"findTransaction"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"id": txId}, &out); err != nil {
		return "", err
	}
	if len(out.FindTransaction) == 0 {
		return "", nil
	}
	return out.FindTransaction[0].Status, nil
}

// ContractCallResult is the per-call outcome row inside a ContractOutput.
type ContractCallResult struct {
	Ret string `json:"ret"`
	Ok  bool   `json:"ok"`
}

// ContractOutputRecord is the full shape of a ContractOutput row from the
// L2 GQL. inputs lists the txids batched into this output (one per contract
// call in the block); results[i] is the per-call outcome for inputs[i].
type ContractOutputRecord struct {
	Id          string               `json:"id"`
	BlockHeight int64                `json:"block_height"`
	ContractId  string               `json:"contract_id"`
	Inputs      []string             `json:"inputs"`
	Results     []ContractCallResult `json:"results"`
}

// FindContractOutputByInput returns the contract execution output(s) triggered
// by the given input transaction id. The byInput filter matches outputs
// whose inputs[] array contains txId — i.e. the L2 block this tx was batched
// into. `ok=false` means the contract call aborted (e.g. ABORT:ErrNoPermission);
// `ret` carries the abort/return string. Used by devnet test diagnostics to
// triage why a contract call's state write didn't appear.
//
// Returns the full ContractOutputRecord list — caller can correlate
// inputs[i] with results[i] to identify which call in a batched output
// succeeded vs aborted.
func (d *Devnet) FindContractOutputByInput(ctx context.Context, node int, txId string) ([]ContractOutputRecord, error) {
	const q = `query($id:String!){findContractOutput(filterOptions:{byInput:$id}){id block_height contract_id inputs results{ret ok}}}`
	var out struct {
		FindContractOutput []ContractOutputRecord `json:"findContractOutput"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"id": txId}, &out); err != nil {
		return nil, err
	}
	return out.FindContractOutput, nil
}

// GetStateByKeys reads contract state values for the given keys (UTF-8 decoded).
func (d *Devnet) GetStateByKeys(ctx context.Context, node int, contractId string, keys []string) (map[string]any, error) {
	const q = `query($c:String!,$k:[String!]!){getStateByKeys(contractId:$c,keys:$k)}`
	var out struct {
		GetStateByKeys map[string]any `json:"getStateByKeys"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"c": contractId, "k": keys}, &out); err != nil {
		return nil, err
	}
	return out.GetStateByKeys, nil
}

// GetStateByKeysHex reads contract state values for the given keys, hex-
// encoded so non-UTF8 binary values (e.g. the packed-uint64 internal
// balance bytes — mapping/forwarder_integration.go:489) round-trip through
// JSON cleanly. Used by devnet diagnostics where the plain getStateByKeys
// returns nil for binary state values even when they're set.
func (d *Devnet) GetStateByKeysHex(ctx context.Context, node int, contractId string, keys []string) (map[string]any, error) {
	const q = `query($c:String!,$k:[String!]!){getStateByKeys(contractId:$c,keys:$k,encoding:"hex")}`
	var out struct {
		GetStateByKeys map[string]any `json:"getStateByKeys"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"c": contractId, "k": keys}, &out); err != nil {
		return nil, err
	}
	return out.GetStateByKeys, nil
}

// ElectionInfo is the subset of ElectionResult we assert on.
type ElectionInfo struct {
	Epoch       uint64
	Members     []string
	Weights     []uint64
	TotalWeight uint64
	BlockHeight uint64
}

// GetElectionGQL reads getElection(epoch) and returns the ordered member accounts.
func (d *Devnet) GetElectionGQL(ctx context.Context, node int, epoch uint64) (*ElectionInfo, error) {
	const q = `query($e:Uint64!){getElection(epoch:$e){epoch members{account} weights total_weight block_height}}`
	var out struct {
		GetElection *struct {
			Epoch   uint64 `json:"epoch"`
			Members []struct {
				Account string `json:"account"`
			} `json:"members"`
			Weights     []uint64 `json:"weights"`
			TotalWeight uint64   `json:"total_weight"`
			BlockHeight uint64   `json:"block_height"`
		} `json:"getElection"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"e": epoch}, &out); err != nil {
		return nil, err
	}
	if out.GetElection == nil {
		return nil, nil
	}
	info := &ElectionInfo{
		Epoch:       out.GetElection.Epoch,
		Weights:     out.GetElection.Weights,
		TotalWeight: out.GetElection.TotalWeight,
		BlockHeight: out.GetElection.BlockHeight,
	}
	for _, m := range out.GetElection.Members {
		info.Members = append(info.Members, m.Account)
	}
	return info, nil
}

// LocalNodeInfo returns a node's last processed block and current epoch.
func (d *Devnet) LocalNodeInfo(ctx context.Context, node int) (lastBlock, epoch uint64, err error) {
	const q = `query{localNodeInfo{last_processed_block epoch}}`
	var out struct {
		LocalNodeInfo *struct {
			LastProcessedBlock uint64 `json:"last_processed_block"`
			Epoch              uint64 `json:"epoch"`
		} `json:"localNodeInfo"`
	}
	if err = d.gqlQuery(ctx, node, q, nil, &out); err != nil {
		return 0, 0, err
	}
	if out.LocalNodeInfo == nil {
		return 0, 0, fmt.Errorf("magi-%d returned nil localNodeInfo", node)
	}
	return out.LocalNodeInfo.LastProcessedBlock, out.LocalNodeInfo.Epoch, nil
}

// TssRequestRecord is a TSS signing request as seen via GQL.
type TssRequestRecord struct {
	KeyId  string `json:"key_id"`
	Status string `json:"status"`
	Msg    string `json:"msg"`
	Sig    string `json:"sig"`
}

// GetTssRequests reads signing requests for a key id.
func (d *Devnet) GetTssRequests(ctx context.Context, node int, keyId string) ([]TssRequestRecord, error) {
	const q = `query($k:String!){getTssRequests(keyId:$k){key_id status msg sig}}`
	var out struct {
		GetTssRequests []TssRequestRecord `json:"getTssRequests"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"k": keyId}, &out); err != nil {
		return nil, err
	}
	return out.GetTssRequests, nil
}
