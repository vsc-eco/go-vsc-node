package tss

import (
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"slices"
	"sort"
	"strings"

	"vsc-node/modules/common"
	"vsc-node/modules/common/consensusversion"
	tss_helpers "vsc-node/modules/tss/helpers"

	"github.com/multiformats/go-multicodec"
	blsu "github.com/protolambda/bls12-381-util"
)

// POA-8 (consensus 0.9.0): per-accused reshare statements.
//
// A failed reshare used to produce one blame commitment naming the node's whole
// culprit set. Two honest nodes rarely name the same set (one a round behind
// names everybody it still waits on), so the commitment never reached 2/3 of
// the BLS weight, and from 0.8.0 the timeout set is withheld altogether (B3a).
// A seat that attests ready and then goes silent therefore stalled rotation for
// as long as it liked.
//
// Here every node also signs one statement per party it is still waiting on
// ("S: A did not deliver"), and the leader asks about every party of the
// session (accuseAsksOf), not only the ones it waited on itself. A statement lands on its own 2/3 quorum, so the
// honest laggard's signature still counts for the real withholder while its
// extra names stay far below quorum (Chainflip tallies per accused the same
// way).
//
// What a landed statement means, and what it does not: more than 2/3 of the
// committee could not get A's messages in S. It is NOT proof that A misbehaved.
// btss rounds wait on every party, so a withholder that starves one honest node
// H in round 1 makes everyone else wait on H in round 2, and H gets named.
// Chainflip avoids that with an echo stage Magi does not have. And every party
// left out of a reshare lowers the new key's threshold (it is derived from the
// committee size), so exclusions an attacker can steer must not add up: framing
// six of nineteen would let nine colluders sign alone. Hence:
//   - a statement only leaves A out of the next reshare of the same key, until
//     that rotation succeeds;
//   - at most ONE party is left out per reshare (accuseMaxExclusions): the same
//     exposure as one honest node being offline at a rotation, and it does not
//     build up, because the next rotation draws its committee from the full
//     election again;
//   - btss error culprits are never accused (old party 0's round-1 message is
//     the SSID reference, so a bad party 0 gets party 1 blamed by everyone);
//   - it is its own commitment type: blame scoring, bans, the 33% blame rule,
//     signing and reward reductions never read it.
const accuseTypeReshare = "reshare_accuse"

// accuseMaxExclusions caps how many accused parties one reshare leaves out.
const accuseMaxExclusions = 1

// accusePerOp bounds the accusations one broadcast carries, in a single extra
// custom_json op in its own transaction: Hive caps a custom_json at 8192 bytes
// (an entry is ~450) and an account at 5 custom_json ops per block, so the
// leader adds one op per broadcast, and a refused accusation transaction never
// takes the session commitments with it. Accusations beyond it are dropped;
// the party is named again the next time a session fails.
const accusePerOp = 10

// accuseSigKey routes an accusation's BLS signatures apart from the session's
// own commitment (sigChannels, ask_sigs/res_sig).
func accuseSigKey(sessionId, accused string) string {
	if accused == "" {
		return sessionId
	}
	return sessionId + "|" + accused
}

// accusedSet is the sorted, de-duplicated set a node accuses in a failed
// session: the parties its new party still waits on, never itself.
func accusedSet(self string, waiting []string) []string {
	seen := make(map[string]bool, len(waiting))
	out := make([]string, 0, len(waiting))
	for _, a := range waiting {
		if a == "" || a == self || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// accusedIfActive is accusedSet behind the 0.9.0 gate (nil below it, so a
// failed reshare carries no accusations and nothing below changes).
func accusedIfActive(active consensusversion.Version, self string, waiting []string) []string {
	if !consensusversion.TssPerAccusedBlameActive(active) {
		return nil
	}
	return accusedSet(self, waiting)
}

// accuseRecord is what a failed reshare result carries for its statements.
type accuseRecord struct {
	SessionId   string
	KeyId       string
	BlockHeight uint64
	NewEpoch    uint64
	OldEpoch    uint64
	Accused     []string
}

// accuseRecordOf returns the accusation data of a stored session result, if it
// is a failed reshare that carries any.
func accuseRecordOf(result DispatcherResult) (accuseRecord, bool) {
	rec, ok := accuseSessionOf(result)
	if !ok || len(rec.Accused) == 0 {
		return accuseRecord{}, false
	}
	return rec, true
}

// accuseSessionOf returns the session fields of a stored result if it is a
// failed reshare, whatever this node accused (the leader asks about every
// party, see accuseAsksOf).
func accuseSessionOf(result DispatcherResult) (accuseRecord, bool) {
	var rec accuseRecord
	switch r := result.(type) {
	case TimeoutResult:
		rec = accuseRecord{r.SessionId, r.KeyId, r.BlockHeight, r.Epoch, r.OldEpoch, r.Accused}
	case ErrorResult:
		rec = accuseRecord{r.SessionId, r.KeyId, r.BlockHeight, r.Epoch, r.OldEpoch, r.Accused}
	default:
		return accuseRecord{}, false
	}
	if !strings.HasPrefix(rec.SessionId, sessionPrefixReshare) {
		return accuseRecord{}, false
	}
	return rec, true
}

// accuseStatement is the statement a node SIGNS: only for a party it accuses
// itself in that session.
func (tssMgr *TssManager) accuseStatement(rec accuseRecord, accused string) (tss_helpers.BaseCommitment, bool) {
	if !slices.Contains(rec.Accused, accused) {
		return tss_helpers.BaseCommitment{}, false
	}
	return tssMgr.buildAccuseStatement(rec, accused)
}

// buildAccuseStatement builds the statement for one accused party. Every input
// is fixed by the session (id, key, height, epochs) or on-chain (the election
// that encodes the bit), so every node that names the same party in the same
// session builds the identical commitment and CID. The bit is encoded against
// the session's new election when the accused is in it, else against the old
// commitment's election (an old key holder that is no longer elected); a party
// in neither cannot be encoded and gets no statement.
func (tssMgr *TssManager) buildAccuseStatement(rec accuseRecord, accused string) (tss_helpers.BaseCommitment, bool) {
	for _, epoch := range []uint64{rec.NewEpoch, rec.OldEpoch} {
		election := tssMgr.electionDb.GetElection(epoch)
		if election == nil {
			continue
		}
		for idx, m := range election.Members {
			if m.Account != accused {
				continue
			}
			bits := new(big.Int).SetBit(new(big.Int), idx, 1)
			return tss_helpers.BaseCommitment{
				Type:        accuseTypeReshare,
				SessionId:   rec.SessionId,
				KeyId:       rec.KeyId,
				Commitment:  base64.RawURLEncoding.EncodeToString(bits.Bytes()),
				BlockHeight: rec.BlockHeight,
				Epoch:       epoch,
			}, true
		}
	}
	return tss_helpers.BaseCommitment{}, false
}

// reshareAccusedCounts reads the landed statements for keyId after the key's
// last keygen/reshare commitment (so they lapse once a rotation succeeds) and
// within the blame window, and counts them per account. Each row is decoded
// against its own epoch's election.
func (tssMgr *TssManager) reshareAccusedCounts(keyId string, bh, lastCommitHeight, expireBlock uint64) map[string]int {
	from := lastCommitHeight
	if expireBlock > from {
		from = expireBlock
	}
	rows, err := tssMgr.tssCommitments.FindCommitmentsSimple(&keyId, []string{accuseTypeReshare}, nil, &from, &bh, BLAME_WINDOW_MAX_ROWS)
	if err != nil {
		log.Warn("failed to fetch reshare accusations", "keyId", keyId, "err", err)
	}
	counts := make(map[string]int)
	for _, row := range rows {
		election := tssMgr.electionDb.GetElection(row.Epoch)
		if election == nil || election.Members == nil {
			continue
		}
		raw, derr := base64.RawURLEncoding.DecodeString(row.Commitment)
		if derr != nil {
			continue
		}
		bits := new(big.Int).SetBytes(raw)
		for idx, m := range election.Members {
			if bits.Bit(idx) == 1 {
				counts[m.Account]++
			}
		}
	}
	return counts
}

// selectAccusedExclusions picks the accused party to leave out of the next
// reshare: most-named first, ties by account, at most accuseMaxExclusions minus
// the new-committee members blame or ban already left out (alreadyOut), so
// POA-8 never adds to another exclusion path, and never an old-committee member
// when that would leave fewer than oldMin (oldMembers is the old committee that
// would run: after the blame, ban and readiness filters), so an exclusion never
// starves a session that could otherwise run. When the most-named party is
// such an old member, nobody is left out (it never falls through to a
// less-named party). Deterministic for identical inputs.
//
// Limits, by design: one exclusion per reshare means two parties that attest
// and then withhold keep every session failing (the exclusion alternates
// between them). And any blamed or banned member of the current election
// switches exclusions off (alreadyOut >= 1), for every key while a ban lasts,
// so a long ban also turns withholder recovery off.
func selectAccusedExclusions(counts map[string]int, oldMembers []string, oldMin, alreadyOut int) map[string]bool {
	inOld := make(map[string]bool, len(oldMembers))
	for _, m := range oldMembers {
		inOld[m] = true
	}
	order := make([]string, 0, len(counts))
	for a, c := range counts {
		if c > 0 {
			order = append(order, a)
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if counts[order[i]] != counts[order[j]] {
			return counts[order[i]] > counts[order[j]]
		}
		return order[i] < order[j]
	})
	slots := len(oldMembers) - oldMin
	out := make(map[string]bool)
	for _, a := range order {
		if len(out)+alreadyOut >= accuseMaxExclusions {
			break
		}
		if inOld[a] {
			if slots <= 0 {
				// The most-named party cannot be left out, so this session
				// most likely fails anyway. Leaving out a less-named party
				// instead (usually an honest node everyone waited on behind
				// the withholder) would only lower the new key's threshold
				// if it succeeded. Leave nobody out; the counts decide again
				// at the next session.
				break
			}
			slots--
		}
		out[a] = true
	}
	return out
}

// accuseCommit is a commitment awaiting its BLS quorum, with the accused party
// that routes it ("" for a session's own commitment).
type accuseCommit struct {
	commitment tss_helpers.BaseCommitment
	accused    string
}

// accuseAsksOf lists the statements the leader collects for a failed reshare:
// one per party of the session (old and new committee, never itself), not only
// the parties its own node was waiting on. A withholder that sends its round-1
// messages only to the leader (predictable from the schedule) leaves the
// leader waiting on everyone else, so the leader alone would never ask about
// the withholder the others all name; and a leader outside the new committee
// names nobody. Signers still sign only the parties they accuse themselves
// (ask_sigs handler), so the statement CID and the 2/3 rule are unchanged.
// Asking about a party nobody names costs one ask and a collection that times
// out, in parallel with the others. Empty below 0.9.0.
func (tssMgr *TssManager) accuseAsksOf(active consensusversion.Version, result DispatcherResult, parties []string) []accuseCommit {
	if !consensusversion.TssPerAccusedBlameActive(active) {
		return nil
	}
	rec, ok := accuseSessionOf(result)
	if !ok {
		return nil
	}
	named := accusedSet(tssMgr.config.Get().HiveUsername, parties)
	out := make([]accuseCommit, 0, len(named))
	for _, a := range named {
		if stmt, ok := tssMgr.buildAccuseStatement(rec, a); ok {
			out = append(out, accuseCommit{commitment: stmt, accused: a})
		}
	}
	return out
}

// leaderAccuseAsks is accuseAsksOf for the leader only: no other node
// collects statements, so the others skip the election reads.
func (tssMgr *TssManager) leaderAccuseAsks(isLeader bool, active consensusversion.Version, result DispatcherResult, dsc Dispatcher) []accuseCommit {
	if !isLeader {
		return nil
	}
	return tssMgr.accuseAsksOf(active, result, reshareParties(dsc))
}

// reshareParties is every account of a reshare session: old committee that
// runs, then new committee.
func reshareParties(dsc Dispatcher) []string {
	rd, ok := dsc.(*ReshareDispatcher)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(rd.participants)+len(rd.newParticipants))
	for _, p := range rd.participants {
		out = append(out, p.Account)
	}
	for _, p := range rd.newParticipants {
		out = append(out, p.Account)
	}
	return out
}

// bestAccusePerSession picks the one statement per session the leader
// publishes (a row is keyed by key, height and type, so a second one in the
// same bundle would overwrite the first): the most signed weight, ties by the
// smaller key. weights maps a routing key to its signed weight.
func bestAccusePerSession(sessionOf map[string]string, weights map[string]uint64) map[string]string {
	best := make(map[string]string)
	for key, sid := range sessionOf {
		prev, ok := best[sid]
		if !ok || weights[key] > weights[prev] || (weights[key] == weights[prev] && key < prev) {
			best[sid] = key
		}
	}
	return best
}

// circuitWeight is the election weight of the signers in a BLS bit vector.
func circuitWeight(bv string, weights []uint64) uint64 {
	raw, err := base64.RawURLEncoding.DecodeString(bv)
	if err != nil {
		return 0
	}
	bits := new(big.Int).SetBytes(raw)
	total := uint64(0)
	for i, w := range weights {
		if bits.Bit(i) == 1 {
			total += w
		}
	}
	return total
}

// blsSignCommitment signs a commitment's CID with this node's consensus BLS key,
// exactly as the ask_sigs handler signs a session's own commitment.
func (tssMgr *TssManager) blsSignCommitment(c tss_helpers.BaseCommitment) (string, bool) {
	raw, err := common.EncodeDagCbor(c)
	if err != nil {
		return "", false
	}
	commitCid, err := common.HashBytes(raw, multicodec.DagCbor)
	if err != nil {
		return "", false
	}
	seed, err := hex.DecodeString(tssMgr.config.Get().BlsPrivKeySeed)
	if err != nil || len(seed) != 32 {
		return "", false
	}
	var arr [32]byte
	copy(arr[:], seed)
	var sk blsu.SecretKey
	if err := sk.Deserialize(&arr); err != nil {
		return "", false
	}
	sig := blsu.Sign(&sk, commitCid.Bytes()).Serialize()
	return base64.URLEncoding.EncodeToString(sig[:]), true
}

// applyAccusedExclusions leaves the selected accused parties out of both
// reshare lists (the old committee that would run and the new committee).
func applyAccusedExclusions(oldList, newList []Participant, counts map[string]int, fullOldSize, alreadyOut int) (keepOld, keepNew []Participant, out map[string]bool) {
	oldAccounts := make([]string, 0, len(oldList))
	for _, p := range oldList {
		oldAccounts = append(oldAccounts, p.Account)
	}
	oldThreshold, _ := tss_helpers.GetThreshold(fullOldSize)
	out = selectAccusedExclusions(counts, oldAccounts, oldThreshold+1, alreadyOut)
	keepOld = make([]Participant, 0, len(oldList))
	for _, p := range oldList {
		if !out[p.Account] {
			keepOld = append(keepOld, p)
		}
	}
	keepNew = make([]Participant, 0, len(newList))
	for _, p := range newList {
		if !out[p.Account] {
			keepNew = append(keepNew, p)
		}
	}
	return keepOld, keepNew, out
}

// commitmentOpPackets splits one leader broadcast into custom_json payloads,
// each sent in its own transaction: the session commitments exactly as before
// (omitted when empty), then at most accusePerOp POA-8 statements.
func commitmentOpPackets(sessions, accusations []map[string]any) [][]map[string]any {
	out := make([][]map[string]any, 0, 2)
	if len(sessions) > 0 {
		out = append(out, sessions)
	}
	if len(accusations) > 0 {
		out = append(out, accusations[:min(accusePerOp, len(accusations))])
	}
	return out
}
