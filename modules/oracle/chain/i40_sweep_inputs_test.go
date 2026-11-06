package chain

import (
	"encoding/binary"
	"encoding/hex"
	"slices"
	"testing"

	"vsc-node/lib/btcvault"
)

// The testnet BTC contract's in-flight gen-13 -> gen-14 sweep on 11-05 (read
// from state at Hive ~7974700): "msl" listed this txid, and its "ms-" record
// spends ids 1131, 1132, 1134, 1135, 1136 (fee 546, successor gen 14, build
// height 5261811, then the successor address). None had an "ru-" mark.
const (
	i40SweepTxid   = "b519c51ef6627ce2bc1baeb12fc5c438b1cb58fb3e6077be7c1058d4f5f93e2d"
	i40SweepRecord = "00000000000002220000000e005049f30005046b046c046e046f04707462317161723770336a3777686c36676a366b3830736e716e6178356a6b727230366c616a6565397a68673766786e637365717a79767871336b656c6d63"
)

var i40SweepIds = []uint16{1131, 1132, 1134, 1135, 1136}

func (s *solvencyBook) sweep(txid string, record []byte) {
	raw, _ := hex.DecodeString(txid)
	s.keys[btcvault.MigrationSweepRegistryKey] = append(append([]byte{}, s.keys[btcvault.MigrationSweepRegistryKey]...), raw...)
	s.keys[btcvault.MigrationSweepKey(txid)] = record
}

// sweepRecord writes an "ms-" record as the contract's MarshalMigrationSweep does.
func sweepRecord(ids ...uint16) []byte {
	b := binary.BigEndian.AppendUint64(nil, 546)
	b = binary.BigEndian.AppendUint32(b, 14)
	b = binary.BigEndian.AppendUint32(b, 100)
	b = binary.BigEndian.AppendUint16(b, uint16(len(ids)))
	for _, id := range ids {
		b = binary.BigEndian.AppendUint16(b, id)
	}
	return append(b, "tb1qsuccessor"...)
}

// The parser reads the contract's real encoding (the live testnet record).
func TestI40_ParsesTheLiveSweepRecord(t *testing.T) {
	rec, _ := hex.DecodeString(i40SweepRecord)
	ids, err := btcvault.MigrationSweepInputIds(rec)
	if err != nil || !slices.Equal(ids, i40SweepIds) {
		t.Fatalf("live record: %v %v, want %v", ids, err, i40SweepIds)
	}
	reg, _ := hex.DecodeString(i40SweepTxid)
	txids, err := btcvault.UnmarshalMigrationSweepRegistry(reg)
	if err != nil || !slices.Equal(txids, []string{i40SweepTxid}) {
		t.Fatalf("live msl: %v %v", txids, err)
	}
	if !slices.Equal(mustIds(t, sweepRecord(i40SweepIds...)), i40SweepIds) {
		t.Fatal("the test encoder disagrees with the live record layout")
	}
}

func mustIds(t *testing.T, rec []byte) []uint16 {
	t.Helper()
	ids, err := btcvault.MigrationSweepInputIds(rec)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// I-40: a mined migration sweep's confirmed inputs are an authorized spend,
// not missing coins; a confirmed UTXO outside the sweep spent beside it is
// still caught.
func TestI40_AMinedMigrationSweepIsNotInsolvency(t *testing.T) {
	book := newSolvencyBook(5_261_811)
	for i, id := range i40SweepIds {
		book.add(id, byte(0x40+i), uint32(i), 67_798)
	}
	book.add(1133, 0x50, 0, 250_000) // the active generation's coin, not in the sweep
	rec, _ := hex.DecodeString(i40SweepRecord)
	book.sweep(i40SweepTxid, rec)
	spent := map[string]bool{}
	for i := range i40SweepIds {
		spent[op(byte(0x40+i), uint32(i))] = true // the sweep is mined
	}
	l1 := &fakeL1{tip: 5_300_204, spent: spent}

	obs, err := ObserveBtcSolvency(book.reader(), l1, 100_000)
	if err != nil || obs.Insolvent || obs.Missing != 0 || obs.Checked != 1 {
		t.Fatalf("mined sweep, nothing else spent: %+v %v (want solvent, only 1133 checked)", obs, err)
	}

	spent[op(0x50, 0)] = true // and a coin the contract never authorized
	obs, err = ObserveBtcSolvency(book.reader(), l1, 100_000)
	if err != nil || !obs.Insolvent || obs.Missing != 1 || obs.MissingSats != 250_000 {
		t.Fatalf("theft beside a sweep: %+v %v (want 1 missing, 250,000 sats)", obs, err)
	}
}

// A sweep built after the watch's first read and mined before its Bitcoin
// query is dropped on the second look, as a reservation made meanwhile is.
func TestI40_ASweepBuiltDuringTheScanIsDropped(t *testing.T) {
	book := newSolvencyBook(10)
	book.add(1200, 0x60, 0, 300_000_000)
	book.add(1201, 0x61, 0, 300_000_000)
	l1 := &fakeL1{tip: 11, spent: map[string]bool{op(0x60, 0): true, op(0x61, 0): true}}
	once := false
	l1.onCall = func() {
		if !once {
			once = true
			book.sweep(txidOf(0x77), sweepRecord(1200, 1201))
		}
	}
	obs, err := ObserveBtcSolvency(book.reader(), l1, 100_000)
	if err != nil || obs.Insolvent || obs.Missing != 0 {
		t.Fatalf("sweep seen on the second look: %+v %v", obs, err)
	}
}

// Malformed sweep state abstains; a listed sweep without a record excludes
// nothing (the spend is then counted, never hidden).
func TestI40_SweepStateEdges(t *testing.T) {
	newBook := func() (*solvencyBook, *fakeL1) {
		b := newSolvencyBook(10)
		b.add(1300, 0x70, 0, 500_000)
		return b, &fakeL1{tip: 10, spent: map[string]bool{op(0x70, 0): true}}
	}

	b, l1 := newBook()
	b.keys[btcvault.MigrationSweepRegistryKey] = make([]byte, 31)
	if _, err := ObserveBtcSolvency(b.reader(), l1, 100_000); err == nil {
		t.Fatal("an msl that is not whole txids must abstain")
	}

	b, l1 = newBook()
	b.sweep(txidOf(0x78), sweepRecord(1300)[:19]) // n=1 but no id
	if _, err := ObserveBtcSolvency(b.reader(), l1, 100_000); err == nil {
		t.Fatal("a truncated sweep record must abstain")
	}

	b, l1 = newBook()
	raw, _ := hex.DecodeString(txidOf(0x79))
	b.keys[btcvault.MigrationSweepRegistryKey] = raw // listed, no "ms-" record
	obs, err := ObserveBtcSolvency(b.reader(), l1, 100_000)
	if err != nil || !obs.Insolvent || obs.Missing != 1 {
		t.Fatalf("a listed sweep without a record must not hide a spend: %+v %v", obs, err)
	}

	b, l1 = newBook()
	b.sweep(txidOf(0x7a), sweepRecord(1301)) // a sweep of another id
	obs, err = ObserveBtcSolvency(b.reader(), l1, 100_000)
	if err != nil || !obs.Insolvent || obs.Missing != 1 {
		t.Fatalf("a sweep excludes only its own inputs: %+v %v", obs, err)
	}
}
