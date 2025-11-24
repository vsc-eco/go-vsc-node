package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/stretchr/testify/assert"
)

func TestHttpServer(t *testing.T) {
	const dbName = "mappingbottest"
	ctx := context.TODO()
	db, err := database.New(ctx, "mongodb://localhost:27017", dbName, "address_mappings")
	if err != nil {
		t.Fatal(err)
	}

	defer db.Close(ctx)
	defer func() {
		if err := db.DropDatabase(ctx); err != nil {
			t.Logf("failed to drop test database: %s", err.Error())
		}
	}()

	requestBody := requestBody{
		Instruction: "deposit_to=hive:sudo-sandwich",
	}

	btcAddr, err := makeBtcAddress(requestBody.Instruction)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("201 created", func(t *testing.T) {
		buf := &bytes.Buffer{}
		if err := json.NewEncoder(buf).Encode(&requestBody); err != nil {
			t.Fatal(err)
		}

		req, err := http.NewRequest(http.MethodPost, "/", buf)
		if err != nil {
			t.Fatal(err)
		}
		w := &httptest.ResponseRecorder{}

		handler := requestHandler(t.Context(), db).ServeHTTP
		handler(w, req)

		result := w.Result()
		assert.Equal(t, http.StatusCreated, result.StatusCode)

		instruction, err := db.GetInstruction(t.Context(), btcAddr)
		if err != nil {
			t.Fatal(err)
		}

		assert.Equal(t, requestBody.Instruction, instruction)
	})

	t.Run("409 conflict", func(t *testing.T) {
		buf := &bytes.Buffer{}
		if err := json.NewEncoder(buf).Encode(&requestBody); err != nil {
			t.Fatal(err)
		}

		req, err := http.NewRequest(http.MethodPost, "/", buf)
		if err != nil {
			t.Fatal(err)
		}
		w := &httptest.ResponseRecorder{}

		handler := requestHandler(t.Context(), db).ServeHTTP
		handler(w, req)

		result := w.Result()
		assert.Equal(t, http.StatusConflict, result.StatusCode)
	})

	t.Run("400 bad request", func(t *testing.T) {
		body := map[string]string{}
		buf := &bytes.Buffer{}
		if err := json.NewEncoder(buf).Encode(&body); err != nil {
			t.Fatal(err)
		}

		req, err := http.NewRequest(http.MethodPost, "/", buf)
		if err != nil {
			t.Fatal(err)
		}
		w := &httptest.ResponseRecorder{}

		handler := requestHandler(t.Context(), db).ServeHTTP
		handler(w, req)

		result := w.Result()
		assert.Equal(t, http.StatusBadRequest, result.StatusCode)
	})
}
