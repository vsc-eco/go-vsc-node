package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/hive/streamer"
	transactionpool "vsc-node/modules/transaction-pool"

	"github.com/chebyrash/promise"
	"github.com/ipfs/go-cid"
	"github.com/vsc-eco/hivego"
)

var (
	// only usage is to build chain map with proper key-value for ChainOracle.chainMap
	_chains = [...]chainRelay{
		&bitcoinRelayer{},
	}

	_ aggregate.Plugin = &ChainOracle{}

	errInvalidChainData   = errors.New("invalid chain data")
	errInvalidChainSymbol = errors.New("invalid chain symbol")
)

type chainRelay interface {
	Init() error
	// Returns the ticker of the chain (ie, BTC for bitcoin).
	Symbol() string
	// Returns the contract ID for this chain's mapping contract.
	ContractId() string
	// Sets the contract ID for this chain's mapping contract.
	SetContractId(id string)
	// Checks for (optional) latest chain state.
	GetLatestValidHeight() (chainState, error)
	// Fetches chaindata and serializes to raw bytes.
	ChainData(startBlockHeight uint64, count uint64) ([]chainBlock, error)
}

type chainBlock interface {
	Type() string //symbol of block network
	Serialize() (string, error)
	BlockHeight() uint64
}

type chainState struct {
	blockHeight uint64
}

// stateKey used by mapping contracts to store the last submitted block height.
const lastHeightStateKey = "lsthgt"

type ChainOracle struct {
	ctx               context.Context
	logger            *slog.Logger
	signatureChannels *signatureChannels
	chainRelayers     map[string]chainRelay
	conf              common.IdentityConfig
	sconf             systemconfig.SystemConfig
	electionDb        elections.Elections
	contractState     contracts.ContractState
	da                *DataLayer.DataLayer
	txCrafter         *transactionpool.TransactionCrafter
	txPool            *transactionpool.TransactionPool
}

func New(
	ctx context.Context,
	oracleLogger *slog.Logger,
	conf common.IdentityConfig,
	sconf systemconfig.SystemConfig,
	electionDb elections.Elections,
	contractState contracts.ContractState,
	da *DataLayer.DataLayer,
	txCrafter *transactionpool.TransactionCrafter,
	txPool *transactionpool.TransactionPool,
) *ChainOracle {
	logger := oracleLogger.With("sub-service", "chain-relay")

	chainRelayers := make(map[string]chainRelay)
	for _, c := range _chains {
		chainRelayers[strings.ToUpper(c.Symbol())] = c
	}

	return &ChainOracle{
		ctx:               ctx,
		logger:            logger,
		signatureChannels: makeSignatureChannels(),
		chainRelayers:     chainRelayers,
		conf:              conf,
		sconf:             sconf,
		electionDb:        electionDb,
		contractState:     contractState,
		da:                da,
		txCrafter:         txCrafter,
		txPool:            txPool,
	}
}

// SetTxCrafter sets the transaction crafter after initialization.
func (c *ChainOracle) SetTxCrafter(txCrafter *transactionpool.TransactionCrafter) {
	c.txCrafter = txCrafter
}

// ConfigureBitcoin sets the bitcoind RPC connection details from the oracle config.
func (c *ChainOracle) ConfigureBitcoin(host, user, pass string) {
	if btc, ok := c.chainRelayers["BTC"]; ok {
		if br, ok := btc.(*bitcoinRelayer); ok {
			br.Configure(host, user, pass)
		}
	}
}

// Init implements aggregate.Plugin.
func (c *ChainOracle) Init() error {
	// initializes market api's
	for symbol, chainRelayer := range c.chainRelayers {

		c.logger.Debug("initializing chain relay: " + symbol)
		if err := chainRelayer.Init(); err != nil {
			return fmt.Errorf(
				"failed to initialize chainrelayer %s: %w",
				symbol, err,
			)
		}
	}

	// Set contract IDs from system config
	if btc, ok := c.chainRelayers["BTC"]; ok {
		if id := c.sconf.OracleParams().BtcContractId; id != "" {
			btc.SetContractId(id)
		}
	}

	return nil
}

// Start implements aggregate.Plugin.
func (c *ChainOracle) Start() *promise.Promise[any] {

	startSymbols := make(map[string]string)
	for symbol, chainRelayer := range c.chainRelayers {

		fcl, err := chainRelayer.GetLatestValidHeight()

		fmt.Println("fcl", fcl, symbol)
		if err != nil {
			startSymbols[symbol] = err.Error()
		} else {
			startSymbols[symbol] = strconv.Itoa(int(fcl.blockHeight))
		}
	}
	// TODO: replace with user's URIs
	hiveClient := hivego.NewHiveRpc(streamer.DefaultHiveURIs)
	hiveClient.ChainID = c.sconf.HiveChainId()

	jsonBytes, _ := json.Marshal(startSymbols)
	wif := c.conf.Get().HiveActiveKey
	hiveClient.BroadcastJson(
		[]string{c.conf.Get().HiveUsername},
		[]string{},
		"dev_vsc.chain_oracle",
		string(jsonBytes),
		&wif,
	)
	return promise.New(func(resolve func(any), _ func(error)) {
		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
func (c *ChainOracle) Stop() error {
	return nil
}

func (c *ChainOracle) fetchAllStatuses() []chainSession {
	chainSessions := make([]chainSession, 0)

	for _, chain := range c.chainRelayers {
		chainSession, err := c.fetchChainStatus(chain)
		if err != nil {
			c.logger.Error(
				"failed to get chain data.",
				"symbol", chain.Symbol(), "err", err,
			)
			continue
		}

		chainSessions = append(chainSessions, chainSession)
	}

	return chainSessions
}

type chainSession struct {
	symbol            string
	contractId        string
	chainData         []chainBlock
	newBlocksToSubmit bool
}

// getContractBlockHeight reads the last submitted block height from a
// mapping contract's state via the data layer.
func (c *ChainOracle) getContractBlockHeight(contractId string) (uint64, error) {
	output, err := c.contractState.GetLastOutput(contractId, math.MaxInt64)
	if err != nil {
		return 0, fmt.Errorf("no contract output found for %s: %w", contractId, err)
	}

	cidz, err := cid.Parse(output.StateMerkle)
	if err != nil {
		return 0, fmt.Errorf("failed to parse state merkle CID: %w", err)
	}

	databin := DataLayer.NewDataBinFromCid(c.da, cidz)
	cidVal, err := databin.Get(lastHeightStateKey)
	if err != nil {
		if err == os.ErrNotExist {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get %s from state: %w", lastHeightStateKey, err)
	}

	rawVal, err := c.da.GetRaw(*cidVal)
	if err != nil {
		return 0, fmt.Errorf("failed to read state value: %w", err)
	}

	height, err := strconv.ParseUint(string(rawVal), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse block height %q: %w", string(rawVal), err)
	}

	return height, nil
}

func (c *ChainOracle) fetchChainStatus(chain chainRelay) (chainSession, error) {
	contractId := chain.ContractId()
	if contractId == "" {
		return chainSession{}, fmt.Errorf("no contract ID configured for %s relay", chain.Symbol())
	}

	latestChainState, err := chain.GetLatestValidHeight()
	if err != nil {
		return chainSession{
			newBlocksToSubmit: false,
		}, fmt.Errorf("failed to check latest state: %w", err)
	}

	contractHeight, err := c.getContractBlockHeight(contractId)
	if err != nil || contractHeight == 0 {
		// When contract state is unavailable (e.g. new or dummy contract),
		// start from near the chain tip instead of block 0 to avoid
		// requesting pruned blocks.
		contractHeight = latestChainState.blockHeight - 1
		c.logger.Warn("failed to get contract state, starting near chain tip",
			"symbol", chain.Symbol(),
			"contractId", contractId,
			"fallbackHeight", contractHeight,
			"err", err,
		)
	}

	if latestChainState.blockHeight <= contractHeight {
		return chainSession{
			symbol:            chain.Symbol(),
			newBlocksToSubmit: false,
		}, nil
	}

	chainData, err := chain.ChainData(contractHeight+1, 100)
	if err != nil {
		return chainSession{}, fmt.Errorf("failed to get chain data: %w", err)
	}

	return chainSession{
		symbol:            chain.Symbol(),
		contractId:        contractId,
		chainData:         chainData,
		newBlocksToSubmit: true,
	}, nil
}

func makeChainSessionID(c *chainSession) (string, error) {
	if len(c.chainData) == 0 {
		return "", errors.New("chainData not supplied")
	}

	startBlock := c.chainData[0].BlockHeight()
	endBlock := c.chainData[len(c.chainData)-1].BlockHeight()

	id := fmt.Sprintf("%s-%d-%d", c.symbol, startBlock, endBlock)
	return id, nil
}

// parseChainSessionID parses "SYMBOL-startBlock-endBlock" format.
func parseChainSessionID(sessionID string) (string, uint64, uint64, error) {
	parts := strings.Split(sessionID, "-")
	if len(parts) != 3 {
		return "", 0, 0, fmt.Errorf("invalid session ID format: %s", sessionID)
	}

	chainSymbol := parts[0]
	startBlock, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid start block: %w", err)
	}

	endBlock, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid end block: %w", err)
	}

	return chainSymbol, startBlock, endBlock, nil
}
