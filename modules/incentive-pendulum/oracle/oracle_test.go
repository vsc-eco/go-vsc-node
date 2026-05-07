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

func TestTrustedHivePriceBps(t *testing.T) {
	// HBD/HIVE precision = 3, so 0.250 HBD = 250 raw, 1.000 HIVE = 1000 raw.
	// Quote.PriceBps() = HbdRaw * BpsScale / HiveRaw.
	q := map[string]Quote{
		"a": {HbdRaw: 250, HiveRaw: 1000},
		"b": {HbdRaw: 270, HiveRaw: 1000},
		"c": {HbdRaw: 500, HiveRaw: 1000}, // ignored: not trusted
	}
	tr := map[string]bool{"a": true, "b": true, "c": false}
	got, ok := TrustedHivePriceBps(q, tr)
	if !ok {
		t.Fatal("ok")
	}
	// Mean of 2500 bps and 2700 bps = 2600 bps.
	want := int64(2600)
	if got != want {
		t.Fatalf("want %d got %d", want, got)
	}
}
