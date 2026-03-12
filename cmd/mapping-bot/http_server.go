package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/cmd/mapping-bot/mapper"

	"github.com/go-playground/validator/v10"
)

var requestValidator = validator.New(validator.WithRequiredStructEnabled())

// staleBlockThreshold is how long without a processed block before health is degraded.
// BTC mainnet averages ~10 min per block; 20 min gives one missed block of headroom.
const staleBlockThreshold = 20 * time.Minute

func mapBotHttpServer(
	ctx context.Context,
	addressStore *database.AddressStore,
	port int,
	bot *mapper.Bot,
) {
	if addressStore == nil {
		fmt.Fprintf(os.Stderr, "datastore or mutext not providred\n")
		return
	}

	mux := http.NewServeMux()
	mux.Handle("GET /health", healthHandler(bot))
	mux.Handle("/", requestHandler(ctx, bot))
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), mux))
}

type healthResponse struct {
	Status      string  `json:"status"`
	BlockHeight uint64  `json:"blockHeight"`
	LastBlockAt *string `json:"lastBlockAt"`
	StaleSecs   *int64  `json:"staleSecs,omitempty"`
}

func healthHandler(bot *mapper.Bot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		height, lastAt := bot.LastBlock()

		resp := healthResponse{BlockHeight: height}

		if lastAt.IsZero() {
			// No block processed yet this session — not necessarily unhealthy,
			// could be a fresh start. Report status but return 200.
			resp.Status = "starting"
		} else {
			ts := lastAt.UTC().Format(time.RFC3339)
			resp.LastBlockAt = &ts
			stale := int64(time.Since(lastAt).Seconds())
			if time.Since(lastAt) > staleBlockThreshold {
				resp.Status = "unhealthy"
				resp.StaleSecs = &stale
				w.WriteHeader(http.StatusServiceUnavailable)
			} else {
				resp.Status = "ok"
			}
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

		var requestBody requestBody
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			writeResponse(w, http.StatusInternalServerError, "")
			writeError(err)
			return
		}

		if err := requestValidator.Struct(&requestBody); err != nil {
			writeResponse(w, http.StatusBadRequest, "invalid request body")
			return
		}

		// fetch public keys from contract state
		ctx, cancel := context.WithTimeout(globalCtx, 15*time.Second)
		defer cancel()

		primaryKeyHex := bot.BotConfig.PrimaryKey()
		primaryKey, err := hex.DecodeString(primaryKeyHex)
		if err != nil {
			writeResponse(w, http.StatusBadRequest, "primary key invalid, please set in "+bot.BotConfig.FilePath())
			return
		}
		backupKeyHex := bot.BotConfig.BackupKey()
		backupKey, err := hex.DecodeString(backupKeyHex)
		if err != nil {
			writeResponse(w, http.StatusBadRequest, "backup key invalid, please set in "+bot.BotConfig.FilePath())
			return
		}

		// make btc address
		tag := sha256.Sum256([]byte(requestBody.Instruction))
		btcAddr, _, err := createP2WSHAddressWithBackup(
			primaryKey,
			backupKey,
			tag[:],
			bot.ChainParams,
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
				writeResponse(w, http.StatusConflict, "address map exists")
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

func writeResponse(w http.ResponseWriter, statusCode int, msg string) {
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
