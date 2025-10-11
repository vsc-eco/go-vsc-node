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
)

func mapBotHttpServer(
	ctx context.Context,
	db *database.MappingBotDatabase,
	port int,
) {
	if db == nil {
		fmt.Fprintf(os.Stderr, "datastore or mutext not providred\n")
		return
	}

	http.Handle("/", requestHandler(ctx, db))
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
}

type requestBody struct {
	VscAddr string `json:"vsc_addr"`
}

func requestHandler(
	globalCtx context.Context,
	db *database.MappingBotDatabase,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		btcAddr, err := makeBtcAddress(requestBody.VscAddr)
		if err != nil {
			writeResponse(w, http.StatusInternalServerError, "")
			writeError(err)
			return
		}

		vscAddr := requestBody.VscAddr

		ctx, cancel := context.WithTimeout(globalCtx, 15*time.Second)
		defer cancel()

		if err := db.InsertAddressMap(ctx, btcAddr, vscAddr); err != nil {
			if errors.Is(err, database.ErrAddrExists) {
				writeResponse(w, http.StatusBadRequest, "address map exists")
			} else {
				writeResponse(w, http.StatusInternalServerError, "")
				writeError(err)
			}
		} else {
			writeResponse(w, http.StatusCreated, "address mapping created")
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
