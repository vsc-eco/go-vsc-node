package chain

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"

	"vsc-node/lib/btcvault"
)

// solvencyBook is a BTC contract state for the watch: registered UTXOs (id,
// txid byte, vout, amount), reservations, and the relayed height.
type solvencyBook struct {
	keys map[string][]byte
	err  error
}

func newSolvencyBook(relayed uint64) *solvencyBook {
	return &solvencyBook{keys: map[string][]byte{btcvault.BtcRelayedHeightKey: []byte(strconv.FormatUint(relayed, 10))}}
}

func txidOf(b byte) string { return strings.Repeat(hex.EncodeToString([]byte{b}), 32) }

// add registers id with a contract-layout record (as MarshalUtxo writes it).
func (s *solvencyBook) add(id uint16, txByte byte, vout uint32, amount int64) {
	reg := s.keys[btcvault.UtxoRegistryKey]
	e := make([]byte, 8)
	binary.BigEndian.PutUint16(e, id)
	var a [8]byte
	binary.BigEndian.PutUint64(a[:], uint64(amount))
	copy(e[2:], a[2:])
	s.keys[btcvault.UtxoRegistryKey] = append(append([]byte{}, reg...), e...)
	rec := make([]byte, 0, 60)
	txid, _ := hex.DecodeString(txidOf(txByte))
	rec = append(rec, txid...)
	rec = binary.BigEndian.AppendUint32(rec, vout)
	rec = binary.BigEndian.AppendUint64(rec, uint64(amount))
	rec = append(rec, 1, 0x51, 0)               // pkScript OP_TRUE, no tag
	rec = binary.BigEndian.AppendUint32(rec, 0) // generation
	s.keys[btcvault.UtxoKey(id)] = rec
}

func (s *solvencyBook) reserve(id uint16) { s.keys[btcvault.ReservedUtxoKey(id)] = []byte("1") }

func (s *solvencyBook) reader() SolvencyStateReader {
	return func() (func(string) ([]byte, bool, error), error) {
		if s.err != nil {
			return nil, s.err
		}
		snap := make(map[string][]byte, len(s.keys))
		for k, v := range s.keys {
			snap[k] = v
		}
		return func(k string) ([]byte, bool, error) {
			v, ok := snap[k]
			return v, ok, nil
		}, nil
	}
}

type fakeL1 struct {
	tip     uint64
	spent   map[string]bool // "txid:vout"
	err     error
	utxoErr error // Unspent only
	calls   int
	onCall  func() // runs after each Unspent answer (to change the contract meanwhile)
}

func (f *fakeL1) Tip() (uint64, error) { return f.tip, f.err }
func (f *fakeL1) Unspent(txid string, vout uint32) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	if f.utxoErr != nil {
		return false, f.utxoErr
	}
	defer func() {
		if f.onCall != nil {
			f.onCall()
		}
	}()
	return !f.spent[txid+":"+strconv.FormatUint(uint64(vout), 10)], nil
}

func op(b byte, vout uint32) string { return txidOf(b) + ":" + strconv.FormatUint(uint64(vout), 10) }

// N4: the confirmed, unreserved book must be unspent on L1; reserved inputs
// (authorized spends) and the unconfirmed pool are not checked.
func TestN4_ObserveHealthyBookAndTheft(t *testing.T) {
	book := newSolvencyBook(900_000)
	book.add(1024, 0x01, 0, 250_000_000)
	book.add(1025, 0x02, 1, 40_000_000)
	book.add(1026, 0x03, 0, 7_000)
	book.add(5, 0x04, 0, 1_000) // unconfirmed pool: may not exist on L1
	book.add(1027, 0x05, 0, 90_000_000)
	book.reserve(1027) // an unmap's input, already spent on L1
	l1 := &fakeL1{tip: 900_002, spent: map[string]bool{op(0x04, 0): true, op(0x05, 0): true}}

	obs, err := ObserveBtcSolvency(book.reader(), l1, 100_000)
	if err != nil || obs.Insolvent || obs.Missing != 0 || obs.Checked != 3 || obs.Digest != "" {
		t.Fatalf("healthy book: %+v %v (want solvent, 3 checked)", obs, err)
	}

	l1.spent[op(0x01, 0)] = true // a confirmed vault UTXO spent without a reservation
	obs, err = ObserveBtcSolvency(book.reader(), l1, 100_000)
	if err != nil || !obs.Insolvent || obs.Missing != 1 || obs.MissingSats != 250_000_000 || obs.Digest == "" || obs.Tip != 900_002 {
		t.Fatalf("theft: %+v %v", obs, err)
	}
}

// A shortfall inside the gap is reported but not insolvent.
func TestN4_ObserveGap(t *testing.T) {
	book := newSolvencyBook(10)
	book.add(1030, 0x07, 0, 50_000)
	l1 := &fakeL1{tip: 10, spent: map[string]bool{op(0x07, 0): true}}
	obs, err := ObserveBtcSolvency(book.reader(), l1, 100_000)
	if err != nil || obs.Insolvent || obs.Missing != 1 || obs.MissingSats != 50_000 {
		t.Fatalf("inside the gap: %+v %v", obs, err)
	}
	if obs, _ := ObserveBtcSolvency(book.reader(), l1, 49_999); !obs.Insolvent {
		t.Fatalf("over the gap: %+v", obs)
	}
	if obs, _ := ObserveBtcSolvency(book.reader(), l1, 50_000); obs.Insolvent {
		t.Fatalf("exactly the gap is inside it: %+v", obs)
	}
}

// An authorized spend reserved after the first read (and mined during the
// scan) is dropped on the second look; so is an id the contract settled and
// gave to another output meanwhile.
func TestN4_ObserveRechecksAgainstNewerState(t *testing.T) {
	book := newSolvencyBook(10)
	book.add(1040, 0x0a, 0, 300_000_000)
	book.add(1041, 0x0b, 0, 300_000_000)
	l1 := &fakeL1{tip: 11, spent: map[string]bool{op(0x0a, 0): true, op(0x0b, 0): true}}
	l1.onCall = func() {
		book.reserve(1040)                        // unmap built meanwhile
		delete(book.keys, btcvault.UtxoKey(1041)) // settled meanwhile...
		book.add(1041, 0x0c, 2, 300_000_000)      // ...and the id reused
	}
	obs, err := ObserveBtcSolvency(book.reader(), l1, 100_000)
	if err != nil || obs.Insolvent || obs.Missing != 0 {
		t.Fatalf("newer state explains both: %+v %v", obs, err)
	}
}

// Anything unreadable, or a bitcoind behind the contract, abstains.
func TestN4_ObserveAbstains(t *testing.T) {
	book := newSolvencyBook(100)
	book.add(1024, 0x01, 0, 250_000_000)
	if _, err := ObserveBtcSolvency(book.reader(), &fakeL1{tip: 99}, 0); err == nil || !strings.Contains(err.Error(), "behind") {
		t.Fatalf("bitcoind behind the contract: %v", err)
	}
	if _, err := ObserveBtcSolvency(book.reader(), &fakeL1{tip: 100, err: errors.New("refused")}, 0); err == nil {
		t.Fatal("an RPC error did not abstain")
	}
	if _, err := ObserveBtcSolvency(book.reader(), &fakeL1{tip: 100, utxoErr: errors.New("timeout")}, 0); err == nil {
		t.Fatal("a gettxout error was taken as unspent instead of abstaining")
	}
	noH := newSolvencyBook(0)
	delete(noH.keys, btcvault.BtcRelayedHeightKey)
	if _, err := ObserveBtcSolvency(noH.reader(), &fakeL1{tip: 100}, 0); err == nil {
		t.Fatal("no relayed height did not abstain")
	}
	book.err = errors.New("mongo down")
	if _, err := ObserveBtcSolvency(book.reader(), &fakeL1{tip: 100}, 0); err == nil {
		t.Fatal("a state error did not abstain")
	}
	book.err = nil
	book.keys[btcvault.UtxoRegistryKey] = []byte{1, 2, 3}
	if _, err := ObserveBtcSolvency(book.reader(), &fakeL1{tip: 100}, 0); err == nil {
		t.Fatal("a corrupt registry did not abstain")
	}
}

// Witnesses that see the same missing outpoints report the same digest,
// whatever order the registry lists them in.
func TestN4_ObserveDigestIsOrderFree(t *testing.T) {
	spent := map[string]bool{op(0x01, 0): true, op(0x02, 1): true}
	a, b := newSolvencyBook(1), newSolvencyBook(1)
	a.add(1024, 0x01, 0, 1e8)
	a.add(1025, 0x02, 1, 1e8)
	b.add(1025, 0x02, 1, 1e8)
	b.add(1024, 0x01, 0, 1e8)
	oa, _ := ObserveBtcSolvency(a.reader(), &fakeL1{tip: 1, spent: spent}, 0)
	ob, _ := ObserveBtcSolvency(b.reader(), &fakeL1{tip: 1, spent: spent}, 0)
	if oa.Digest == "" || oa.Digest != ob.Digest || oa.MissingSats != 2e8 {
		t.Fatalf("digests %q %q (%d sats)", oa.Digest, ob.Digest, oa.MissingSats)
	}
}
