package btcclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAddressBalanceSats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/address/bc1qexample" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// funded 500000, spent 120000 => confirmed balance 380000
		fmt.Fprint(w, `{"address":"bc1qexample","chain_stats":{"funded_txo_sum":500000,"spent_txo_sum":120000},"mempool_stats":{"funded_txo_sum":7,"spent_txo_sum":0}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second)
	bal, err := c.AddressBalanceSats(context.Background(), "bc1qexample")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bal != 380000 {
		t.Fatalf("expected confirmed balance 380000, got %d", bal)
	}
}

func TestAddressBalanceSats_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second)
	if _, err := c.AddressBalanceSats(context.Background(), "bc1qexample"); err == nil {
		t.Fatal("expected error on non-200 status, got nil")
	}
}

func TestAddressBalanceSats_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, `{"chain_stats":{"funded_txo_sum":1,"spent_txo_sum":0}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.AddressBalanceSats(ctx, "bc1qexample"); err == nil {
		t.Fatal("expected context deadline error, got nil")
	}
}
