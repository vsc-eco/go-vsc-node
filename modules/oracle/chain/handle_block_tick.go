package chain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/chain/api"
	"vsc-node/modules/oracle/p2p"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

	"github.com/hasura/go-graphql-client"
)

type blockProducer struct {
	ctx                    context.Context
	logger                 *slog.Logger
	username               string
	p2pSpec                p2p.OracleP2PSpec
	sigChan                *signatureChannels
	electedMembers         []elections.ElectionMember
	totalWeight            uint64
	electedMemberWeightMap []uint64
}

// input expected by the contract
type AddBlocksInput struct {
	Blocks    string `json:"blocks"`
	LatestFee int64  `json:"latest_fee"`
}

// HandleBlockTick implements oracle.BlockTickHandler.
func (o *ChainOracle) HandleBlockTick(
	signal p2p.BlockTickSignal,
	p2pSpec p2p.OracleP2PSpec,
) {
	o.logger.Debug("block tick event")

	if !signal.IsProducer {
		return
	}

	bp := &blockProducer{
		ctx:                    o.ctx,
		logger:                 o.logger,
		username:               o.conf.Get().HiveUsername,
		p2pSpec:                p2pSpec,
		sigChan:                o.signatureChannels,
		electedMembers:         signal.ElectedMembers,
		totalWeight:            signal.TotalElectionWeight,
		electedMemberWeightMap: signal.WeightMap,
	}

	for symbol, chain := range o.chainRelayers {
		session, err := o.makeChainSession(symbol)
		if err != nil {
			o.logger.Error("failed to get chain session",
				"symbol", symbol,
				"err", err,
			)
			continue
		}

		if err := bp.handleChainSession(chain, session); err != nil {
			o.logger.Error(
				"failed to process chain session",
				"network", symbol, "err", err,
			)
		}
	}
}

func (bp *blockProducer) handleChainSession(
	chain api.ChainRelay,
	chainSession api.ChainSession,
) error {
	bp.logger.Debug(
		"chain session",
		"symbol", chain.Symbol(),
		"session-id", chainSession.SessionID,
	)

	if len(bp.electedMemberWeightMap) != len(bp.electedMembers) {
		return errors.New(
			"invalid weightMap or members of the current election",
		)
	}

	// make signature receiver
	sigChan, err := bp.sigChan.makeSession(chainSession.SessionID)
	if err != nil {
		return fmt.Errorf("failed to make signature session: %w", err)
	}
	defer bp.sigChan.removeSession(chainSession.SessionID)

	// create tx + chain hash
	tx, err := makeChainTx(chain, chainSession.ChainData)
	txCid := tx.Cid()

	// create transaction + bls circuit
	witnessDIDs := make([]dids.Member, len(bp.electedMembers))
	for i := range bp.electedMembers {
		witnessDIDs[i] = dids.BlsDID(bp.electedMembers[i].Key)
	}

	circuit, err := dids.NewBlsCircuitGenerator(witnessDIDs).Generate(txCid)
	if err != nil {
		return fmt.Errorf("failed to generate bls circuit: %w", err)
	}

	// collect witness data
	witnessAccounts := make([]string, len(bp.electedMembers))
	for i := range bp.electedMembers {
		witnessAccounts[i] = bp.electedMembers[i].Account
	}

	// broadcast signature request
	blockProducerMsg := chainOracleBlockProducerMessage{
		BlockProducer: bp.username,
		SigHash:       txCid.String(),
	}

	msgJsonBytes, err := json.Marshal(&blockProducerMsg)
	if err != nil {
		return fmt.Errorf("failed to serialize block producer message: %w", err)
	}

	signatureRequestMsg := chainOracleMessage{
		MessageType: signatureRequest,
		SessionID:   chainSession.SessionID,
		Payload:     json.RawMessage(msgJsonBytes),
	}

	if err := bp.p2pSpec.Broadcast(p2p.MsgChainOracle, &signatureRequestMsg); err != nil {
		return fmt.Errorf("failed to broadcast message: %w", err)
	}

	bp.logger.Debug(
		"broadcasted signature request message",
		"message", signatureRequestMsg,
	)

	// collect witness signatures
	ctx, cancel := context.WithTimeout(bp.ctx, time.Minute)
	defer cancel()

	if err := bp.collectSignature(ctx, circuit, sigChan); err != nil {
		return fmt.Errorf("failed to collect signature: %w", err)
	}

	// make transaction + submit to contract
	blsCircuit, err := circuit.Finalize()
	if err != nil {
		return fmt.Errorf("failed to finalize circuit: %w", err)
	}

	serializedCircuit, err := blsCircuit.Serialize()
	if err != nil {
		return fmt.Errorf("failed to finalize circuit: %w", err)
	}

	sigPackage := stateEngine.TransactionSig{
		Type: "vsc-sig",
		Sigs: []common.Sig{{
			Algo: "bls-agg",
			Sig:  serializedCircuit.Signature,
			Bv:   serializedCircuit.BitVector,
			Kid:  "",
		}},
	}

	sigBytes, err := common.EncodeDagCbor(sigPackage)
	if err != nil {
		return fmt.Errorf("failed encode DagCbor: %w", err)
	}

	//Submit to graphql SubmitTransaction()
	vscTx := transactionpool.SerializedVSCTransaction{
		Tx:  tx.Cid().Bytes(),
		Sig: sigBytes,
	}

	if err := bp.submitToContract(vscTx); err != nil {
		return fmt.Errorf("failed to submit to contract: %w", err)
	}

	return nil
}

func (bp *blockProducer) collectSignature(
	ctx context.Context,
	circuit dids.PartialBlsCircuit,
	sigChan <-chan chainOracleWitnessMessage,
) error {
	weightThreshold := (bp.totalWeight * 2) / 3
	signedWeight := uint64(0)

	for signedWeight < weightThreshold {
		select {
		case <-ctx.Done():
			err := ctx.Err()
			if errors.Is(err, context.DeadlineExceeded) {
				return errors.New("signature collection timed out")
			}
			return err

		case msg := <-sigChan:
			memberIndex := slices.IndexFunc(
				bp.electedMembers,
				func(m elections.ElectionMember) bool {
					return m.Account == msg.Signer
				},
			)

			if memberIndex == -1 {
				bp.logger.Debug(
					"invalid witness signature, dropping.",
					"signer", msg.Signer,
				)
				continue
			}

			memberDID := dids.BlsDID(bp.electedMembers[memberIndex].Key)

			added, err := circuit.AddAndVerify(memberDID, msg.Signature)
			if err != nil {
				bp.logger.Error("failed to add member to circuit.", "err", err)
				continue
			}

			if !added {
				bp.logger.Debug("invalid member, signature not added to circuit")
				continue
			}

			signedWeight += bp.electedMemberWeightMap[memberIndex]

			bp.logger.Debug(
				"witness signature verified",
				"progress", float32(signedWeight)/float32(weightThreshold),
				"signer", msg.Signer, "signature", msg.Signature,
			)
		}
	}

	return nil
}

func (bp *blockProducer) submitToContract(
	vscTx transactionpool.SerializedVSCTransaction,
) error {
	const timer = time.Minute

	var query struct {
		Result struct{} `graphql:"submitTransactionV1(tx: $tx, sig: $sig)"`
	}

	enc := base64.StdEncoding.EncodeToString
	variables := map[string]any{
		"tx":  graphql.String(enc(vscTx.Tx)),
		"sig": graphql.String(enc(vscTx.Sig)),
	}

	opName := graphql.OperationName("SubmitContract")

	client := graphql.NewClient(
		"https://api.vsc.eco/api/v1/graphql",
		&http.Client{Timeout: timer},
	)

	ctx, cancel := context.WithTimeout(bp.ctx, timer)
	defer cancel()

	if err := client.Query(ctx, &query, variables, opName); err != nil {
		return fmt.Errorf("failed graphql query: %w", err)
	}

	return nil
}
