package main

import (
	"context"
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

func mapBotHttpServer(
	ctx context.Context,
	addressStore *database.AddressStore,
	port int,
	bot *mapper.MapperState,
) {
	if addressStore == nil {
		fmt.Fprintf(os.Stderr, "datastore or mutext not providred\n")
		return
	}

	http.Handle("/", requestHandler(ctx, addressStore, bot))
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
}

type requestBody struct {
	Instruction string `json:"instruction,omitempty" validate:"required"`
}

func requestHandler(
	globalCtx context.Context,
	addressStore *database.AddressStore,
	bot *mapper.MapperState,
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

		vscAddr := requestBody.Instruction

		// make btc address
		btcAddr, err := makeBtcAddress(requestBody.Instruction)
		if err != nil {
			writeResponse(w, http.StatusInternalServerError, "")
			writeError(err)
			return
		}

		// insert mapping
		ctx, cancel := context.WithTimeout(globalCtx, 15*time.Second)
		defer cancel()

		if err := addressStore.Insert(ctx, btcAddr, vscAddr); err != nil {
			if errors.Is(err, database.ErrAddrExists) {
				writeResponse(w, http.StatusConflict, "address map exists")
			} else {
				writeResponse(w, http.StatusInternalServerError, "")
				writeError(err)
			}
		} else {
			writeResponse(w, http.StatusCreated, "address mapping created")
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
