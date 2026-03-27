package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mapper"

	"github.com/go-playground/validator/v10"
)

var requestValidator = validator.New(validator.WithRequiredStructEnabled())

// staleBlockMultiplier is how many block intervals without a processed block
// before health is degraded. 2x gives one missed block of headroom.
const staleBlockMultiplier = 2

func mapBotHttpServer(
	ctx context.Context,
	addressStore *database.AddressStore,
	bot *mapper.Bot,
) {
	if addressStore == nil {
		fmt.Fprintf(os.Stderr, "datastore or mutext not providred\n")
		return
	}

	mux := http.NewServeMux()
	mux.Handle("GET /health", healthHandler(bot))
	mux.Handle("POST /sign", signHandler(ctx, bot))
	mux.Handle("/", requestHandler(ctx, bot))
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", bot.BotConfig.HttpPort()), mux))
}

type healthResponse struct {
	Status          string            `json:"status"`
	BlockHeight     uint64            `json:"blockHeight"`
	LastBlockAt     *string           `json:"lastBlockAt"`
	StaleSecs       *int64            `json:"staleSecs,omitempty"`
	PendingSentTxs  int               `json:"pendingSentTxs"`            // txs broadcast but not yet confirmed
	PendingUnsigned int               `json:"pendingUnsigned,omitempty"` // txs awaiting TSS signatures
	FailedVscTxs    []mapper.FailedTx `json:"failedVscTxs,omitempty"`    // VSC txs that reached FAILED status
	Issues          []string          `json:"issues,omitempty"`          // specific problems detected
}

func healthHandler(bot *mapper.Bot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		height, lastAt := bot.LastBlock()

		resp := healthResponse{BlockHeight: height}

		// Count sent-but-unconfirmed txs and pending-unsigned txs
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sentIDs, err := bot.Db.State.GetSentTransactionIDs(ctx)
		if err == nil {
			resp.PendingSentTxs = len(sentIDs)
		}
		pendingHashes, err := bot.Db.State.GetAllPendingSigHashes(ctx)
		if err == nil && len(pendingHashes) > 0 {
			resp.PendingUnsigned = len(pendingHashes)
		}

		var issues []string

		if lastAt.IsZero() {
			resp.Status = "starting"
		} else {
			ts := lastAt.UTC().Format(time.RFC3339)
			resp.LastBlockAt = &ts
			stale := int64(time.Since(lastAt).Seconds())
			staleThreshold := time.Duration(staleBlockMultiplier) * bot.Chain.BlockInterval
			if time.Since(lastAt) > staleThreshold {
				issues = append(issues, fmt.Sprintf("block processing stale for %ds", stale))
				resp.StaleSecs = &stale
			}
		}

		// Flag if sent txs have been hanging too long (>1h)
		if resp.PendingSentTxs > 0 {
			issues = append(issues, fmt.Sprintf("%d txs broadcast but unconfirmed", resp.PendingSentTxs))
		}

		// Flag unsigned txs (waiting on TSS)
		if resp.PendingUnsigned > 0 {
			issues = append(issues, fmt.Sprintf("%d sig hashes awaiting TSS signatures", resp.PendingUnsigned))
		}

		// Flag failed VSC transactions
		failedTxs := bot.FailedTxs()
		if len(failedTxs) > 0 {
			resp.FailedVscTxs = failedTxs
			issues = append(issues, fmt.Sprintf("%d VSC transaction(s) failed", len(failedTxs)))
		}

		if len(issues) > 0 {
			resp.Status = "unhealthy"
			resp.Issues = issues
			w.WriteHeader(http.StatusServiceUnavailable)
		} else if !lastAt.IsZero() {
			resp.Status = "ok"
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

type requestBody struct {
	Instruction string `json:"instruction,omitempty" validate:"required"`
}

func requestHandler(
	globalCtx context.Context,
	bot *mapper.Bot,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// validate incoming request + parse for vsc address
		if r.Method != http.MethodPost {
			writeResponse(w, http.StatusMethodNotAllowed, "only POST allowed")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
		var requestBody requestBody
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&requestBody); err != nil {
			writeResponse(w, http.StatusBadRequest, "invalid request body")
			writeError(err)
			return
		}
		var trailing json.RawMessage
		if err := dec.Decode(&trailing); err == nil || !errors.Is(err, io.EOF) {
			writeResponse(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if err := requestValidator.Struct(&requestBody); err != nil {
			writeResponse(w, http.StatusBadRequest, "invalid request body")
			return
		}

		// fetch public keys from contract state
		ctx, cancel := context.WithTimeout(globalCtx, 15*time.Second)
		defer cancel()

		// primaryKeyHex := bot.BotConfig.PrimaryKey()
		// primaryKey, err := hex.DecodeString(primaryKeyHex)
		// if err != nil {
		// 	writeResponse(w, http.StatusBadRequest, "primary key invalid, please set in "+bot.BotConfig.FilePath())
		// 	return
		// }
		// backupKeyHex := bot.BotConfig.BackupKey()
		// backupKey, err := hex.DecodeString(backupKeyHex)
		// if err != nil {
		// 	writeResponse(w, http.StatusBadRequest, "backup key invalid, please set in "+bot.BotConfig.FilePath())
		// 	return
		// }
		primaryKey, backupKey, err := bot.FetchPublicKeys(ctx)
		if err != nil {
			writeResponse(w, http.StatusInternalServerError, "could not fetch public keys")
			writeError(err)
			return
		}

		// Generate deposit address using the chain-specific address generator
		btcAddr, _, err := bot.Chain.AddressGen.GenerateDepositAddress(
			hex.EncodeToString(primaryKey),
			hex.EncodeToString(backupKey),
			requestBody.Instruction,
		)
		if err != nil {
			writeResponse(w, http.StatusInternalServerError, "")
			writeError(err)
			return
		}

		// insert mapping
		ctx, cancel = context.WithTimeout(globalCtx, 15*time.Second)
		defer cancel()

		if err := bot.Db.Addresses.Insert(ctx, btcAddr, requestBody.Instruction); err != nil {
			if errors.Is(err, database.ErrAddrExists) {
				writeResponse(w, http.StatusOK, "address mapping exists: "+btcAddr+" -> "+requestBody.Instruction)
				return
			} else {
				writeResponse(w, http.StatusInternalServerError, "")
				writeError(err)
			}
		} else {
			writeResponse(w, http.StatusCreated, "address mapping created: "+btcAddr+" -> "+requestBody.Instruction)
		}

		// handle this error, also allows test scripts witout bot
		if bot != nil {
			go bot.HandleExistingTxs(btcAddr)
		} else {
			fmt.Fprintf(os.Stderr, "no mapper state passed to http server, skipping check for previous txs")
		}
	}
}

type signatureEntry struct {
	Index     int    `json:"index"     validate:"min=0"`
	Signature string `json:"signature" validate:"required"`
}

type signRequest struct {
	TxID       string           `json:"tx_id"      validate:"required"`
	Signatures []signatureEntry `json:"signatures" validate:"required,min=1,dive"`
}

func signHandler(
	globalCtx context.Context,
	bot *mapper.Bot,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeResponse(w, http.StatusMethodNotAllowed, "only POST allowed")
			return
		}

		// Auth: require API key in Authorization header
		apiKey := bot.BotConfig.SignApiKey()
		if apiKey == "" {
			writeResponse(w, http.StatusForbidden, "/sign endpoint disabled — set SignApiKey in config")
			return
		}
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer "+apiKey {
			writeResponse(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
		var req signRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeResponse(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		var trailing json.RawMessage
		if err := dec.Decode(&trailing); err == nil || !errors.Is(err, io.EOF) {
			writeResponse(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if err := requestValidator.Struct(&req); err != nil {
			writeResponse(w, http.StatusBadRequest, "tx_id and at least one signature are required")
			return
		}

		ctx, cancel := context.WithTimeout(globalCtx, 15*time.Second)
		defer cancel()

		tx, err := bot.Db.State.GetPendingTransaction(ctx, req.TxID)
		if err != nil {
			writeResponse(w, http.StatusNotFound, fmt.Sprintf("pending transaction not found: %s", req.TxID))
			return
		}

		// Build a signatures map keyed by sighash for each provided index
		sigMap := make(map[string]database.SignatureUpdate)
		for _, entry := range req.Signatures {
			// Validate index bounds (including negative values)
			if entry.Index < 0 || entry.Index >= len(tx.Signatures) {
				writeResponse(
					w,
					http.StatusBadRequest,
					fmt.Sprintf("index %d out of range (0-%d)", entry.Index, len(tx.Signatures)-1),
				)
				return
			}
			sigBytes, err := hex.DecodeString(entry.Signature)
			if err != nil {
				writeResponse(w, http.StatusBadRequest,
					fmt.Sprintf("signature at index %d must be valid hex", entry.Index))
				return
			}
			// Validate ECDSA DER signature length (64-73 bytes)
			if len(sigBytes) < 64 || len(sigBytes) > 73 {
				writeResponse(
					w,
					http.StatusBadRequest,
					fmt.Sprintf(
						"signature at index %d has invalid length %d (expected 64-73 bytes)",
						entry.Index,
						len(sigBytes),
					),
				)
				return
			}
			slot := tx.Signatures[entry.Index]
			sigMap[hex.EncodeToString(slot.SigHash)] = database.SignatureUpdate{Bytes: sigBytes, IsBackup: true}
		}

		fullySigned, err := bot.Db.State.UpdateSignatures(ctx, sigMap)
		if err != nil {
			writeResponse(w, http.StatusInternalServerError, "failed to update signatures")
			writeError(err)
			return
		}

		if len(fullySigned) > 0 {
			writeResponse(w, http.StatusOK,
				fmt.Sprintf("signature applied, transaction %s is now fully signed", req.TxID))
		} else {
			writeResponse(w, http.StatusOK,
				fmt.Sprintf("%d signature(s) applied, transaction %s still awaiting more signatures", len(sigMap), req.TxID))
		}
	}
}

func writeResponse(w http.ResponseWriter, statusCode int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(statusCode)

	if len(msg) == 0 {
		return
	}

	if _, err := w.Write([]byte(msg)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write response: %s\n", err.Error())
		return
	}
}

func writeError(err error) {
	fmt.Fprintf(os.Stderr, "%s\n", err.Error())
}
