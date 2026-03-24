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

func TestTrustedHivePrice(t *testing.T) {
	q := map[string]float64{"a": 0.25, "b": 0.27, "c": 0.5}
	tr := map[string]bool{"a": true, "b": true, "c": false}
	p, ok := TrustedHivePrice(q, tr)
	if !ok {
		t.Fatal("ok")
	}
	want := (0.25 + 0.27) / 2
	if p != want {
		t.Fatalf("want %v got %v", want, p)
	}
}
