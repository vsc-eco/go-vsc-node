package tss

// TSS-FRAME-1. Some tss-lib checks name, as the culprit, a party that did not
// author the bad data: the data being compared came from ANOTHER party (a
// reference message, or whichever party's value was seen first). One lying
// party can then make every honest node name the same honest party, so the
// blame commitment gathers its 2/3 and feeds the per-key blame rule and the
// ban score. These tests drive the real tss-lib v3 rounds in process, on the
// library's own keygen fixtures, with one party lying; nothing is simulated
// except the lie.

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/elections"
	tss_helpers "vsc-node/modules/tss/helpers"

	keyGenSecp256k1 "github.com/bnb-chain/tss-lib/v3/ecdsa/keygen"
	reshareSecp256k1 "github.com/bnb-chain/tss-lib/v3/ecdsa/resharing"
	btss "github.com/bnb-chain/tss-lib/v3/tss"
	"github.com/multiformats/go-multicodec"
	"github.com/stretchr/testify/require"
)

// frameRig runs one tss-lib ceremony in process. Messages are delivered one at
// a time in FIFO order, so a run is deterministic: the same lie produces the
// same errors on every run.
type frameRig struct {
	oldPIDs, newPIDs btss.SortedPartyIDs
	old, new         []btss.Party
	out              chan btss.Message
	endReshare       chan *keyGenSecp256k1.LocalPartySaveData
	finished         int
	// errs holds every btss error, keyed by the Id of the party that raised it.
	errs map[string][]*btss.Error
	// route, when set, may swap the message one destination receives (a liar
	// that is also in the new committee sees its own genuine message).
	route func(m btss.Message, to btss.Party) btss.Message
}

func loadSortedFixtures(t *testing.T, count int) ([]keyGenSecp256k1.LocalPartySaveData, btss.SortedPartyIDs) {
	t.Helper()
	keys, pids, err := keyGenSecp256k1.LoadKeygenTestFixtures(count)
	require.NoError(t, err, "tss-lib ships 5 ECDSA keygen fixtures in its module")
	// Pair each sorted party with the fixture holding its share id: the
	// fixture file order is not guaranteed to be the sorted order.
	sorted := make([]keyGenSecp256k1.LocalPartySaveData, len(pids))
	for j, p := range pids {
		found := false
		for _, k := range keys {
			if k.ShareID.Cmp(p.KeyInt()) == 0 {
				sorted[j] = k
				found = true
				break
			}
		}
		require.True(t, found, "fixture for party %s", p.Id)
	}
	return sorted, pids
}

// newFrameReshare builds a reshare from the 5 fixture parties (key threshold 2)
// to 5 new parties, with the same threshold rule the node uses.
func newFrameReshare(t *testing.T) *frameRig {
	t.Helper()
	oldKeys, fixturePIDs := loadSortedFixtures(t, 5)
	oldIDs := make(btss.UnSortedPartyIDs, len(fixturePIDs))
	for j, p := range fixturePIDs {
		oldIDs[j] = btss.NewPartyID(fmt.Sprintf("old-%d", j), "", p.KeyInt())
	}
	oldPIDs := btss.SortPartyIDs(oldIDs)
	newIDs := make(btss.UnSortedPartyIDs, 5)
	for j := range newIDs {
		newIDs[j] = btss.NewPartyID(fmt.Sprintf("new-%d", j), "", big.NewInt(int64(1000+j)))
	}
	newPIDs := btss.SortPartyIDs(newIDs)
	oldCtx, newCtx := btss.NewPeerContext(oldPIDs), btss.NewPeerContext(newPIDs)
	oldThreshold := 2 // the fixtures were generated at threshold 2 of 5
	newThreshold, _ := tss_helpers.GetThreshold(len(newPIDs))

	r := &frameRig{
		oldPIDs:    oldPIDs,
		newPIDs:    newPIDs,
		out:        make(chan btss.Message, 10000),
		endReshare: make(chan *keyGenSecp256k1.LocalPartySaveData, 100),
		errs:       make(map[string][]*btss.Error),
	}
	for j, pid := range oldPIDs {
		params := btss.NewReSharingParameters(btss.S256(), oldCtx, newCtx, pid, len(oldPIDs), oldThreshold, len(newPIDs), newThreshold)
		r.old = append(r.old, reshareSecp256k1.NewLocalParty(params, oldKeys[j], r.out, r.endReshare))
	}
	for j, pid := range newPIDs {
		params := btss.NewReSharingParameters(btss.S256(), oldCtx, newCtx, pid, len(oldPIDs), oldThreshold, len(newPIDs), newThreshold)
		params.SetNoProofMod() // speed only; the checks under test do not use these proofs
		params.SetNoProofFac()
		save := keyGenSecp256k1.NewLocalPartySaveData(len(newPIDs))
		save.LocalPreParams = oldKeys[j].LocalPreParams
		r.new = append(r.new, reshareSecp256k1.NewLocalParty(params, save, r.out, r.endReshare))
	}
	return r
}

// newFrameKeygen builds a 5-party ECDSA keygen reusing the fixture pre-params.
func newFrameKeygen(t *testing.T) (*frameRig, chan *keyGenSecp256k1.LocalPartySaveData) {
	t.Helper()
	fixtures, fixturePIDs := loadSortedFixtures(t, 5)
	ids := make(btss.UnSortedPartyIDs, len(fixturePIDs))
	for j, p := range fixturePIDs {
		ids[j] = btss.NewPartyID(fmt.Sprintf("kg-%d", j), "", p.KeyInt())
	}
	pids := btss.SortPartyIDs(ids)
	ctx := btss.NewPeerContext(pids)
	threshold, _ := tss_helpers.GetThreshold(len(pids))
	end := make(chan *keyGenSecp256k1.LocalPartySaveData, 100)
	r := &frameRig{newPIDs: pids, out: make(chan btss.Message, 10000), errs: make(map[string][]*btss.Error)}
	for j, pid := range pids {
		params := btss.NewParameters(btss.S256(), ctx, pid, len(pids), threshold)
		params.SetNoProofMod()
		params.SetNoProofFac()
		r.new = append(r.new, keyGenSecp256k1.NewLocalParty(params, r.out, end, fixtures[j].LocalPreParams))
	}
	return r, end
}

// tamperFn inspects a message about to be delivered and returns what is
// actually delivered in its place (the message itself when honest).
type tamperFn func(msg btss.Message) []btss.Message

func honest(msg btss.Message) []btss.Message { return []btss.Message{msg} }

func (r *frameRig) deliver(t *testing.T, p btss.Party, msg btss.Message) {
	if p.PartyID() == msg.GetFrom() {
		return
	}
	if r.route != nil {
		msg = r.route(msg, p)
	}
	bz, _, err := msg.WireBytes()
	require.NoError(t, err)
	parsed, err := btss.ParseWireMessage(bz, msg.GetFrom(), msg.IsBroadcast())
	require.NoError(t, err)
	if _, tErr := p.Update(parsed); tErr != nil {
		r.errs[p.PartyID().Id] = append(r.errs[p.PartyID().Id], tErr)
	}
}

// run starts every party and delivers until nothing is left in flight.
func (r *frameRig) run(t *testing.T, tamper tamperFn) {
	t.Helper()
	for _, p := range append(append([]btss.Party{}, r.new...), r.old...) {
		if err := p.Start(); err != nil {
			r.errs[p.PartyID().Id] = append(r.errs[p.PartyID().Id], err)
		}
	}
	isReshare := len(r.old) > 0
	for {
		select {
		case msg := <-r.out:
			for _, m := range tamper(msg) {
				to := m.GetTo()
				if !isReshare {
					if to == nil {
						for _, p := range r.new {
							r.deliver(t, p, m)
						}
					} else {
						for _, d := range to {
							r.deliver(t, r.new[d.Index], m)
						}
					}
					continue
				}
				// Same routing as tss-lib's own resharing test.
				if m.IsToOldCommittee() || m.IsToOldAndNewCommittees() {
					for _, d := range to[:len(r.old)] {
						r.deliver(t, r.old[d.Index], m)
					}
				}
				if !m.IsToOldCommittee() || m.IsToOldAndNewCommittees() {
					for _, d := range to {
						r.deliver(t, r.new[d.Index], m)
					}
				}
			}
		case <-r.endReshare:
			r.finished++
		default:
			return
		}
	}
}

// replaceContent rebuilds msg with new content, keeping its routing.
func replaceContent(t *testing.T, msg btss.Message, content btss.MessageContent) btss.Message {
	t.Helper()
	_, routing, err := msg.WireBytes()
	require.NoError(t, err)
	return btss.NewMessage(*routing, content, btss.NewMessageWrapper(*routing, content))
}

func contentOf(t *testing.T, msg btss.Message) btss.MessageContent {
	t.Helper()
	bz, _, err := msg.WireBytes()
	require.NoError(t, err)
	parsed, err := btss.ParseWireMessage(bz, msg.GetFrom(), msg.IsBroadcast())
	require.NoError(t, err)
	// A fresh parse, so the caller may edit the content freely.
	return parsed.Content()
}

// blameOf runs a party's errors through the node's own recording path and
// returns the culprits its blame commitment would name, decoded against the
// election the commitment is encoded with.
func blameOf(t *testing.T, errs []*btss.Error, dropUnattributable bool, members []string) []string {
	t.Helper()
	const epoch = 7
	mgr := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{epoch: makeElection(epoch, members)},
	}}
	d := &BaseDispatcher{tssMgr: mgr, dropUnattributableCulprits: dropUnattributable}
	for _, e := range errs {
		d.recordTssError(e)
	}
	tssErr, culprits, text := d.snapshotTssError()
	require.NotNil(t, tssErr, "the party saw a btss error")
	res := ErrorResult{tssMgr: mgr, tssErr: tssErr, Culprits: culprits, ErrorText: text, Epoch: epoch}
	return decodeBlameBitset(res.Serialize().Commitment, makeElection(epoch, members).Members)
}

func (r *frameRig) allIds() []string {
	ids := make([]string, 0, len(r.oldPIDs)+len(r.newPIDs))
	for _, p := range r.oldPIDs {
		ids = append(ids, p.Id)
	}
	for _, p := range r.newPIDs {
		ids = append(ids, p.Id)
	}
	return ids
}

// blameTally counts, per culprit set, how many raising parties would sign it.
func (r *frameRig) blameTally(t *testing.T, dropUnattributable bool) map[string]int {
	t.Helper()
	tally := make(map[string]int)
	for _, errs := range r.errs {
		tally[fmt.Sprint(blameOf(t, errs, dropUnattributable, r.allIds()))]++
	}
	return tally
}

func TestTssFrame1_ControlHonestReshareCompletes(t *testing.T) {
	r := newFrameReshare(t)
	r.run(t, honest)
	require.Empty(t, r.errs, "no lie, no error")
	require.Equal(t, len(r.old)+len(r.new), r.finished, "every party finishes: the rig itself is sound")
}

// A lying old party 0 sends a wrong SSID. Round 2 of every new party compares
// each old party's SSID against old party 0's, so the new parties name old
// party 1 (the new party at index 1 skips old index 1 and names old party 2).
func TestTssFrame1_SsidReferenceFramesOldParty1(t *testing.T) {
	r := newFrameReshare(t)
	liar := r.oldPIDs[0]
	r.run(t, func(msg btss.Message) []btss.Message {
		if msg.GetFrom() != liar {
			return []btss.Message{msg}
		}
		c, ok := contentOf(t, msg).(*reshareSecp256k1.DGRound1Message)
		if !ok {
			return []btss.Message{msg}
		}
		c.Ssid = []byte("forged by old party 0")
		return []btss.Message{replaceContent(t, msg, c)}
	})

	require.Len(t, r.errs, len(r.new), "every new party aborts in round 2")
	for _, errs := range r.errs {
		require.Equal(t, "ssid mismatch", errs[0].Cause().Error())
	}
	framed := fmt.Sprint([]string{r.oldPIDs[1].Id})
	legacy := r.blameTally(t, false)
	t.Logf("pre-0.9.0 blame commitments by culprit set: %v (liar %s)", legacy, liar.Id)
	require.Equal(t, 4, legacy[framed], "4 of 5 new parties name the honest old party 1; the liar is never named")
	require.GreaterOrEqual(t, legacy[framed]*3, len(r.new)*2, "4 of 5 clears the 2/3 quorum, so the framed blame lands")

	fixed := r.blameTally(t, true)
	t.Logf("0.9.0 blame commitments by culprit set: %v", fixed)
	require.Equal(t, map[string]int{"[]": len(r.new)}, fixed, "at 0.9.0 nobody is named")
}

// Old party 0 sends its round-1 message twice, the second with another public
// key, timed just before the last old party's message. tss-lib stores the
// replacement (it leaves replay protection to the caller, and the node forwards
// repeats), then compares old party 0's CURRENT key with the one saved from its
// first message and names the party whose message it is processing: the honest
// last sender, on every new party alike.
func TestTssFrame1_PubKeyReplacementFramesNextSender(t *testing.T) {
	r := newFrameReshare(t)
	liar, victim := r.oldPIDs[0], r.oldPIDs[len(r.oldPIDs)-1]
	var liarMsg btss.Message
	r.run(t, func(msg btss.Message) []btss.Message {
		if _, ok := contentOf(t, msg).(*reshareSecp256k1.DGRound1Message); !ok {
			return []btss.Message{msg}
		}
		switch msg.GetFrom() {
		case liar:
			liarMsg = msg
		case victim:
			swapped := contentOf(t, liarMsg).(*reshareSecp256k1.DGRound1Message)
			g := btss.S256().Params()
			swapped.EcdsaPubX, swapped.EcdsaPubY = g.Gx.Bytes(), g.Gy.Bytes()
			return []btss.Message{replaceContent(t, liarMsg, swapped), msg}
		}
		return []btss.Message{msg}
	})

	require.Len(t, r.errs, len(r.new), "every new party aborts in round 1")
	for _, errs := range r.errs {
		require.Equal(t, "ecdsa pub key did not match what we received previously", errs[0].Cause().Error())
	}
	framed := fmt.Sprint([]string{victim.Id})
	legacy := r.blameTally(t, false)
	t.Logf("pre-0.9.0 blame commitments by culprit set: %v (liar %s)", legacy, liar.Id)
	require.Equal(t, map[string]int{framed: len(r.new)}, legacy, "every new party names the honest last sender, one culprit, so the blame lands")

	require.Equal(t, map[string]int{"[]": len(r.new)}, r.blameTally(t, true), "at 0.9.0 nobody is named")
}

// New party 0 copies new party 1's h1 into its own round-2 message (h1 is sent
// in the clear, so a party that waits for the others can copy it). The
// uniqueness check keeps the first value it sees in index order and names the
// later party: the honest one.
func TestTssFrame1_H1CopyFramesTheOriginalOwner_Reshare(t *testing.T) {
	r := newFrameReshare(t)
	liar, victim := r.newPIDs[0], r.newPIDs[1]
	var victimH1 []byte
	var held []btss.Message
	r.run(t, func(msg btss.Message) []btss.Message {
		c, ok := contentOf(t, msg).(*reshareSecp256k1.DGRound2Message1)
		if !ok {
			return []btss.Message{msg}
		}
		switch msg.GetFrom() {
		case victim:
			victimH1 = c.H1
			out := []btss.Message{msg}
			for _, h := range held { // the rushing liar sends once it has seen the victim's value
				lc := contentOf(t, h).(*reshareSecp256k1.DGRound2Message1)
				lc.H1 = victimH1
				out = append(out, replaceContent(t, h, lc))
			}
			held = nil
			return out
		case liar:
			if victimH1 == nil {
				held = append(held, msg)
				return nil
			}
			c.H1 = victimH1
			return []btss.Message{replaceContent(t, msg, c)}
		}
		return []btss.Message{msg}
	})

	named := 0
	for id, errs := range r.errs {
		if id == liar.Id || id == victim.Id {
			continue
		}
		require.Equal(t, "this h1j was already used by another party", errs[0].Cause().Error())
		require.Equal(t, []string{victim.Id}, blameOf(t, errs, false, r.allIds()), "pre-0.9.0 the honest owner of h1 is named")
		require.Empty(t, blameOf(t, errs, true, r.allIds()), "at 0.9.0 nobody is named")
		named++
	}
	require.Equal(t, len(r.new)-2, named, "every other new party names the honest owner")
}

// Same copy in a fresh keygen (the ceremony every vault generation starts
// with): party 0 copies party 1's h1.
func TestTssFrame1_H1CopyFramesTheOriginalOwner_Keygen(t *testing.T) {
	r, _ := newFrameKeygen(t)
	liar, victim := r.newPIDs[0], r.newPIDs[1]
	var victimH1 []byte
	var held []btss.Message
	r.run(t, func(msg btss.Message) []btss.Message {
		c, ok := contentOf(t, msg).(*keyGenSecp256k1.KGRound1Message)
		if !ok {
			return []btss.Message{msg}
		}
		switch msg.GetFrom() {
		case victim:
			victimH1 = c.H1
			out := []btss.Message{msg}
			for _, h := range held {
				lc := contentOf(t, h).(*keyGenSecp256k1.KGRound1Message)
				lc.H1 = victimH1
				out = append(out, replaceContent(t, h, lc))
			}
			held = nil
			return out
		case liar:
			if victimH1 == nil {
				held = append(held, msg)
				return nil
			}
			c.H1 = victimH1
			return []btss.Message{replaceContent(t, msg, c)}
		}
		return []btss.Message{msg}
	})

	ids := make([]string, 0, len(r.newPIDs))
	for _, p := range r.newPIDs {
		ids = append(ids, p.Id)
	}
	named := 0
	for id, errs := range r.errs {
		if id == liar.Id || id == victim.Id {
			continue
		}
		require.Equal(t, "this h1j was already used by another party", errs[0].Cause().Error())
		require.Equal(t, []string{victim.Id}, blameOf(t, errs, false, ids), "pre-0.9.0 the honest owner of h1 is named")
		require.Empty(t, blameOf(t, errs, true, ids), "at 0.9.0 nobody is named")
		named++
	}
	require.Equal(t, len(r.newPIDs)-2, named, "every other party names the honest owner")
}

// Control for the fix: a lie the library CAN attribute is still blamed on the
// liar at 0.9.0. Old party 0 sends a wrong share; each new party verifies that
// share against old party 0's own commitment and names old party 0.
func TestTssFrame1_AttributableLieStillBlamesTheLiar(t *testing.T) {
	r := newFrameReshare(t)
	liar := r.oldPIDs[0]
	r.run(t, func(msg btss.Message) []btss.Message {
		if msg.GetFrom() != liar {
			return []btss.Message{msg}
		}
		c, ok := contentOf(t, msg).(*reshareSecp256k1.DGRound3Message1)
		if !ok {
			return []btss.Message{msg}
		}
		c.Share = big.NewInt(12345).Bytes()
		return []btss.Message{replaceContent(t, msg, c)}
	})

	require.Len(t, r.errs, len(r.new), "every new party rejects the share")
	for _, errs := range r.errs {
		require.Equal(t, "share from old committee did not pass Verify()", errs[0].Cause().Error())
		require.Equal(t, []string{liar.Id}, blameOf(t, errs, false, r.allIds()))
		require.Equal(t, []string{liar.Id}, blameOf(t, errs, true, r.allIds()), "the fix does not blind real blame")
	}
}

// CHECK 4 of the TSS manual: every node must reach the same CID or the blame
// never lands. Replay the SSID lie through the real Done() of each node (the
// five new parties that saw the error, plus an old-only member that only timed
// out). At 0.9.0 every node resolves the same culprit-free blame; below 0.9.0
// the framed commitment is the 4-node majority.
func TestTssFrame1_EveryNodeResolvesTheSameBlame(t *testing.T) {
	r := newFrameReshare(t)
	liar := r.oldPIDs[0]
	r.run(t, func(msg btss.Message) []btss.Message {
		c, ok := contentOf(t, msg).(*reshareSecp256k1.DGRound1Message)
		if !ok || msg.GetFrom() != liar {
			return []btss.Message{msg}
		}
		c.Ssid = []byte("forged by old party 0")
		return []btss.Message{replaceContent(t, msg, c)}
	})
	require.Len(t, r.errs, len(r.new))

	const epoch, bh = 7, 700
	members := r.allIds()
	nodeCid := func(errs []*btss.Error, active consensusversion.Version) (string, []string) {
		mgr := newTestTssManager(t, "observer")
		mgr.electionDb = &test_utils.MockElectionDb{
			Elections: map[uint64]*elections.ElectionResult{epoch: makeElection(epoch, members)},
		}
		mgr.scheduler = &fakeSolvencyScheduler{minVer: active}
		d := &ReshareDispatcher{
			BaseDispatcher: BaseDispatcher{
				tssMgr: mgr, sessionId: "reshare-700-0-k", keyId: "k", blockHeight: bh,
				done:                       make(chan struct{}, 1),
				dropUnattributableCulprits: consensusversion.TssUnattributableCulpritsDroppedActive(active),
			},
			newEpoch: epoch,
		}
		for _, e := range errs {
			d.recordTssError(e)
		}
		d.timeout = true // the aborted session ends on the inactivity timer
		d.done <- struct{}{}
		res, err := d.Done().Await(context.Background())
		require.NoError(t, err)
		bc := (*res).Serialize()
		bz, err := common.EncodeDagCbor(bc)
		require.NoError(t, err)
		c, err := common.HashBytes(bz, multicodec.DagCbor)
		require.NoError(t, err)
		return c.String(), decodeBlameBitset(bc.Commitment, makeElection(epoch, members).Members)
	}

	for _, tc := range []struct {
		name   string
		active consensusversion.Version
	}{{"0.8.0", consensusversion.V0_8_0}, {"0.9.0", consensusversion.V0_9_0}} {
		t.Run(tc.name, func(t *testing.T) {
			cids := map[string][]string{}
			count := map[string]int{}
			for _, errs := range r.errs {
				c, named := nodeCid(errs, tc.active)
				cids[c] = named
				count[c]++
			}
			c, named := nodeCid(nil, tc.active) // old-only member: no btss error, timeout only
			cids[c] = named
			count[c]++
			for c, named := range cids {
				t.Logf("%s: %d of %d nodes sign %s naming %v", tc.name, count[c], len(r.new)+1, c[len(c)-8:], named)
			}
			if tc.active == consensusversion.V0_9_0 {
				require.Len(t, cids, 1, "every node resolves the same commitment")
				for _, named := range cids {
					require.Empty(t, named, "and it names nobody")
				}
				return
			}
			framedCid := ""
			for c, named := range cids {
				if len(named) == 1 && named[0] == r.oldPIDs[1].Id {
					framedCid = c
				}
			}
			require.Equal(t, 4, count[framedCid], "below 0.9.0 four nodes sign the commitment naming honest old party 1")
		})
	}
}

// What the SSID liar can still do at 0.9.0 (measured, not fixed here). On a real
// network the liar is also in the new committee and sees its own genuine
// message, so its new party carries on and sends its round-2 messages, while
// every honest new party aborted in round 2 and sends nothing. When the session
// times out the honest parties are waiting on each other and the liar is waiting
// on all of them, so POA-8 accuses every honest party (n-1 of n accusers each)
// and never the liar. selectAccusedExclusions then leaves out ONE honest party
// (most named, ties by account) from the next reshare of that key: POA-8's
// designed bound, the same exposure as a party that starves one honest node,
// never stacked on blame or ban, gone after the next rotation. Blame names
// nobody (TestTssFrame1_SsidReferenceFramesOldParty1).
func TestTssFrame1_ResidualSsidLieMeetsThePoa8Bound(t *testing.T) {
	r := newFrameReshare(t)
	liarOld, liarNew := r.oldPIDs[0], r.newPIDs[0]
	var genuine btss.Message
	r.run(t, func(msg btss.Message) []btss.Message {
		c, ok := contentOf(t, msg).(*reshareSecp256k1.DGRound1Message)
		if !ok || msg.GetFrom() != liarOld {
			return []btss.Message{msg}
		}
		genuine = msg
		c.Ssid = []byte("forged by old party 0")
		forged := replaceContent(t, msg, c)
		r.route = func(m btss.Message, to btss.Party) btss.Message {
			if m == forged && to.PartyID() == liarNew {
				return genuine
			}
			return m
		}
		return []btss.Message{forged}
	})

	_, liarNewErred := r.errs[liarNew.Id]
	require.False(t, liarNewErred, "the liar's own new party sees the genuine SSID and carries on")
	require.Len(t, r.errs, len(r.new)-1, "every honest new party aborts in round 2")

	counts := map[string]int{}
	for j, p := range r.new {
		waiting := []string{}
		for _, w := range p.WaitingFor() {
			waiting = append(waiting, w.Id)
		}
		accused := accusedSet(r.newPIDs[j].Id, waiting)
		t.Logf("%s accuses %v", r.newPIDs[j].Id, accused)
		for _, a := range accused {
			counts[a]++
		}
	}
	t.Logf("accusations per party: %v", counts)
	require.Zero(t, counts[liarNew.Id], "the liar sent every message, so nobody accuses it")
	for _, p := range r.newPIDs[1:] {
		require.Equal(t, len(r.new)-1, counts[p.Id], "each honest party is accused by every other party")
		require.GreaterOrEqual(t, counts[p.Id]*3, len(r.new)*2, "which clears the 2/3 quorum")
	}
	out := selectAccusedExclusions(counts, nil, 0, 0)
	t.Logf("next reshare leaves out: %v", out)
	require.Len(t, out, accuseMaxExclusions, "POA-8 leaves out one party, never more")
	require.False(t, out[liarNew.Id])
}
