package oracle

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/hive_blocks"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	// DefaultTickIntervalBlocks aligns trusted price + APR recompute with the 100-block witness window.
	DefaultTickIntervalBlocks = 100
	defaultMovingAvgTicks     = 3

	// WarmupDistanceBlocks is the minimum span of replayed Hive blocks
	// required to fully populate the rolling signature window (100 blocks)
	// AND the moving-average ring (3 ticks × 100 blocks = 300 blocks).
	// 400 leaves a one-tick buffer so the explicit warmup pass crosses at
	// least three tick boundaries even if the head sits mid-tick.
	WarmupDistanceBlocks = 400

	// bsonFieldPrefix matches hive_blocks.makeBSONCompatible nested-array encoding.
	bsonFieldPrefix = "69ba102f-c815-4ce9-8022-90e520fe8516_"
)

// WarmupSource is the minimal hive_blocks reader the FeedTracker needs to
// replay the rolling-state-defining blocks at startup. Implementations must
// return blocks deterministically given the same range parameters; production
// is satisfied directly by hive_blocks.HiveBlocks.
type WarmupSource interface {
	GetLastProcessedBlock() (uint64, error)
	FetchStoredBlocks(startBlock uint64, endBlock uint64) ([]hive_blocks.HiveBlock, error)
}

// FeedTickSnapshot is the integer-typed pendulum oracle view (after a tick).
type FeedTickSnapshot struct {
	TickBlockHeight uint64

	// HBD per HIVE in basis points (10_000 = 1.0).
	TrustedHivePriceBps int64
	TrustedHiveOK       bool

	HiveMovingAvgBps int64
	HiveMovingAvgOK  bool

	HBDInterestRateBps int
	HBDInterestRateOK  bool

	TrustedWitnessGroup []string
}

// FeedTracker ingests Hive L1 blocks: witness producer schedule, feed_publish (HIVE/HBD),
// and witness_set_properties (hbd_interest_rate). Every DefaultTickIntervalBlocks it
// recomputes the trusted interquartile-mean HIVE price and mode HBD APR from the top
// trusted witnesses.
type FeedTracker struct {
	mu sync.Mutex

	win *WitnessProductionWindow

	quotes       map[string]Quote
	lastFeedBlk  map[string]uint64
	witnessProps map[string]WitnessProperties
	seenWitness  map[string]struct{}

	ma *MovingAverageRing

	last FeedTickSnapshot

	// warmedExplicit flips true after Warmup() completes a successful
	// replay. Allows callers to gate on Warmed() before warmup has had a
	// chance to fill the rolling state organically (the natural-fill path
	// of Warmed() — full signature window + full MA ring — also satisfies
	// the gate but requires ~300 blocks of live operation).
	warmedExplicit bool
}

// NewFeedTracker builds a tracker with a 100-block signature window and a short MA over ticks.
func NewFeedTracker() *FeedTracker {
	return &FeedTracker{
		win:          NewWitnessProductionWindow(100),
		quotes:       make(map[string]Quote),
		lastFeedBlk:  make(map[string]uint64),
		witnessProps: make(map[string]WitnessProperties),
		seenWitness:  make(map[string]struct{}),
		ma:           NewMovingAverageRing(defaultMovingAvgTicks),
	}
}

// LastTick returns a copy of the most recent tick snapshot (may be zero if no tick yet).
func (t *FeedTracker) LastTick() FeedTickSnapshot {
	if t == nil {
		return FeedTickSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.last
	if len(t.last.TrustedWitnessGroup) > 0 {
		out.TrustedWitnessGroup = append([]string(nil), t.last.TrustedWitnessGroup...)
	}
	return out
}

// Warmed reports whether the tracker's in-memory state has reached a
// long-running peer's steady state. Returns true when either:
//
//   - Warmup() has completed an explicit replay, OR
//   - the rolling signature window is full AND the moving-average ring is
//     full (i.e., enough live blocks have flowed through ProcessBlock to
//     fill both organically).
//
// Consumers that read tracker outputs in consensus paths (the swap-time
// applier, contract env keys) MUST gate on this — a partial signature
// window or partial MA ring produces values that diverge from peers running
// since genesis, causing the same swap to compute different geometry across
// nodes and forking the chain.
func (t *FeedTracker) Warmed() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.warmedExplicit {
		return true
	}
	winFull := t.win != nil && t.win.BlocksRecorded() >= t.win.Width
	maFull := t.ma != nil && t.ma.IsFull()
	return winFull && maFull
}

// Warmup replays the last WarmupDistanceBlocks of stored Hive blocks through
// the ingest path so the tracker's in-memory state matches a long-running
// peer at the same head before any consensus-bound caller (swap applier,
// contract env) reads from it. Marks the tracker explicitly warmed on
// completion.
//
// Idempotent — re-invoking is a no-op once Warmed() returns true. Safe to
// call before the block consumer starts streaming new blocks; the consumer
// resumes from GetLastProcessedBlock()+1 so there's no double-ingest of the
// replayed range.
func (t *FeedTracker) Warmup(src WarmupSource) error {
	if t == nil || src == nil {
		return nil
	}
	if t.Warmed() {
		return nil
	}
	head, err := src.GetLastProcessedBlock()
	if err != nil {
		return err
	}
	if head == 0 {
		// Fresh chain or fresh node — nothing to replay. Mark warmed so
		// genesis-era consumers don't block forever waiting for the
		// rolling state to fill from prior history that doesn't exist.
		t.mu.Lock()
		t.warmedExplicit = true
		t.mu.Unlock()
		return nil
	}
	var start uint64 = 1
	if head > WarmupDistanceBlocks {
		start = head - WarmupDistanceBlocks + 1
	}
	blocks, err := src.FetchStoredBlocks(start, head)
	if err != nil {
		return err
	}
	for i := range blocks {
		blk := &blocks[i]
		if blk.Witness != "" {
			t.RecordWitnessBlock(blk.Witness)
		}
		for _, tx := range blk.Transactions {
			t.IngestTransactionOps(blk.BlockNumber, tx)
		}
		t.TickIfDue(blk.BlockNumber)
	}
	t.mu.Lock()
	t.warmedExplicit = true
	t.mu.Unlock()
	return nil
}

// RecordWitnessBlock records the Hive L1 block producer for the rolling signature window.
func (t *FeedTracker) RecordWitnessBlock(witness string) {
	if t == nil || witness == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seenWitness[witness] = struct{}{}
	t.win.PushBlock([]string{witness})
}

// IngestTransactionOps scans all operations in a Hive transaction for feed + witness props.
func (t *FeedTracker) IngestTransactionOps(blockHeight uint64, tx hive_blocks.Tx) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, op := range tx.Operations {
		switch normalizeOpType(op.Type) {
		case "feed_publish":
			t.ingestFeedPublish(blockHeight, op.Value)
		case "witness_set_properties":
			t.ingestWitnessSetProperties(op.Value)
		}
	}
}

// TickIfDue runs the trusted-price + APR aggregation when blockHeight is a
// multiple of the tick interval. The trusted mean and the MA over recent
// trusted means are both computed deterministically from the per-witness
// rational quotes — bit-equal across nodes.
func (t *FeedTracker) TickIfDue(blockHeight uint64) {
	if t == nil || blockHeight == 0 || blockHeight%DefaultTickIntervalBlocks != 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	width := t.win.Width
	if width < 1 {
		width = 100
	}

	// Evict feed entries that have aged out of the trust window. The trust
	// check below uses `lastFeedBlk + width > blockHeight`; entries failing
	// that predicate can't contribute to this tick or any future tick
	// without a fresh feed_publish (which re-populates the maps), so
	// holding them is pure overhead. Bounds the maps by the actively-
	// publishing witness count.
	for w, last := range t.lastFeedBlk {
		if last+uint64(width) <= blockHeight {
			delete(t.quotes, w)
			delete(t.lastFeedBlk, w)
			delete(t.witnessProps, w)
			delete(t.seenWitness, w)
		}
	}

	trusted := make(map[string]bool)
	blocksProduced := make(map[string]int)
	for w := range t.quotes {
		blocksProduced[w] = t.win.BlocksProducedBy(w)
		published := t.lastFeedBlk[w] > 0 && t.lastFeedBlk[w]+uint64(width) > blockHeight
		trusted[w] = FeedTrust(blocksProduced[w], published, DefaultMinBlocksProduced)
	}

	meanBps, meanOk := TrustedHivePriceBps(t.quotes, trusted)
	if meanOk {
		t.ma.Push(meanBps)
	}
	maBps, maOk := t.ma.Mean()

	group := RunningWitnessGroup(trusted, blocksProduced, DefaultWitnessGroupSize)
	apr, aprOk := HBDAPRModeFromGroup(group, t.witnessProps)

	t.last = FeedTickSnapshot{
		TickBlockHeight:     blockHeight,
		TrustedHivePriceBps: meanBps,
		TrustedHiveOK:       meanOk,
		HiveMovingAvgBps:    maBps,
		HiveMovingAvgOK:     maOk,
		HBDInterestRateBps:  apr,
		HBDInterestRateOK:   aprOk,
		TrustedWitnessGroup: append([]string(nil), group...),
	}
}

// witnessIsActive returns true iff the account has produced at least one
// L1 block inside the rolling production window. Used as the ingest gate
// for both feed_publish and witness_set_properties so non-witness spam
// can't grow the in-memory maps.
//
// Hive's top-20 witness rotation guarantees ~5 productions per 100-block
// window for active witnesses, so requiring just 1 production is permissive
// enough that newly-promoted witnesses are accepted as soon as they enter
// rotation, while non-witnesses (who can't produce L1 blocks at all) are
// rejected. RecordWitnessBlock fires before IngestTransactionOps for the
// same Hive block (state_engine.ProcessBlock), so a witness publishing
// alongside their own production passes the gate even on the very first
// block they produce.
func (t *FeedTracker) witnessIsActive(account string) bool {
	if t == nil || t.win == nil {
		return false
	}
	return t.win.BlocksProducedBy(account) >= 1
}

func (t *FeedTracker) ingestFeedPublish(blockHeight uint64, value map[string]interface{}) {
	pub, _ := value["publisher"].(string)
	if pub == "" {
		return
	}
	if !t.witnessIsActive(pub) {
		return
	}
	er, _ := value["exchange_rate"].(map[string]interface{})
	if er == nil {
		return
	}
	base, _ := asString(er["base"])
	quote, _ := asString(er["quote"])
	q, ok := hiveHBDPerHiveFromFeed(base, quote)
	if !ok {
		return
	}
	t.quotes[pub] = q
	t.lastFeedBlk[pub] = blockHeight
	t.seenWitness[pub] = struct{}{}
}

func (t *FeedTracker) ingestWitnessSetProperties(value map[string]interface{}) {
	owner, _ := value["owner"].(string)
	if owner == "" {
		return
	}
	if !t.witnessIsActive(owner) {
		return
	}
	propsVal, ok := value["props"]
	if !ok || propsVal == nil {
		return
	}
	rate, ok := hbdInterestFromProps(propsVal)
	if !ok {
		return
	}
	wp := t.witnessProps[owner]
	wp.HBDInterestRateBps = rate
	t.witnessProps[owner] = wp
	t.seenWitness[owner] = struct{}{}
}

func normalizeOpType(opType string) string {
	return strings.TrimSuffix(strings.TrimSpace(opType), "_operation")
}

func asString(v interface{}) (string, bool) {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x), x != ""
	case fmt.Stringer:
		s := strings.TrimSpace(x.String())
		return s, s != ""
	default:
		s := strings.TrimSpace(fmt.Sprint(x))
		return s, s != ""
	}
}

// hiveHBDPerHiveFromFeed accepts a feed_publish exchange_rate's base+quote in
// either order and returns the rational HBD-per-HIVE Quote.
func hiveHBDPerHiveFromFeed(base, quote string) (Quote, bool) {
	if q, ok := parseHbdPerHivePair(base, quote); ok {
		return q, true
	}
	return parseHbdPerHivePair(quote, base)
}

// parseHbdPerHivePair returns the rational Quote {HbdRaw, HiveRaw} given the
// "X HBD" and "Y HIVE" asset strings — base-unit integers, no float intermediate.
func parseHbdPerHivePair(a, b string) (Quote, bool) {
	va, ca, okA := parseAssetAmount(a)
	vb, cb, okB := parseAssetAmount(b)
	if !okA || !okB {
		return Quote{}, false
	}
	if ca == "hbd" && cb == "hive" && vb > 0 {
		return Quote{HbdRaw: va, HiveRaw: vb}, true
	}
	return Quote{}, false
}

// parseAssetAmount parses a Hive asset string ("0.250 HBD" / "1.000 HIVE") into
// base-unit raw int64 + canonical lowercase symbol. Hive's legacy STEEM symbol
// is normalized to "hive". The amount goes through the precision registry, so
// inputs that exceed an asset's L1 precision (e.g. "1.0001 HBD") fail loudly
// rather than silently truncating.
func parseAssetAmount(s string) (amount int64, sym string, ok bool) {
	s = strings.TrimSpace(s)
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0, "", false
	}
	sym = strings.ToLower(strings.TrimSpace(parts[1]))
	if sym == "steem" {
		sym = "hive"
	}
	amt, err := common.ParseAssetAmount(parts[0], sym)
	if err != nil || amt <= 0 {
		return 0, "", false
	}
	return amt, sym, true
}

func hbdInterestFromProps(props interface{}) (int, bool) {
	pairs := flattenPropsPairs(props)
	for _, p := range pairs {
		if strings.EqualFold(strings.TrimSpace(p[0]), "hbd_interest_rate") {
			return parseIntProp(p[1])
		}
	}
	return 0, false
}

func parseIntProp(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return v, true
}

// flattenPropsPairs normalizes Hive/BSON prop encodings into [key,value] pairs.
func flattenPropsPairs(props interface{}) [][2]string {
	var out [][2]string
	switch x := props.(type) {
	case []interface{}:
		for _, elem := range x {
			if k, v, ok := pairFromUnknown(elem); ok {
				out = append(out, [2]string{k, v})
			}
		}
	case primitive.A:
		return flattenPropsPairs([]interface{}(x))
	case map[string]interface{}:
		if isBSONFieldKeyedMap(x) {
			for i := 0; ; i++ {
				key := fmt.Sprintf("%s%d", bsonFieldPrefix, i)
				inner, ok := x[key]
				if !ok {
					break
				}
				if k, v, ok2 := pairFromUnknown(inner); ok2 {
					out = append(out, [2]string{k, v})
				}
			}
		}
	}
	return out
}

func isBSONFieldKeyedMap(m map[string]interface{}) bool {
	_, ok := m[bsonFieldPrefix+"0"]
	return ok
}

func pairFromUnknown(elem interface{}) (key, val string, ok bool) {
	switch e := elem.(type) {
	case []interface{}:
		if len(e) < 2 {
			return "", "", false
		}
		k, ok1 := asString(e[0])
		v, ok2 := asString(e[1])
		return k, v, ok1 && ok2
	case primitive.A:
		return pairFromUnknown([]interface{}(e))
	case map[string]interface{}:
		return pairFromBSONPairMap(e)
	default:
		return "", "", false
	}
}

func pairFromBSONPairMap(m map[string]interface{}) (k, v string, ok bool) {
	if !isBSONFieldKeyedMap(m) {
		return "", "", false
	}
	k0, ok0 := m[bsonFieldPrefix+"0"]
	k1, ok1 := m[bsonFieldPrefix+"1"]
	if !ok0 || !ok1 {
		return "", "", false
	}
	ks, okK := asString(k0)
	vs, okV := asString(k1)
	return ks, vs, okK && okV
}
