package state_engine

import (
	"errors"
	"testing"

	"vsc-node/modules/db/vsc/nonces"

	"go.mongodb.org/mongo-driver/mongo"
)

// flakyNonces fails the first `fails` reads with err, then returns rec/recErr.
type flakyNonces struct {
	nonces.Nonces
	fails  int
	err    error
	rec    nonces.NonceRecord
	recErr error
	calls  int
}

func (f *flakyNonces) GetNonce(string) (nonces.NonceRecord, error) {
	f.calls++
	if f.calls <= f.fails {
		return nonces.NonceRecord{}, f.err
	}
	return f.rec, f.recErr
}

// The apply-side nonce rule reads the account nonce through storedNonce. A
// missing record is a real 0; any other read error must never read as 0,
// because a node that did would run a stale transaction its peers skip.
func TestStoredNonce(t *testing.T) {
	t.Run("no record reads as 0", func(t *testing.T) {
		f := &flakyNonces{recErr: mongo.ErrNoDocuments}
		se := &StateEngine{nonceDb: f}
		if got := se.storedNonce("k"); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
		if f.calls != 1 {
			t.Fatalf("calls = %d, want 1", f.calls)
		}
	})
	t.Run("stored value", func(t *testing.T) {
		f := &flakyNonces{rec: nonces.NonceRecord{Nonce: 7}}
		se := &StateEngine{nonceDb: f}
		if got := se.storedNonce("k"); got != 7 {
			t.Fatalf("got %d, want 7", got)
		}
	})
	t.Run("read error retries, never 0", func(t *testing.T) {
		f := &flakyNonces{fails: 2, err: errors.New("connection reset"), rec: nonces.NonceRecord{Nonce: 7}}
		se := &StateEngine{nonceDb: f}
		if got := se.storedNonce("k"); got != 7 {
			t.Fatalf("got %d after transient errors, want 7", got)
		}
		if f.calls != 3 {
			t.Fatalf("calls = %d, want 3 (2 failures + 1 success)", f.calls)
		}
	})
}
