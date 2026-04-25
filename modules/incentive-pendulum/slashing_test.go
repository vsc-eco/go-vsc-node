package pendulum

import "testing"

func TestSlashDefaultsCompliantIsZero(t *testing.T) {
	p := DefaultSlashParams()
	e := OracleEvidence{Signatures: p.MinSignatures, UpdatedFeed: true, Equivocated: false}
	if got := SlashBps(p, e); got != 0 {
		t.Fatalf("got %d want 0", got)
	}
}

func TestSlashMissingSignaturesOnly(t *testing.T) {
	p := DefaultSlashParams()
	e := OracleEvidence{Signatures: 2, UpdatedFeed: true, Equivocated: false}
	// deficit 2 * 25 bps
	if got := SlashBps(p, e); got != 50 {
		t.Fatalf("got %d want 50", got)
	}
}

func TestSlashMissingUpdateOnly(t *testing.T) {
	p := DefaultSlashParams()
	e := OracleEvidence{Signatures: p.MinSignatures, UpdatedFeed: false, Equivocated: false}
	if got := SlashBps(p, e); got != 50 {
		t.Fatalf("got %d want 50", got)
	}
}

func TestSlashEquivocationOnly(t *testing.T) {
	p := DefaultSlashParams()
	e := OracleEvidence{Signatures: p.MinSignatures, UpdatedFeed: true, Equivocated: true}
	if got := SlashBps(p, e); got != 500 {
		t.Fatalf("got %d want 500", got)
	}
}

func TestSlashCombined(t *testing.T) {
	p := DefaultSlashParams()
	e := OracleEvidence{Signatures: 0, UpdatedFeed: false, Equivocated: true}
	// 4*25 + 50 + 500 = 650
	if got := SlashBps(p, e); got != 650 {
		t.Fatalf("got %d want 650", got)
	}
}

func TestSlashCap(t *testing.T) {
	p := DefaultSlashParams()
	p.CapBps = 100
	e := OracleEvidence{Signatures: 0, UpdatedFeed: false, Equivocated: true}
	if got := SlashBps(p, e); got != 100 {
		t.Fatalf("got %d want 100", got)
	}
}

func TestSignatureDeficitFloorsAtZero(t *testing.T) {
	p := DefaultSlashParams()
	e := OracleEvidence{Signatures: p.MinSignatures + 10, UpdatedFeed: true, Equivocated: false}
	if got := SignatureDeficit(p, e); got != 0 {
		t.Fatalf("got %d want 0", got)
	}
}

func TestSlashNoCapWhenCapNonPositive(t *testing.T) {
	p := DefaultSlashParams()
	p.CapBps = 0
	e := OracleEvidence{Signatures: 0, UpdatedFeed: false, Equivocated: true}
	// 4*25 + 50 + 500 = 650, no cap when CapBps <= 0
	if got := SlashBps(p, e); got != 650 {
		t.Fatalf("got %d want 650", got)
	}
}
