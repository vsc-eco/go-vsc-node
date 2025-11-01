package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type jsonRPCRequest struct {
	JsonRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      int             `json:"id"`
	Params  json.RawMessage `json:"params"`
}

func postRPC(
	ctx context.Context,
	endpoint, method string,
	params []any,
	resultBuf any,
) error {
	// make rpc request
	paramBytes, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("failed to serialize params: %w", err)
	}

	reqBody := jsonRPCRequest{
		JsonRPC: "2.0",
		Method:  method,
		ID:      1,
		Params:  paramBytes,
	}

	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(&reqBody); err != nil {
		return fmt.Errorf("failed to serialize request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, buf)
	if err != nil {
		return fmt.Errorf("failed to create new request: %w", err)
	}
	req.Header.Add("content-type", "application/json")

	// send post request
	httpClient := http.Client{}

	response, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		buf.Reset()
		if _, err := io.Copy(buf, response.Body); err != nil {
			return fmt.Errorf("failed to read response error: %w", err)
		}
		return fmt.Errorf("request failed: %s", buf.String())
	}

	// deserialize json RPC result
	var resBody struct {
		Result json.RawMessage `json:"result"`
	}

	if err := json.NewDecoder(response.Body).Decode(&resBody); err != nil {
		return fmt.Errorf("failed to deserialize response: %w", err)
	}

	if err := json.Unmarshal(resBody.Result, resultBuf); err != nil {
		return fmt.Errorf("failed to deserialize result: %w", err)
	}

	return nil
}
