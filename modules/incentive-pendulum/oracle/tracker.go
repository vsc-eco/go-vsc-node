package oracle

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"vsc-node/modules/db/vsc/hive_blocks"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	// DefaultTickIntervalBlocks aligns trusted price + APR recompute with the 100-block witness window.
	DefaultTickIntervalBlocks = 100
	defaultMovingAvgTicks     = 3

	// bsonFieldPrefix matches hive_blocks.makeBSONCompatible nested-array encoding.
	bsonFieldPrefix = "69ba102f-c815-4ce9-8022-90e520fe8516_"
)

// FeedTickSnapshot is the last computed pendulum oracle view (after a tick).
type FeedTickSnapshot struct {
	TickBlockHeight uint64

	TrustedHiveMean float64
	TrustedHiveOK   bool

	HiveMovingAvg   float64
	HiveMovingAvgOK bool

	HBDInterestRateBps int
	HBDInterestRateOK  bool

	TrustedWitnessGroup []string
	WitnessSlashBps     map[string]int
}

// FeedTracker ingests Hive L1 blocks: witness producer schedule, feed_publish (HIVE/HBD),
// and witness_set_properties (hbd_interest_rate). Every DefaultTickIntervalBlocks it
// recomputes trusted mean HIVE price and mode HBD APR from the top trusted witnesses.
type FeedTracker struct {
	mu sync.Mutex

	win *WitnessSignatureWindow

	quotes       map[string]float64
	lastFeedBlk  map[string]uint64
	witnessProps map[string]WitnessProperties
	seenWitness  map[string]struct{}

	ma *MovingAverageRing

	last FeedTickSnapshot
}

// NewFeedTracker builds a tracker with a 100-block signature window and a short MA over ticks.
func NewFeedTracker() *FeedTracker {
	return &FeedTracker{
		win:          NewWitnessSignatureWindow(100),
		quotes:       make(map[string]float64),
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
	if len(t.last.WitnessSlashBps) > 0 {
		out.WitnessSlashBps = make(map[string]int, len(t.last.WitnessSlashBps))
		for w, bps := range t.last.WitnessSlashBps {
			out.WitnessSlashBps[w] = bps
		}
	}
	return out
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

// TickIfDue runs the trusted-price + APR aggregation when blockHeight is a multiple of the tick interval.
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

	trusted := make(map[string]bool)
	sigs := make(map[string]int)
	for w := range t.quotes {
		sigs[w] = t.win.SignatureCount(w)
		published := t.lastFeedBlk[w] > 0 && t.lastFeedBlk[w]+uint64(width) > blockHeight
		trusted[w] = FeedTrust(sigs[w], published, DefaultMinSignatures)
	}

	mean, meanOk := TrustedHivePrice(t.quotes, trusted)
	if meanOk {
		t.ma.Push(mean)
	}
	ma, maOk := t.ma.Mean()

	group := RunningWitnessGroup(trusted, sigs, DefaultWitnessGroupSize)
	apr, aprOk := HBDAPRModeFromGroup(group, t.witnessProps)
	slashBps := t.computeWitnessSlashBps(blockHeight)

	t.last = FeedTickSnapshot{
		TickBlockHeight:      blockHeight,
		TrustedHiveMean:      mean,
		TrustedHiveOK:        meanOk,
		HiveMovingAvg:        ma,
		HiveMovingAvgOK:      maOk,
		HBDInterestRateBps:   apr,
		HBDInterestRateOK:    aprOk,
		TrustedWitnessGroup:  append([]string(nil), group...),
		WitnessSlashBps:      slashBps,
	}
}

func (t *FeedTracker) computeWitnessSlashBps(blockHeight uint64) map[string]int {
	if t == nil {
		return nil
	}
	p := defaultOracleSlashParams()
	width := t.win.Width
	if width < 1 {
		width = 100
	}

	ws := t.witnessUniverse()
	if len(ws) == 0 {
		return nil
	}
	out := make(map[string]int, len(ws))
	for _, w := range ws {
		published := t.lastFeedBlk[w] > 0 && t.lastFeedBlk[w]+uint64(width) > blockHeight
		e := oracleSlashEvidence{
			Signatures:  t.win.SignatureCount(w),
			UpdatedFeed: published,
			Equivocated: false, // equivocation evidence is a future extension point
		}
		out[w] = oracleSlashBps(p, e)
	}
	return out
}

type oracleSlashParams struct {
	MinSignatures     int
	MissingSigStepBps int
	MissingUpdateBps  int
	EquivocationBps   int
	CapBps            int
}

type oracleSlashEvidence struct {
	Signatures  int
	UpdatedFeed bool
	Equivocated bool
}

func defaultOracleSlashParams() oracleSlashParams {
	return oracleSlashParams{
		MinSignatures:     4,
		MissingSigStepBps: 25,
		MissingUpdateBps:  50,
		EquivocationBps:   500,
		CapBps:            1000,
	}
}

func oracleSlashBps(p oracleSlashParams, e oracleSlashEvidence) int {
	deficit := p.MinSignatures - e.Signatures
	if deficit < 0 {
		deficit = 0
	}
	raw := deficit*p.MissingSigStepBps
	if !e.UpdatedFeed {
		raw += p.MissingUpdateBps
	}
	if e.Equivocated {
		raw += p.EquivocationBps
	}
	if raw < 0 {
		return 0
	}
	if p.CapBps > 0 && raw > p.CapBps {
		return p.CapBps
	}
	return raw
}

func (t *FeedTracker) witnessUniverse() []string {
	u := make(map[string]struct{})
	for w := range t.seenWitness {
		if strings.TrimSpace(w) != "" {
			u[w] = struct{}{}
		}
	}
	for w := range t.quotes {
		if strings.TrimSpace(w) != "" {
			u[w] = struct{}{}
		}
	}
	for w := range t.lastFeedBlk {
		if strings.TrimSpace(w) != "" {
			u[w] = struct{}{}
		}
	}
	for w := range t.witnessProps {
		if strings.TrimSpace(w) != "" {
			u[w] = struct{}{}
		}
	}
	if len(u) == 0 {
		return nil
	}
	out := make([]string, 0, len(u))
	for w := range u {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

func (t *FeedTracker) ingestFeedPublish(blockHeight uint64, value map[string]interface{}) {
	pub, _ := value["publisher"].(string)
	if pub == "" {
		return
	}
	er, _ := value["exchange_rate"].(map[string]interface{})
	if er == nil {
		return
	}
	base, _ := asString(er["base"])
	quote, _ := asString(er["quote"])
	price, ok := hiveHBDPerHiveFromFeed(base, quote)
	if !ok || price <= 0 {
		return
	}
	t.quotes[pub] = price
	t.lastFeedBlk[pub] = blockHeight
	t.seenWitness[pub] = struct{}{}
}

func (t *FeedTracker) ingestWitnessSetProperties(value map[string]interface{}) {
	owner, _ := value["owner"].(string)
	if owner == "" {
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

func hiveHBDPerHiveFromFeed(base, quote string) (float64, bool) {
	if p, ok := parseHbdPerHivePair(base, quote); ok {
		return p, true
	}
	return parseHbdPerHivePair(quote, base)
}

// parseHbdPerHivePair returns HBD amount per 1 HIVE given "X HBD" and "Y HIVE" asset strings.
func parseHbdPerHivePair(a, b string) (float64, bool) {
	va, ca, okA := parseAssetAmount(a)
	vb, cb, okB := parseAssetAmount(b)
	if !okA || !okB {
		return 0, false
	}
	if isHBD(ca) && isHive(cb) && vb > 0 {
		return va / vb, true
	}
	return 0, false
}

func parseAssetAmount(s string) (amt float64, sym string, ok bool) {
	s = strings.TrimSpace(s)
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0, "", false
	}
	v, err := strconv.ParseFloat(parts[0], 64)
	if err != nil || v <= 0 {
		return 0, "", false
	}
	return v, strings.TrimSpace(parts[1]), true
}

func isHBD(sym string) bool {
	s := strings.ToUpper(sym)
	return s == "HBD"
}

func isHive(sym string) bool {
	s := strings.ToUpper(sym)
	return s == "HIVE" || s == "STEEM"
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
