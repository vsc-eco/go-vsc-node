package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// This is a minimal bolt-on TSS bridge service intended for testnet
// experiments. For now it only accepts incoming TSS messages over HTTP
// and logs them. The forwarding / NAT traversal layer (e.g. WebRTC +
// TURN) can be added later without touching go-vsc-node.

type bridgeSendRequest struct {
	ToAccount   string `json:"to_account"`
	SessionId   string `json:"session_id"`
	IsBroadcast bool   `json:"is_broadcast"`
	Type        string `json:"type"`
	KeyId       string `json:"key_id"`
	Cmt         string `json:"cmt"`
	CmtFrom     string `json:"cmt_from"`
	Data        []byte `json:"data"`
}

func handleTssSend(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	var req bridgeSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	log.Printf("[tss-bridge] recv msg to=%s session=%s len=%d type=%s cmt=%s from=%s\n",
		req.ToAccount, req.SessionId, len(req.Data), req.Type, req.Cmt, req.CmtFrom)

	// TODO: In a future iteration, look up or establish a transport
	// (e.g. WebRTC + TURN) for req.ToAccount and forward the payload.

	w.WriteHeader(http.StatusNoContent)
}

func main() {
	addr := os.Getenv("TSS_BRIDGE_BIND")
	if addr == "" {
		addr = ":8081"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/tss/send", handleTssSend)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Println("[tss-bridge] listening on", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

