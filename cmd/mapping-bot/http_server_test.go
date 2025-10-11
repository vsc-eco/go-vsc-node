package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"vsc-node/cmd/mapping-bot/database"

	"github.com/stretchr/testify/assert"
)

func TestHttpServer(t *testing.T) {
	const path = "/tmp/vscdb"
	db, err := database.New(path)
	if err != nil {
		t.Fatal(err)
	}

	defer os.RemoveAll(path)

	requestBody := requestBody{
		VscAddr: "deposit_to=sudo-sandwich",
	}

	btcAddr, err := makeBtcAddress(requestBody.VscAddr)
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

		vscAddr, err := db.GetVscAddress(t.Context(), btcAddr)
		if err != nil {
			t.Fatal(err)
		}

		assert.Equal(t, requestBody.VscAddr, vscAddr)
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
