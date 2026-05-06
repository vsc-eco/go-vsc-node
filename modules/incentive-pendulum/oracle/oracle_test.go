package oracle

import "testing"

func TestWitnessSignatureWindow(t *testing.T) {
	w := NewWitnessSignatureWindow(5)
	for i := 0; i < 10; i++ {
		w.PushBlock([]string{"alice"})
	}
	if w.SignatureCount("alice") != 5 {
		t.Fatalf("want 5 got %d", w.SignatureCount("alice"))
	}
	w.PushBlock([]string{"bob"})
	if w.SignatureCount("alice") != 4 {
		t.Fatalf("alice want 4 got %d", w.SignatureCount("alice"))
	}
	if w.SignatureCount("bob") != 1 {
		t.Fatalf("bob")
	}
}

func TestFeedTrust(t *testing.T) {
	if FeedTrust(3, true, 4) {
		t.Fatal("3 sigs should not trust")
	}
	if !FeedTrust(4, true, 4) {
		t.Fatal("4 sigs + update should trust")
	}
	if FeedTrust(4, false, 4) {
		t.Fatal("no update should not trust")
	}
}

func TestTrustedHivePriceSQ64(t *testing.T) {
	// HBD/HIVE precision = 3, so 0.250 HBD = 250 raw, 1.000 HIVE = 1000 raw.
	// Quote.ToSQ64() = HbdRaw * SQ64Scale / HiveRaw.
	q := map[string]Quote{
		"a": {HbdRaw: 250, HiveRaw: 1000},
		"b": {HbdRaw: 270, HiveRaw: 1000},
		"c": {HbdRaw: 500, HiveRaw: 1000}, // ignored: not trusted
	}
	tr := map[string]bool{"a": true, "b": true, "c": false}
	got, ok := TrustedHivePriceSQ64(q, tr)
	if !ok {
		t.Fatal("ok")
	}
	// Mean of SQ64(0.250) and SQ64(0.270) = (25_000_000 + 27_000_000) / 2.
	wantSum := q["a"].ToSQ64() + q["b"].ToSQ64()
	want := wantSum / 2
	if got != want {
		t.Fatalf("want %d got %d", want, got)
	}
}
