package oracle

import "testing"

func TestWitnessProductionWindow(t *testing.T) {
	w := NewWitnessProductionWindow(5)
	for i := 0; i < 10; i++ {
		w.PushBlock([]string{"alice"})
	}
	if w.BlocksProducedBy("alice") != 5 {
		t.Fatalf("want 5 got %d", w.BlocksProducedBy("alice"))
	}
	w.PushBlock([]string{"bob"})
	if w.BlocksProducedBy("alice") != 4 {
		t.Fatalf("alice want 4 got %d", w.BlocksProducedBy("alice"))
	}
	if w.BlocksProducedBy("bob") != 1 {
		t.Fatalf("bob")
	}
}

func TestFeedTrust(t *testing.T) {
	if FeedTrust(3, true, 4) {
		t.Fatal("3 produced should not trust")
	}
	if !FeedTrust(4, true, 4) {
		t.Fatal("4 produced + update should trust")
	}
	if FeedTrust(4, false, 4) {
		t.Fatal("no update should not trust")
	}
}

// TestTrustedHivePriceBps_SimpleMeanForSmallN covers the n<4 path where
// the interquartile trim drops 0 and reduces to a simple mean across all
// trusted contributors.
func TestTrustedHivePriceBps_SimpleMeanForSmallN(t *testing.T) {
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
	// Two trusted contributors → trim=floor(2/4)=0 → mean of both.
	// Mean of 2500 and 2700 = 2600 bps.
	want := int64(2600)
	if got != want {
		t.Fatalf("want %d got %d", want, got)
	}
}

// TestTrustedHivePriceBps_InterquartileTrim_DropsExtremes verifies the
// outlier-suppression property: with a cluster of trusted contributors,
// extreme values at the top and bottom are excluded from the mean. With
// 8 contributors, trim=floor(8/4)=2 → middle 4 values are averaged.
func TestTrustedHivePriceBps_InterquartileTrim_DropsExtremes(t *testing.T) {
	// 8 prices: 1000, 2000, 2400, 2500, 2600, 2700, 8000, 9000 bps.
	// Trim = 2 from each end → keep {2400, 2500, 2600, 2700}.
	// Mean = (2400+2500+2600+2700)/4 = 2550.
	q := map[string]Quote{
		"a": {HbdRaw: 100, HiveRaw: 1000},  // 1000 bps  (dropped low)
		"b": {HbdRaw: 200, HiveRaw: 1000},  // 2000 bps  (dropped low)
		"c": {HbdRaw: 240, HiveRaw: 1000},  // 2400 bps
		"d": {HbdRaw: 250, HiveRaw: 1000},  // 2500 bps
		"e": {HbdRaw: 260, HiveRaw: 1000},  // 2600 bps
		"f": {HbdRaw: 270, HiveRaw: 1000},  // 2700 bps
		"g": {HbdRaw: 800, HiveRaw: 1000},  // 8000 bps  (dropped high)
		"h": {HbdRaw: 900, HiveRaw: 1000},  // 9000 bps  (dropped high)
	}
	tr := map[string]bool{
		"a": true, "b": true, "c": true, "d": true,
		"e": true, "f": true, "g": true, "h": true,
	}
	got, ok := TrustedHivePriceBps(q, tr)
	if !ok {
		t.Fatal("expected ok")
	}
	if got != 2550 {
		t.Fatalf("interquartile mean: want 2550 got %d", got)
	}
}

// TestTrustedHivePriceBps_InterquartileTrim_TwentyContributors covers the
// pendulum-spec sizing: 20 trusted contributors → trim 5 from each end →
// mean of the middle 10. A coalition pushing 4 outliers off one side no
// longer moves the result, since they're trimmed before the mean.
func TestTrustedHivePriceBps_InterquartileTrim_TwentyContributors(t *testing.T) {
	q := make(map[string]Quote, 20)
	tr := make(map[string]bool, 20)
	// Construct 20 contributors with prices 100, 200, ..., 2000 bps.
	for i := 1; i <= 20; i++ {
		name := string(rune('a' - 1 + i))
		if i > 26 {
			break
		}
		q[name] = Quote{HbdRaw: int64(i) * 10, HiveRaw: 1000}
		tr[name] = true
	}
	// Wait — need 20 unique names. Use double-letter for >20.
	q = map[string]Quote{}
	tr = map[string]bool{}
	for i := 1; i <= 20; i++ {
		name := string(rune('a'-1+((i-1)/26))) + string(rune('a'+((i-1)%26)))
		q[name] = Quote{HbdRaw: int64(i) * 10, HiveRaw: 1000}
		tr[name] = true
	}
	// Prices ascending: 100, 200, ..., 2000 bps.
	// Trim 5 from each end → keep i=6..15 → prices 600..1500.
	// Mean = (600+700+...+1500)/10 = 10500/10 = 1050.
	got, ok := TrustedHivePriceBps(q, tr)
	if !ok {
		t.Fatal("expected ok")
	}
	if got != 1050 {
		t.Fatalf("interquartile mean of 20: want 1050 got %d", got)
	}
}

// TestTrustedHivePriceBps_FourContributors covers the boundary where
// trimming first kicks in: trim=floor(4/4)=1 → middle 2 averaged.
func TestTrustedHivePriceBps_FourContributors(t *testing.T) {
	q := map[string]Quote{
		"a": {HbdRaw: 100, HiveRaw: 1000}, // 1000 bps (dropped low)
		"b": {HbdRaw: 250, HiveRaw: 1000}, // 2500 bps
		"c": {HbdRaw: 270, HiveRaw: 1000}, // 2700 bps
		"d": {HbdRaw: 900, HiveRaw: 1000}, // 9000 bps (dropped high)
	}
	tr := map[string]bool{"a": true, "b": true, "c": true, "d": true}
	got, ok := TrustedHivePriceBps(q, tr)
	if !ok {
		t.Fatal("ok")
	}
	// Mean of 2500 and 2700 = 2600.
	if got != 2600 {
		t.Fatalf("want 2600 got %d", got)
	}
}
