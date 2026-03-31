// Package chain implements a multi-chain block relay system for the VSC oracle.
//
// The chain relay watches external blockchains (BTC, DASH, ETH, etc.) and submits
// their block headers to VSC mapping contracts via BLS-signed consensus transactions.
//
// # Architecture
//
// The system is built around two interfaces:
//
//   - chainRelay: implemented per-chain (e.g. bitcoin.go) to handle RPC
//     communication, block fetching, and serialization.
//   - chainBlock: represents a single block from any chain, with methods
//     to serialize it for relay.
//
// # Adding a new chain
//
// 1. Create a new file (e.g. dash.go) implementing chainRelay and chainBlock.
// 2. Self-register via init(): func init() { RegisterChain(&dashRelayer{}) }
// 3. Add the chain's contract ID to ChainContracts in system-config.
// 4. Add RPC connection details to the Chains map in the oracle config JSON.
//
// No other files need to be modified — the consensus, P2P signature collection,
// and transaction submission logic is fully chain-agnostic.
package chain

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/nonces"
	transactionpool "vsc-node/modules/transaction-pool"

	"github.com/chebyrash/promise"
	"github.com/ipfs/go-cid"
)

var (
	_ aggregate.Plugin = &ChainOracle{}

	errInvalidChainData   = errors.New("invalid chain data")
	errInvalidChainSymbol = errors.New("invalid chain symbol")
)

// chainRelay is the interface that chain-specific relayers must implement.
// To add a new chain, implement this interface and register it via
// RegisterChain() in an init() function.
type chainRelay interface {
	Init(sconf systemconfig.SystemConfig) error
	// Returns the ticker of the chain (e.g. "BTC", "DASH").
	Symbol() string
	// Returns the contract ID for this chain's mapping contract.
	ContractId() string
	// Sets the contract ID for this chain's mapping contract.
	SetContractId(id string)
	// Configure sets the RPC connection details for this chain.
	Configure(host, user, pass string)
	// Checks for (optional) latest chain state.
	GetLatestValidHeight() (chainState, error)
	// Fetches chaindata and serializes to raw bytes. latestValidHeight is the
	// inclusive upper bound beyond which blocks must not be returned (e.g. the
	// chain tip minus the relayer's validity threshold). Implementations cap
	// the fetch range at this value so callers can never receive too-recent
	// blocks that are still subject to reorg.
	ChainData(ctx context.Context, startBlockHeight uint64, count uint64, latestValidHeight uint64) ([]chainBlock, error)
	// GetCanonicalBlockHeader returns the raw 80-byte block header hex for a
	// given height according to the chain's RPC. Used for reorg detection.
	// Chains that don't support this should return "", nil.
	GetCanonicalBlockHeader(height uint64) (string, error)
	// Clone returns a fresh, independent copy of this relayer.
	// Used by New() to avoid sharing singleton state from the registry.
	Clone() chainRelay
	// AutoReorgDetection returns true if the oracle should automatically
	// detect and fix reorgs by calling replaceBlock on the contract.
	AutoReorgDetection() bool
}

// chainRegistry holds all registered chain relayers.
// Chains register themselves via RegisterChain(), typically in init().
var chainRegistry = make(map[string]chainRelay)

// RegisterChain registers a chain relayer. Call this from init() in
// each chain's source file (e.g. bitcoin.go, dash.go).
func RegisterChain(r chainRelay) {
	chainRegistry[strings.ToUpper(r.Symbol())] = r
}

// RegisteredChains returns the symbols of all registered chains.
func RegisteredChains() map[string]struct{} {
	result := make(map[string]struct{}, len(chainRegistry))
	for symbol := range chainRegistry {
		result[symbol] = struct{}{}
	}
	return result
}

// chainBlock represents a single block from any external chain.
// Each chain implementation provides its own struct satisfying this interface.
type chainBlock interface {
	// Type returns the chain symbol (e.g. "BTC", "DASH").
	Type() string
	// Serialize encodes the block (typically the header) as a hex string
	// for inclusion in the relay transaction payload.
	Serialize() (string, error)
	// BlockHeight returns the block's height on its native chain.
	BlockHeight() uint64
}

// chainState holds minimal chain tip information returned by GetLatestValidHeight.
type chainState struct {
	blockHeight uint64
}

// stateKey used by mapping contracts to store the last submitted block height.
const lastHeightStateKey = "h"

// ChainOracle orchestrates block relay for all registered chains.
// It runs as an aggregate.Plugin and is driven by Hive block ticks.
// On each tick (when this node is the block producer), it:
//  1. Fetches new blocks from each chain's RPC via the chainRelay interface.
//  2. Builds a relay transaction and requests BLS signatures from peer witnesses.
//  3. Once 2/3+ weighted signatures are collected, submits the signed transaction.
//
// All consensus, P2P, and submission logic is chain-agnostic — only the
// chainRelay implementations contain chain-specific code.
type ChainOracle struct {
	ctx               context.Context
	logger            *vsclog.Logger
	signatureChannels *signatureChannels
	chainRelayers     map[string]chainRelay // symbol -> relayer instance
	conf              common.IdentityConfig
	sconf             systemconfig.SystemConfig
	electionDb        elections.Elections
	contractState     contracts.ContractState
	da                *DataLayer.DataLayer
	txCrafter         *transactionpool.TransactionCrafter
	txPool            *transactionpool.TransactionPool
	nonceDb           nonces.Nonces
	// lastSubmittedEnd tracks the end height of the last submitted block range
	// per chain symbol. Any new submission whose start height <= this value
	// is skipped until the contract state catches up, preventing overlapping
	// batches when the previous tx is still in the mempool.
	lastSubmittedEnd map[string]uint64    // symbol -> endHeight
	lastSubmittedAt  map[string]time.Time // symbol -> when submitted
	// recentlyWitnessed tracks block ranges this node recently signed as a
	// witness for another producer. If we become producer and see the same
	// range, we skip it to avoid duplicate submissions across nodes.
	recentlyWitnessed map[string]time.Time // "SYMBOL:startHeight-endHeight" -> when witnessed
}

func New(
	ctx context.Context,
	oracleLogger *vsclog.Logger,
	conf common.IdentityConfig,
	sconf systemconfig.SystemConfig,
	electionDb elections.Elections,
	contractState contracts.ContractState,
	da *DataLayer.DataLayer,
	txCrafter *transactionpool.TransactionCrafter,
	txPool *transactionpool.TransactionPool,
	nonceDb nonces.Nonces,
) *ChainOracle {
	logger := oracleLogger.With("sub-service", "chain-relay")

	// Clone registered chains so this instance owns independent state.
	chainRelayers := make(map[string]chainRelay, len(chainRegistry))
	for symbol, c := range chainRegistry {
		chainRelayers[symbol] = c.Clone()
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
		nonceDb:           nonceDb,
		lastSubmittedEnd:  make(map[string]uint64),
		lastSubmittedAt:   make(map[string]time.Time),
		recentlyWitnessed: make(map[string]time.Time),
	}
}

// SetTxCrafter sets the transaction crafter after initialization.
func (c *ChainOracle) SetTxCrafter(txCrafter *transactionpool.TransactionCrafter) {
	c.txCrafter = txCrafter
}

// ConfigureChain sets the RPC connection details for a registered chain.
func (c *ChainOracle) ConfigureChain(symbol, host, user, pass string) {
	if chain, ok := c.chainRelayers[strings.ToUpper(symbol)]; ok {
		chain.Configure(host, user, pass)
	}
}

// Init implements aggregate.Plugin.
func (c *ChainOracle) Init() error {
	// initializes market api's
	for symbol, chainRelayer := range c.chainRelayers {

		c.logger.Debug("initializing chain relay: " + symbol)
		if err := chainRelayer.Init(c.sconf); err != nil {
			return fmt.Errorf(
				"failed to initialize chainrelayer %s: %w",
				symbol, err,
			)
		}
	}

	// Set contract IDs from system config for all registered chains
	oracleParams := c.sconf.OracleParams()
	for symbol, chainRelayer := range c.chainRelayers {
		if id := oracleParams.ContractId(symbol); id != "" {
			chainRelayer.SetContractId(id)
		}
	}

	return nil
}

// Start implements aggregate.Plugin.
func (c *ChainOracle) Start() *promise.Promise[any] {

	startSymbols := make(map[string]string)
	for symbol, chainRelayer := range c.chainRelayers {
		// Only attempt RPC connection for chains that are fully configured
		// (have both a contract ID and RPC details). Skip the rest silently.
		if chainRelayer.ContractId() == "" {
			c.logger.Debug("chain relay not configured, skipping", "symbol", symbol)
			continue
		}

		fcl, err := chainRelayer.GetLatestValidHeight()

		if err != nil {
			startSymbols[symbol] = err.Error()
			c.logger.Error("failed to get latest chain height on startup", "symbol", symbol, "err", err)
		} else {
			startSymbols[symbol] = strconv.Itoa(int(fcl.blockHeight))
			c.logger.Info("chain relay starting", "symbol", symbol, "height", fcl.blockHeight)
		}
	}
	return promise.New(func(resolve func(any), _ func(error)) {
		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
func (c *ChainOracle) Stop() error {
	return nil
}

// fetchAllStatuses queries every registered chain for new blocks to relay.
// Returns a chainSession per chain indicating whether there are blocks to submit.
// Chains without a contract ID are silently skipped — they are registered but
// not yet enabled for this network.
func (c *ChainOracle) fetchAllStatuses() []chainSession {
	chainSessions := make([]chainSession, 0)

	for _, chain := range c.chainRelayers {
		// Skip chains that have no contract ID configured for this network.
		// This avoids noisy error logs for registered-but-unused chains
		// (e.g. DASH/LTC on a node that only relays BTC).
		if chain.ContractId() == "" {
			continue
		}

		// Skip chains that use ZK proof verification instead of oracle relay.
		// A ZK prover submits headers directly to the verifier contract.
		if c.sconf.OracleParams().HasZKVerifier(chain.Symbol()) {
			continue
		}

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

// chainSession captures the relay state for a single chain during one tick:
// which chain, its target contract, the fetched blocks, and whether there
// are new blocks that need submitting.
type chainSession struct {
	symbol            string       // chain ticker (e.g. "BTC")
	contractId        string       // VSC mapping contract for this chain
	chainData         []chainBlock // new blocks fetched from the chain's RPC
	newBlocksToSubmit bool         // true if chainData contains unseen blocks
	replaceBlock      bool         // true if a reorg was detected and replaceBlocks should be called
	replaceBlockHex   string       // concatenated canonical block header hex for replaceBlocks
	replaceBlockDepth int          // number of blocks being replaced (reorg depth)
}

// getContractBlockHeight reads the last submitted block height from a
// mapping contract's state via the data layer.
func (c *ChainOracle) getContractBlockHeight(contractId string) (uint64, error) {
	output, err := c.contractState.GetLastOutput(contractId, math.MaxInt64)
	if err != nil {
		return 0, fmt.Errorf("failed to query contract output for %s: %w", contractId, err)
	}
	// GetLastOutput returns a zero-valued ContractOutput (no error) when the
	// contract has never produced an output. Surface that as the same
	// "fresh contract" signal as a missing height key in the databin so the
	// caller can distinguish fresh state from a transient read error.
	if output.StateMerkle == "" {
		return 0, nil
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

	rawVal, err := c.da.GetRaw(context.Background(), *cidVal)
	if err != nil {
		return 0, fmt.Errorf("failed to read state value: %w", err)
	}

	height, err := strconv.ParseUint(string(rawVal), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse block height %q: %w", string(rawVal), err)
	}

	return height, nil
}

// fetchChainStatus compares a chain's current tip against the last height
// submitted to its mapping contract. If new blocks exist, it fetches up to
// 50 of them via the chain's RPC for relay.
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
	if err != nil {
		// Transient read error (DA unreachable, malformed CID, etc.). Wait
		// and retry on the next tick — do NOT bootstrap, because a transient
		// error on a contract that already has state would submit a
		// tip-relative range that doesn't follow the contract's last height.
		c.logger.Debug("failed to read contract state, waiting",
			"symbol", chain.Symbol(),
			"contractId", contractId,
			"err", err,
		)
		return chainSession{newBlocksToSubmit: false}, nil
	}
	if contractHeight == 0 {
		// Fresh contract: no prior addBlocks output. Only ETH bootstraps
		// today — UTXO chains stay quiet until manually seeded.
		if chain.Symbol() != "ETH" {
			c.logger.Debug("contract has no state, waiting (bootstrap not enabled for chain)",
				"symbol", chain.Symbol(),
				"contractId", contractId,
			)
			return chainSession{newBlocksToSubmit: false}, nil
		}
		const bootstrapLookback = 1
		// On a short chain (devnet / fresh testnet) start from genesis so
		// the contract picks up every block. On a long-running chain,
		// start near the tip to avoid requesting pruned blocks.
		startFromGenesis := latestChainState.blockHeight <= bootstrapLookback
		if !startFromGenesis {
			contractHeight = latestChainState.blockHeight - bootstrapLookback
		}
		c.logger.Info("contract has no state, bootstrapping",
			"symbol", chain.Symbol(),
			"contractId", contractId,
			"startHeight", contractHeight+1,
			"chainTip", latestChainState.blockHeight,
			"fromGenesis", startFromGenesis,
		)
	}

	if latestChainState.blockHeight <= contractHeight {
		c.logger.Debug("no new blocks to relay",
			"symbol", chain.Symbol(),
			"contractHeight", contractHeight,
			"latestValidHeight", latestChainState.blockHeight,
		)
		return chainSession{
			symbol:            chain.Symbol(),
			newBlocksToSubmit: false,
		}, nil
	}

	// Auto-reorg detection: compare stored block with canonical chain.
	if chain.AutoReorgDetection() && contractHeight > 0 {
		reorgDetected, canonicalHex, depth, err := c.checkForReorg(chain, contractId, contractHeight)
		if err != nil {
			c.logger.Warn("reorg detection check failed, continuing with addBlocks",
				"symbol", chain.Symbol(), "err", err,
			)
		} else if reorgDetected {
			c.logger.Warn("reorg detected! stored block differs from canonical chain",
				"symbol", chain.Symbol(),
				"height", contractHeight,
				"depth", depth,
			)
			return chainSession{
				symbol:            chain.Symbol(),
				contractId:        contractId,
				newBlocksToSubmit: true,
				replaceBlock:      true,
				replaceBlockHex:   canonicalHex,
				replaceBlockDepth: depth,
			}, nil
		}
	}

	var chainData []chainBlock
	if chain.Symbol() == "ETH" {
		chainData, err = chain.ChainData(c.ctx, contractHeight+1, 35, latestChainState.blockHeight)
	} else {
		chainData, err = chain.ChainData(c.ctx, contractHeight+1, 50, latestChainState.blockHeight)
	}
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

// maxReorgDepth is the maximum number of blocks we'll walk back to find the
// fork point during reorg detection. This prevents unbounded RPC calls.
const maxReorgDepth = 20

// checkForReorg compares block headers stored in the contract with the canonical
// chain. If a mismatch is found at the tip, it walks backwards to find the full
// reorg depth. Returns: detected bool, concatenated canonical headers hex
// (oldest-first), reorg depth, and error.
func (c *ChainOracle) checkForReorg(chain chainRelay, contractId string, height uint64) (bool, string, int, error) {
	// Check the tip first
	canonicalHex, err := chain.GetCanonicalBlockHeader(height)
	if err != nil {
		return false, "", 0, fmt.Errorf("failed to get canonical header at %d: %w", height, err)
	}
	if canonicalHex == "" {
		return false, "", 0, nil // chain doesn't support this check
	}

	storedHex, err := c.getStoredBlockHeaderHex(contractId, height)
	if err != nil {
		return false, "", 0, fmt.Errorf("failed to get stored header at %d: %w", height, err)
	}

	if storedHex == canonicalHex {
		return false, "", 0, nil // no reorg
	}

	// Reorg detected at tip. Walk backwards to find the fork point.
	// Collect canonical headers from the reorged range (oldest first).
	canonicalHeaders := []string{canonicalHex}

	for depth := 1; depth < maxReorgDepth; depth++ {
		checkHeight := height - uint64(depth)
		if checkHeight == 0 {
			break // can't go below height 1
		}

		canonical, err := chain.GetCanonicalBlockHeader(checkHeight)
		if err != nil {
			c.logger.Warn("reorg walk-back: failed to get canonical header",
				"symbol", chain.Symbol(), "height", checkHeight, "err", err,
			)
			break
		}

		stored, err := c.getStoredBlockHeaderHex(contractId, checkHeight)
		if err != nil {
			c.logger.Warn("reorg walk-back: failed to get stored header",
				"symbol", chain.Symbol(), "height", checkHeight, "err", err,
			)
			break
		}

		if stored == canonical {
			break // found the fork point — this block matches
		}

		// This block is also reorged; prepend its canonical header
		canonicalHeaders = append([]string{canonical}, canonicalHeaders...)
	}

	// Concatenate all canonical headers (oldest first) into one hex string
	concatenated := ""
	for _, h := range canonicalHeaders {
		concatenated += h
	}

	depth := len(canonicalHeaders)
	c.logger.Info("reorg depth determined",
		"symbol", chain.Symbol(),
		"tipHeight", height,
		"depth", depth,
		"forkPoint", height-uint64(depth),
	)

	return true, concatenated, depth, nil
}

// getStoredBlockHeaderHex reads the raw block header from the contract state
// at the given height and returns it as a hex string.
func (c *ChainOracle) getStoredBlockHeaderHex(contractId string, height uint64) (string, error) {
	output, err := c.contractState.GetLastOutput(contractId, math.MaxInt64)
	if err != nil {
		return "", fmt.Errorf("no contract output found: %w", err)
	}

	cidz, err := cid.Parse(output.StateMerkle)
	if err != nil {
		return "", fmt.Errorf("failed to parse state merkle: %w", err)
	}

	blockKey := "b-" + strconv.FormatUint(height, 10)
	databin := DataLayer.NewDataBinFromCid(c.da, cidz)
	cidVal, err := databin.Get(blockKey)
	if err != nil {
		return "", fmt.Errorf("block key %s not found in state: %w", blockKey, err)
	}

	rawVal, err := c.da.GetRaw(context.Background(), *cidVal)
	if err != nil {
		return "", fmt.Errorf("failed to read block data: %w", err)
	}

	return fmt.Sprintf("%x", rawVal), nil
}

// ChainStatus holds the block height status of a single chain relayer.
type ChainStatus struct {
	Symbol      string
	BlockHeight *uint64 // nil if unreachable or not configured
}

// GetAllChainStatuses returns the block height status for every registered chain.
func (c *ChainOracle) GetAllChainStatuses() []ChainStatus {
	statuses := make([]ChainStatus, 0, len(c.chainRelayers))
	for symbol, chain := range c.chainRelayers {
		cs := ChainStatus{Symbol: symbol}
		if chain.ContractId() != "" {
			if state, err := chain.GetLatestValidHeight(); err == nil {
				h := state.blockHeight
				cs.BlockHeight = &h
			}
		}
		statuses = append(statuses, cs)
	}
	return statuses
}

// makeChainSessionID builds a unique session identifier for P2P signature
// collection in the format "SYMBOL-hiveHeight-startBlock-endBlock"
// (e.g. "BTC-93000000-640000-640100"). Including the Hive block height
// prevents collisions when the same chain block range is retried across
// multiple Hive blocks.
func makeChainSessionID(c *chainSession, hiveBlockHeight uint64) (string, error) {
	if c.replaceBlock {
		id := fmt.Sprintf("%s-%d-replace", c.symbol, hiveBlockHeight)
		return id, nil
	}

	if len(c.chainData) == 0 {
		return "", errors.New("chainData not supplied")
	}

	startBlock := c.chainData[0].BlockHeight()
	endBlock := c.chainData[len(c.chainData)-1].BlockHeight()

	id := fmt.Sprintf("%s-%d-%d-%d", c.symbol, hiveBlockHeight, startBlock, endBlock)
	return id, nil
}

// parseChainSessionID parses "SYMBOL-hiveHeight-startBlock-endBlock" format.
func parseChainSessionID(sessionID string) (string, uint64, uint64, uint64, error) {
	parts := strings.Split(sessionID, "-")
	if len(parts) != 4 {
		return "", 0, 0, 0, fmt.Errorf("invalid session ID format: %s", sessionID)
	}

	chainSymbol := parts[0]
	hiveBlockHeight, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("invalid hive block height: %w", err)
	}

	startBlock, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("invalid start block: %w", err)
	}

	endBlock, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("invalid end block: %w", err)
	}

	return chainSymbol, hiveBlockHeight, startBlock, endBlock, nil
}
