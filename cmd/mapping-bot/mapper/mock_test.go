package mapper

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"vsc-node/cmd/mapping-bot/chain"
	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"
)

// ---------------------------------------------------------------------------
// mockGraphQL — satisfies GraphQLFetcher
// ---------------------------------------------------------------------------

type mockGraphQL struct {
	mu sync.Mutex

	txSpends   map[string]*contractinterface.SigningData
	signatures map[string]database.SignatureUpdate
	lastHeight string
	primaryKey []byte
	backupKey  []byte
	observedTx map[string]bool   // key: "txid:vout"
	txStatuses map[string]string // key: tx ID, value: status

	// Track which methods were called and with what args.
	calls []mockGQLCall
}

type mockGQLCall struct {
	Method string
	Args   []interface{}
}

func (m *mockGraphQL) recordCall(method string, args ...interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, mockGQLCall{Method: method, Args: args})
}

func (m *mockGraphQL) FetchTxSpends(ctx context.Context) (map[string]*contractinterface.SigningData, error) {
	m.recordCall("FetchTxSpends")
	if m.txSpends == nil {
		return make(map[string]*contractinterface.SigningData), nil
	}
	return m.txSpends, nil
}

func (m *mockGraphQL) FetchSignatures(ctx context.Context, msgHex []string) (map[string]database.SignatureUpdate, error) {
	m.recordCall("FetchSignatures", msgHex)
	if m.signatures == nil {
		return make(map[string]database.SignatureUpdate), nil
	}
	return m.signatures, nil
}

func (m *mockGraphQL) FetchLastHeight(ctx context.Context) (string, error) {
	m.recordCall("FetchLastHeight")
	return m.lastHeight, nil
}

func (m *mockGraphQL) FetchPublicKeys(ctx context.Context) ([]byte, []byte, error) {
	m.recordCall("FetchPublicKeys")
	return m.primaryKey, m.backupKey, nil
}

func (m *mockGraphQL) FetchObservedTx(ctx context.Context, txId string, vout int) (bool, error) {
	m.recordCall("FetchObservedTx", txId, vout)
	if m.observedTx == nil {
		return false, nil
	}
	key := txId + ":" + string(rune('0'+vout))
	return m.observedTx[key], nil
}

func (m *mockGraphQL) FetchTransactionStatus(ctx context.Context, txId string) (string, error) {
	m.recordCall("FetchTransactionStatus", txId)
	if m.txStatuses == nil {
		return "", fmt.Errorf("transaction %s not found", txId)
	}
	if status, ok := m.txStatuses[txId]; ok {
		return status, nil
	}
	return "", fmt.Errorf("transaction %s not found", txId)
}

// ---------------------------------------------------------------------------
// mockContractCaller — satisfies ContractCaller
// ---------------------------------------------------------------------------

type contractCall struct {
	Action  string
	Payload json.RawMessage
}

type mockContractCaller struct {
	mu    sync.Mutex
	calls []contractCall
	err   error // optional error to return
}

func (m *mockContractCaller) CallContract(ctx context.Context, contractInput json.RawMessage, action string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, contractCall{Action: action, Payload: contractInput})
	return "mock-tx-id", m.err
}

func (m *mockContractCaller) getCalls() []contractCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]contractCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// ---------------------------------------------------------------------------
// mockStateStore — in-memory satisfying StateStore
// ---------------------------------------------------------------------------

type mockStateStore struct {
	mu          sync.Mutex
	blockHeight uint64
	txs         map[string]*database.Transaction
}

func newMockStateStore() *mockStateStore {
	return &mockStateStore{
		txs: make(map[string]*database.Transaction),
	}
}

func (m *mockStateStore) IncrementBlockHeight(ctx context.Context) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blockHeight++
	return m.blockHeight, nil
}

func (m *mockStateStore) GetBlockHeight(ctx context.Context) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.blockHeight, nil
}

func (m *mockStateStore) SetBlockHeight(ctx context.Context, height uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blockHeight = height
	return nil
}

func (m *mockStateStore) AddPendingTransaction(ctx context.Context, txID string, rawTx []byte, unsignedHashes []contractinterface.UnsignedSigHash) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.txs[txID]; exists {
		return database.ErrTxExists
	}
	sigs := make([]database.SignatureSlot, len(unsignedHashes))
	for i, uh := range unsignedHashes {
		sigs[i] = database.SignatureSlot{
			Index:         uint64(uh.Index),
			SigHash:       uh.SigHash,
			WitnessScript: uh.WitnessScript,
		}
	}
	m.txs[txID] = &database.Transaction{
		TxID:            txID,
		State:           database.TxStatePending,
		RawTx:           rawTx,
		TotalSignatures: uint64(len(unsignedHashes)),
		Signatures:      sigs,
		CreatedAt:       time.Now().UTC(),
	}
	return nil
}

func (m *mockStateStore) GetPendingTransaction(ctx context.Context, txID string) (*database.Transaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.txs[txID]
	if !ok || tx.State != database.TxStatePending {
		return nil, database.ErrTxNotFound
	}
	return tx, nil
}

func (m *mockStateStore) GetAllPendingTransactions(ctx context.Context) ([]database.Transaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []database.Transaction
	for _, tx := range m.txs {
		if tx.State == database.TxStatePending {
			out = append(out, *tx)
		}
	}
	return out, nil
}

func (m *mockStateStore) GetAllPendingSigHashes(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var hashes []string
	for _, tx := range m.txs {
		if tx.State != database.TxStatePending {
			continue
		}
		for _, sig := range tx.Signatures {
			if sig.Signature == nil {
				hashes = append(hashes, string(sig.SigHash))
			}
		}
	}
	return hashes, nil
}

func (m *mockStateStore) UpdateSignatures(ctx context.Context, signatures map[string]database.SignatureUpdate) ([]*database.Transaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var fullySigned []*database.Transaction
	for sigHash, update := range signatures {
		for _, tx := range m.txs {
			if tx.State != database.TxStatePending {
				continue
			}
			for i := range tx.Signatures {
				if string(tx.Signatures[i].SigHash) == sigHash && tx.Signatures[i].Signature == nil {
					tx.Signatures[i].Signature = update.Bytes
					tx.Signatures[i].IsBackup = update.IsBackup
					tx.CurrentSignatures++
					if tx.CurrentSignatures >= tx.TotalSignatures {
						fullySigned = append(fullySigned, tx)
					}
					break
				}
			}
		}
	}
	return fullySigned, nil
}

func (m *mockStateStore) IsTransactionProcessed(ctx context.Context, txID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.txs[txID]
	if !ok {
		return false, nil
	}
	return tx.State == database.TxStateSent || tx.State == database.TxStateConfirmed, nil
}

func (m *mockStateStore) MarkTransactionSent(ctx context.Context, txID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.txs[txID]
	if !ok || tx.State != database.TxStatePending {
		return database.ErrTxNotFound
	}
	tx.State = database.TxStateSent
	now := time.Now().UTC()
	tx.SentAt = &now
	return nil
}

func (m *mockStateStore) MarkTransactionConfirmed(ctx context.Context, txID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.txs[txID]
	if !ok || tx.State != database.TxStateSent {
		return database.ErrTxNotFound
	}
	tx.State = database.TxStateConfirmed
	now := time.Now().UTC()
	tx.ConfirmedAt = &now
	return nil
}

func (m *mockStateStore) GetSentTransactionIDs(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, tx := range m.txs {
		if tx.State == database.TxStateSent {
			ids = append(ids, tx.TxID)
		}
	}
	return ids, nil
}

// ---------------------------------------------------------------------------
// mockAddressStore — in-memory satisfying AddressStore
// ---------------------------------------------------------------------------

type mockAddressStore struct {
	mu           sync.Mutex
	instructions map[string]string // chainAddr -> instruction
}

func newMockAddressStore() *mockAddressStore {
	return &mockAddressStore{
		instructions: make(map[string]string),
	}
}

func (m *mockAddressStore) GetInstruction(ctx context.Context, chainAddr string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.instructions[chainAddr]
	if !ok {
		return "", database.ErrAddrNotFound
	}
	return inst, nil
}

func (m *mockAddressStore) Insert(ctx context.Context, chainAddr, instruction string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.instructions[chainAddr]; exists {
		return database.ErrAddrExists
	}
	m.instructions[chainAddr] = instruction
	return nil
}

// ---------------------------------------------------------------------------
// mockChainClient — satisfies chain.BlockchainClient
// ---------------------------------------------------------------------------

type mockChainClient struct {
	mu            sync.Mutex
	posted        []string
	tipHeight     uint64
	blockHashes   map[uint64]string
	rawBlocks     map[string][]byte
	txStatuses    map[string]bool // txid -> confirmed
	postTxErr     error
}

func newMockChainClient() *mockChainClient {
	return &mockChainClient{
		blockHashes: make(map[uint64]string),
		rawBlocks:   make(map[string][]byte),
		txStatuses:  make(map[string]bool),
	}
}

func (m *mockChainClient) PostTx(rawTx string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posted = append(m.posted, rawTx)
	return m.postTxErr
}

func (m *mockChainClient) GetAddressTxs(address string) ([]chain.TxHistoryEntry, error) {
	return nil, nil
}

func (m *mockChainClient) GetRawBlock(hash string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.rawBlocks[hash]
	if !ok {
		return nil, nil
	}
	return data, nil
}

func (m *mockChainClient) GetBlockHashAtHeight(height uint64) (string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hash, ok := m.blockHashes[height]
	if !ok {
		return "", 404, nil
	}
	return hash, 200, nil
}

func (m *mockChainClient) GetTipHeight() (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tipHeight, nil
}

func (m *mockChainClient) GetTxStatus(txid string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.txStatuses[txid], nil
}

