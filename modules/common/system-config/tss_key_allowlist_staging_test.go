package systemconfig

import (
	"strings"
	"testing"
)

// The allowlist decides which tss.create_key transactions SUCCEED (KEY-SPAM-1 /
// 18j), so widening it is a consensus change. The height argument used to be
// accepted and ignored, which is worse than absent: it reads as staged at the
// call site while a wider list would in fact bind the moment a node took the
// build, and two nodes disagreeing about whether a create_key is valid is a
// fork. These tests pin that an addition is inert below its FromHeight.
func TestTssKeyContractAllowlistStagesByHeight(t *testing.T) {
	c := &config{
		tssKeyContractAllowlist: []string{"vsc1BTC"},
		tssKeyContractAllowlistAdditions: []TssKeyContractAddition{
			{ID: "vsc1EVM", FromHeight: 111_500_000},
		},
	}

	for _, tc := range []struct {
		height uint64
		want   string
	}{
		{0, "vsc1BTC"},
		{111_499_999, "vsc1BTC"},         // one block before: must NOT bind
		{111_500_000, "vsc1BTC,vsc1EVM"}, // the agreed block
		{111_600_000, "vsc1BTC,vsc1EVM"},
	} {
		got := strings.Join(c.TssKeyContractAllowlistAt(tc.height), ",")
		if got != tc.want {
			t.Errorf("at height %d: got %q, want %q", tc.height, got, tc.want)
		}
	}
}

// Empty must stay nil, not an empty slice: both consumers (filterScheduledKeys
// and the TssCreateKey chokepoint) read "no entries" as UNRESTRICTED, and they
// must not be able to disagree about which empty they were handed.
func TestTssKeyContractAllowlistEmptyIsNil(t *testing.T) {
	c := &config{}
	if got := c.TssKeyContractAllowlistAt(999); got != nil {
		t.Errorf("empty config: got %#v, want nil", got)
	}
	// An addition that has not activated yet must not make the list non-empty
	// either — that would flip a test network from unrestricted to restricted
	// with an empty permitted set, refusing every create_key.
	staged := &config{tssKeyContractAllowlistAdditions: []TssKeyContractAddition{{ID: "vsc1EVM", FromHeight: 100}}}
	if got := staged.TssKeyContractAllowlistAt(99); got != nil {
		t.Errorf("pre-activation addition only: got %#v, want nil", got)
	}
	if got := staged.TssKeyContractAllowlistAt(100); len(got) != 1 || got[0] != "vsc1EVM" {
		t.Errorf("at FromHeight: got %#v, want [vsc1EVM]", got)
	}
}

// A contract listed twice (base + addition, or two additions) must appear once:
// the consumers build a set from this, but a duplicate would also mean the
// mainnet list silently depended on iteration order if that ever changed.
func TestTssKeyContractAllowlistDedupes(t *testing.T) {
	c := &config{
		tssKeyContractAllowlist: []string{"vsc1BTC"},
		tssKeyContractAllowlistAdditions: []TssKeyContractAddition{
			{ID: "vsc1BTC", FromHeight: 0},
			{ID: "vsc1EVM", FromHeight: 0},
			{ID: "vsc1EVM", FromHeight: 0},
			{ID: "", FromHeight: 0},
		},
	}
	if got := strings.Join(c.TssKeyContractAllowlistAt(1), ","); got != "vsc1BTC,vsc1EVM" {
		t.Errorf("got %q, want %q", got, "vsc1BTC,vsc1EVM")
	}
}

// Mainnet ships the BTC mapping contract and nothing else. If an EVM bridge is
// ever appended to the BASE list instead of staged, this fails and says why.
func TestMainnetAllowlistIsStillTheBtcContractOnly(t *testing.T) {
	c := MainnetConfig().(*config)
	if len(c.tssKeyContractAllowlist) != 1 || c.tssKeyContractAllowlist[0] != "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d" {
		t.Errorf("mainnet base allowlist = %#v; a new contract belongs in "+
			"tssKeyContractAllowlistAdditions with a FromHeight, not here", c.tssKeyContractAllowlist)
	}
}
