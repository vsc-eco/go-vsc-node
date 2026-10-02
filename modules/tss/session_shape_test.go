package tss

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
)

// lastBlameCommitments serves GetCommitmentByHeight("blame") from one stored
// commitment, the only read the 0.3.0 keygen-retry rule makes.
type lastBlameCommitments struct {
	tss_db.TssCommitments
	last tss_db.TssCommitment
	err  error
}

func (c *lastBlameCommitments) GetCommitmentByHeight(string, uint64, ...string) (tss_db.TssCommitment, error) {
	return c.last, c.err
}

func blameBits(idxs ...int) string {
	b := new(big.Int)
	for _, i := range idxs {
		b.SetBit(b, i, 1)
	}
	return base64.RawURLEncoding.EncodeToString(b.Bytes())
}

func electionOf(accounts ...string) elections.ElectionResult {
	var e elections.ElectionResult
	for _, a := range accounts {
		e.Members = append(e.Members, elections.ElectionMember{Account: a})
	}
	return e
}

// The session shapes follow the chain-active version: off at mainnet's 0.3.0,
// on from 0.7.0 (testnet's floor), and off without a scheduler.
func TestSessionShape_FollowsTheActiveVersion(t *testing.T) {
	for _, tc := range []struct {
		ver  consensusversion.Version
		want bool
	}{
		{consensusversion.Version{}, false},
		{consensusversion.V0_3_0, false},
		{consensusversion.Version{Major: 0, Consensus: 6}, false},
		{consensusversion.V0_7_0, true},
		{consensusversion.V0_8_0, true},
		{consensusversion.V0_9_0, true},
	} {
		mgr := &TssManager{scheduler: &fakeSolvencyScheduler{minVer: tc.ver}}
		if got := mgr.sessionShapeActive(100); got != tc.want {
			t.Errorf("active %v: sessionShapeActive = %v, want %v", tc.ver, got, tc.want)
		}
	}
	if (&TssManager{}).sessionShapeActive(100) {
		t.Error("no scheduler: want the 0.3.0 shapes")
	}
}

// Below the line the blame-window reads take the 0.3.0 build's 100 rows.
func TestSessionShape_BlameWindowRows(t *testing.T) {
	if got := blameWindowRows(false); got != 100 {
		t.Errorf("0.3.0 shapes: rows = %d, want 100", got)
	}
	if got := blameWindowRows(true); got != BLAME_WINDOW_MAX_ROWS {
		t.Errorf("new shapes: rows = %d, want %d", got, BLAME_WINDOW_MAX_ROWS)
	}
}

// The 0.3.0 keygen-retry rule: every member the key's most recent unexpired blame
// names, decoded against the current election, and nobody without one.
func TestSessionShape_LegacyKeygenBlamed(t *testing.T) {
	const bh = 100_000
	elec := electionOf("a", "b", "c", "d")
	for _, tc := range []struct {
		name string
		c    *lastBlameCommitments
		want []string
	}{
		{"no blame", &lastBlameCommitments{err: errors.New("not found")}, nil},
		{"expired", &lastBlameCommitments{last: tss_db.TssCommitment{BlockHeight: bh - BLAME_EXPIRE, Commitment: blameBits(1)}}, nil},
		{"one named", &lastBlameCommitments{last: tss_db.TssCommitment{BlockHeight: bh - 1, Commitment: blameBits(1)}}, []string{"b"}},
		{"two named", &lastBlameCommitments{last: tss_db.TssCommitment{BlockHeight: bh - BLAME_EXPIRE + 1, Commitment: blameBits(0, 3)}}, []string{"a", "d"}},
	} {
		mgr := &TssManager{tssCommitments: tc.c}
		got := mgr.legacyKeygenBlamed("k", bh, elec)
		if len(got) != len(tc.want) {
			t.Errorf("%s: blamed %v, want %v", tc.name, got, tc.want)
			continue
		}
		for _, a := range tc.want {
			if !got[a] {
				t.Errorf("%s: blamed %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}
