package tss

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// BridgeClient sends TSS messages to an external connectivity bridge
// service over HTTP. The bridge is responsible for delivering the
// message to the target participant using whatever transports it
// supports (e.g. WebRTC + TURN).
type BridgeClient struct {
	baseURL    string
	httpClient *http.Client
}

type bridgeSendRequest struct {
	// Target Hive account (witness.Account) of the participant.
	ToAccount string `json:"to_account"`

	// Original TSS RPC payload.
	SessionId   string `json:"session_id"`
	IsBroadcast bool   `json:"is_broadcast"`
	Type        string `json:"type"`
	KeyId       string `json:"key_id"`
	Cmt         string `json:"cmt"`
	CmtFrom     string `json:"cmt_from"`
	Data        []byte `json:"data"`
}

// NewBridgeClient constructs a new client. If baseURL is empty, the
// client is effectively disabled and Send will return an error.
func NewBridgeClient(baseURL string) *BridgeClient {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 3 * time.Second,
	}

	return &BridgeClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
		},
	}
}

// Enabled reports whether the bridge client has a usable base URL.
func (c *BridgeClient) Enabled() bool {
	return c != nil && c.baseURL != ""
}

// Send forwards a TSS message to the bridge. This is intended to be
// called as a best-effort fallback when direct libp2p delivery fails.
func (c *BridgeClient) Send(ctx context.Context, toAccount string, msg *TMsg) error {
	if !c.Enabled() {
		return fmt.Errorf("bridge client not configured")
	}

	body := bridgeSendRequest{
		ToAccount:   toAccount,
		SessionId:   msg.SessionId,
		IsBroadcast: msg.IsBroadcast,
		Type:        msg.Type,
		KeyId:       msg.KeyId,
		Cmt:         msg.Cmt,
		CmtFrom:     msg.CmtFrom,
		Data:        msg.Data,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/tss/send", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("bridge returned status %d", resp.StatusCode)
	}
	return nil
}

