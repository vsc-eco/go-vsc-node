package mapper

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"testing"
	"time"

	"github.com/hasura/go-graphql-client"
)

// const graphQLUrl = "http://0.0.0.0:8080"

func TestSignatures(t *testing.T) {
	// Create a custom HTTP client with logging
	httpClient := &http.Client{
		Transport: &loggingTransport{http.DefaultTransport},
	}

	cx := graphql.NewClient(graphQLUrl, httpClient)

	msgHex := []string{}

	t.Log("FetchSignatures")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := FetchSignatures(ctx, cx, msgHex)
	if err != nil {
		t.Fatal(err)
	}

	t.Log(result)
}

func TestTxSpends(t *testing.T) {
	slog.SetLogLoggerLevel(slog.LevelDebug)
	httpClient := &http.Client{
		Transport: &loggingTransport{http.DefaultTransport},
	}

	cx := graphql.NewClient(graphQLUrl, httpClient)

	/*
		t.Log("FetchContractData")
		r, d, err := FetchContractData(cx)
		t.Log(r, d, err)
	*/

	t.Log("FetchTxSpends")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := FetchTxSpends(ctx, cx)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("result", result)
}

func TestLastHeight(t *testing.T) {
	httpClient := &http.Client{
		Transport: &loggingTransport{http.DefaultTransport},
	}

	cx := graphql.NewClient(graphQLUrl, httpClient)

	t.Log("FetchTxSpends")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := FetchLastHeight(ctx, cx)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("result", result)
}

// client := graphql.NewClient("https://your-api-endpoint.com/graphql", httpClient)

// Logging transport
type loggingTransport struct {
	transport http.RoundTripper
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reqDump, _ := httputil.DumpRequestOut(req, true)
	fmt.Printf("Request:\n%s\n\n", reqDump)

	resp, err := t.transport.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	respDump, _ := httputil.DumpResponse(resp, true)
	fmt.Printf("Response:\n%s\n\n", respDump)

	return resp, err
}
