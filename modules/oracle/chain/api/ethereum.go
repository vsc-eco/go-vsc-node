package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"time"
)

const (
	etherGasCost = 1e18 // 1 gas costs 0.0000000001 ether
)

var (
	_ ChainBlock = &ethereumBlock{}
)

type Ethereum struct {
	ctx        context.Context
	httpClient *http.Client
	infuraURL  url.URL
}

// Init implements ChainRelay.
func (e *Ethereum) Init(ctx context.Context) error {
	apiKey, ok := os.LookupEnv("METAMASK_API_KEY")
	if !ok {
		return fmt.Errorf("env `METAMASK_API_KEY` not set")
	}

	infuraURL, err := url.Parse("https://mainnet.infura.io/v3/")
	if err != nil {
		return fmt.Errorf("failed to parse infura URL: %w", err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}

	*e = Ethereum{
		ctx: ctx,
		httpClient: &http.Client{
			Jar:     jar,
			Timeout: 15 * time.Second,
		},
		infuraURL: *infuraURL.JoinPath(apiKey),
	}

	return nil
}

// Symbol implements ChainRelay.
func (e *Ethereum) Symbol() string {
	return "eth"
}

// ChainData implements ChainRelay.
func (e *Ethereum) ChainData(startBlockHeight uint64, count uint64) ([]ChainBlock, error) {
	jsonRpcRequests := make([]jsonRPCRequest, count)
	for i := range count {
		params := []any{
			fmt.Sprintf("0x%x", startBlockHeight+i),
			true, // hydrating transactions
		}

		paramBytes, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize param: %w", err)
		}

		jsonRpcRequests[i] = jsonRPCRequest{
			JsonRPC: "2.0",
			Method:  "eth_getBlockByNumber",
			ID:      i + 1,
			Params:  paramBytes,
		}
	}

	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(jsonRpcRequests); err != nil {
		return nil, fmt.Errorf("failed to serialize request body: %w", err)
	}

	// send http request
	ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.infuraURL.String(), body)
	if err != nil {
		return nil, fmt.Errorf("failed to make http request: %w", err)
	}

	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed failed: %w", err)
	}
	defer httpResp.Body.Close()

	jsonRpcResponses := make([]jsonRPCResponse, count)
	if err := json.NewDecoder(httpResp.Body).Decode(&jsonRpcResponses); err != nil {
		return nil, fmt.Errorf("failed to deserialize http repsponse: %w", err)
	}

	// deserialize results
	chainData := make([]ChainBlock, len(jsonRpcResponses))
	for i := range chainData {
		rpcResp := &jsonRpcResponses[i]
		if err := rpcResp.Error; err != nil {
			return nil, fmt.Errorf(
				"rpc %d failed: code %d, err %s",
				rpcResp.ID, rpcResp.Error.Code, rpcResp.Error.Message,
			)
		}

		ethBlock := &ethereumBlock{}
		if err := json.Unmarshal(rpcResp.Result, ethBlock); err != nil {
			return nil, fmt.Errorf("failed to deserialize rpc result: %w", err)
		}

		chainData[i] = ethBlock
	}

	return chainData, nil
}

// ContractID implements ChainRelay.
func (e *Ethereum) ContractID() string {
	// TODO: update ETH contract ID
	return "eth-contract-id"
}

// GetContractState implements ChainRelay.
func (e *Ethereum) GetContractState() (ChainState, error) {
	panic("unimplemented")
}

// GetLatestValidHeight implements ChainRelay.
func (e *Ethereum) GetLatestValidHeight() (ChainState, error) {
	params, err := json.Marshal([]any{})
	if err != nil {
		return ChainState{}, fmt.Errorf("failed to serialized empty param: %s", err)
	}

	rpcRequest := jsonRPCRequest{
		JsonRPC: "2.0",
		Method:  "eth_blockNumber",
		ID:      1,
		Params:  params,
	}

	rpcResponse, err := e.sendRpc(rpcRequest)
	if err != nil {
		return ChainState{}, fmt.Errorf("failed to request latest valid height: %w", err)
	}

	var blockNumHex string
	if err := json.Unmarshal(rpcResponse.Result, &blockNumHex); err != nil {
		return ChainState{}, fmt.Errorf("failed to deserialize response result: %w", err)
	}

	cs := ChainState{}
	if _, err := fmt.Sscanf(blockNumHex, "0x%x", &cs.BlockHeight); err != nil {
		return ChainState{}, fmt.Errorf("failed to parse block height hex: %w", err)
	}

	return cs, nil
}

func (e *Ethereum) sendRpc(rpcRequest jsonRPCRequest) (*jsonRPCResponse, error) {
	reqBody := &bytes.Buffer{}
	if err := json.NewEncoder(reqBody).Encode(rpcRequest); err != nil {
		return nil, fmt.Errorf("failed to serialize jsonrpc requests: %w", err)
	}

	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()

	// ethRPC := "https://ethereum-rpc.publicnode.com"
	ethRPC := e.infuraURL.String()

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, ethRPC, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create new request: %w", err)
	}
	httpRequest.Header.Add("content-type", "application/json")

	httpResponse, err := e.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to send HTTP request: %w", err)
	}

	defer httpResponse.Body.Close()

	if httpResponse.StatusCode != http.StatusOK {
		buf := &bytes.Buffer{}
		if _, err := io.Copy(buf, httpResponse.Body); err != nil {
			return nil, fmt.Errorf("failed to read response error: %w", err)
		}
		return nil, fmt.Errorf("request failed: %s", buf.String())
	}

	rpcResponse := &jsonRPCResponse{}
	if err := json.NewDecoder(httpResponse.Body).Decode(rpcResponse); err != nil {
		return nil, fmt.Errorf("failed to serialize rpc responses: %w", err)
	}

	if err := rpcResponse.Error; err != nil {
		return nil, fmt.Errorf("failed rpc request: [code: %d] %s", err.Code, err.Message)
	}

	return rpcResponse, nil
}

// eth block interface

type ethereumBlock struct {
	BaseFeePerGas         string           `json:"baseFeePerGas"`
	BlobGasUsed           string           `json:"blobGasUsed"`
	Difficulty            string           `json:"difficulty"`
	ExcessBlobGas         string           `json:"excessBlobGas"`
	ExtraData             string           `json:"extraData"`
	GasLimit              string           `json:"gasLimit"`
	GasUsed               string           `json:"gasUsed"`
	Hash                  string           `json:"hash"`
	LogsBloom             string           `json:"logsBloom"`
	Miner                 string           `json:"miner"`
	MixHash               string           `json:"mixHash"`
	Nonce                 string           `json:"nonce"`
	Number                string           `json:"number"`
	ParentBeaconBlockRoot string           `json:"parentBeaconBlockRoot"`
	ParentHash            string           `json:"parentHash"`
	ReceiptsRoot          string           `json:"receiptsRoot"`
	Sha3Uncles            string           `json:"sha3Uncles"`
	Size                  string           `json:"size"`
	StateRoot             string           `json:"stateRoot"`
	Timestamp             string           `json:"timestamp"`
	Transactions          []ethTransaction `json:"transactions"`
	TransactionsRoot      string           `json:"transactionsRoot"`
	Uncles                []any            `json:"uncles"`
	Withdrawals           []ethWithdrawal  `json:"withdrawals"`
	WithdrawalsRoot       string           `json:"withdrawalsRoot"`
}

// AverageFee implements ChainBlock.
// returns the average gas per transaction
// TODO: ask Spencer if he wants the average gas per transaction of if he wants the fee.
// returned average gas per transaction (gwei). See https://ethereum.org/developers/docs/gas/#what-is-gas
func (e *ethereumBlock) AverageFee() (uint64, error) {
	totalGas := uint64(0)

	for _, tx := range e.Transactions {
		txGas, err := tx.gasFee(e.BaseFeePerGas)
		if err != nil {
			return 0, fmt.Errorf("failed to get transaction %s total fee: %w", tx.BlockNumber, err)
		}
		totalGas += txGas
	}

	avgGasPerTx := totalGas / uint64(len(e.Transactions))

	return avgGasPerTx, nil
}

// BlockHeight implements ChainBlock.
func (e *ethereumBlock) BlockHeight() (uint64, error) {
	var blockHeight uint64
	_, err := fmt.Sscanf(e.Number, "0x%x", &blockHeight)
	return blockHeight, err
}

// Serialize implements ChainBlock.
func (e *ethereumBlock) Serialize() (string, error) {
	return e.Hash, nil
}

// Type implements ChainBlock.
func (e *ethereumBlock) Type() string {
	return "ETH"
}

type ethTransaction struct {
	BlockHash            string `json:"blockHash"`
	BlockNumber          string `json:"blockNumber"`
	From                 string `json:"from"`
	Gas                  string `json:"gas"`
	GasPrice             string `json:"gasPrice"`
	MaxPriorityFeePerGas string `json:"maxPriorityFeePerGas"`
	MaxFeePerGas         string `json:"maxFeePerGas"`
	Hash                 string `json:"hash"`
	Input                string `json:"input"`
	Nonce                string `json:"nonce"`
	To                   string `json:"to"`
	TransactionIndex     string `json:"transactionIndex"`
	Value                string `json:"value"`
	Type                 string `json:"type"`
	AccessList           []any  `json:"accessList"`
	ChainID              string `json:"chainId"`
	V                    string `json:"v"`
	YParity              string `json:"yParity"`
	R                    string `json:"r"`
	S                    string `json:"s"`
}

// See: https://ethereum.org/developers/docs/gas/#how-are-gas-fees-calculated
// gasUsed * gasPrice
func (eTx *ethTransaction) gasFee(baseFeeHex string) (uint64, error) {
	var (
		gasPrice uint64
		err      error
	)

	switch eTx.Type {
	case "0x0", "0x1":
		_, err = fmt.Sscanf(eTx.GasPrice, "0x%x", &gasPrice)

	case "0x2", "0x3": // EIP-1559 transactions
		var maxFeePerGas, priorityFee, baseFee uint64
		_, err = fmt.Sscanf(baseFeeHex, "0x%x", &baseFee)
		if err != nil {
			err = fmt.Errorf("invalid base fee hex: %w", err)
			break
		}

		_, err = fmt.Sscanf(eTx.MaxFeePerGas, "0x%x", &maxFeePerGas)
		if err != nil {
			err = fmt.Errorf("invalid max fee per gas hex - %w", err)
			break
		}

		_, err = fmt.Sscanf(eTx.MaxPriorityFeePerGas, "0x%x", &priorityFee)
		if err != nil {
			err = fmt.Errorf("invalid max priority fee per gas hex - %w", err)
			break
		}

		gasPrice = min(maxFeePerGas, baseFee+priorityFee)
		if eTx.Type == "0x3" {
			// TODO: need to add in blob fee
		}

	default:
		return 0, fmt.Errorf("unparsed transaction type: %s", eTx.Type)
	}

	if err != nil {
		return 0, fmt.Errorf("failed to parse gas price: %w", err)
	}

	var gasUsed uint64
	if _, err := fmt.Sscanf(eTx.Gas, "0x%x", &gasUsed); err != nil {
		return 0, fmt.Errorf("failed to parse gas used: %w", err)
	}

	return gasUsed * gasPrice, nil
}

type ethWithdrawal struct {
	Index          string `json:"index"`
	ValidatorIndex string `json:"validatorIndex"`
	Address        string `json:"address"`
	Amount         string `json:"amount"`
}

// RPC

type jsonRPCRequest struct {
	JsonRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      uint64          `json:"id"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *jsonRPCError   `json:"error,omitempty"`
}
