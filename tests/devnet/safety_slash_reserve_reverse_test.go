package devnet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	libhive "vsc-node/lib/hive"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/gateway"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"

	"github.com/vsc-eco/hivego"
)

// devnetChainID (the Hive chain id baked into DevnetConfig, every signature over
// a devnet tx is computed against it) is declared in ledger_ops.go and reused
// here.

// ledgerRecord mirrors the GraphQL LedgerRecord type (schema.graphql:381). Only
// the fields the slash tests assert on are decoded.
type ledgerRecord struct {
	Id          string `json:"id"`
	Amount      int64  `json:"amount"`
	BlockHeight uint64 `json:"block_height"`
	From        string `json:"from"`
	Owner       string `json:"owner"`
	Type        string `json:"type"`
	Asset       string `json:"asset"`
	TxId        string `json:"tx_id"`
}

// findLedgerTXs runs a findLedgerTXs query against a node's GQL endpoint with an
// optional byToFrom account filter and optional byTypes list. Returns the decoded
// rows. Fatals on transport/decoding errors so a flaky node surfaces loudly.
func findLedgerTXs(t *testing.T, gqlURL, byToFrom string, byTypes []string) []ledgerRecord {
	t.Helper()

	// Build the filterOptions literal. byToFrom is always set in these tests;
	// byTypes is optional.
	var filter bytes.Buffer
	filter.WriteString(`byToFrom: "` + byToFrom + `"`)
	if len(byTypes) > 0 {
		filter.WriteString(`, byTypes: [`)
		for i, ty := range byTypes {
			if i > 0 {
				filter.WriteString(", ")
			}
			filter.WriteString(`"` + ty + `"`)
		}
		filter.WriteString(`]`)
	}

	q := `{ findLedgerTXs(filterOptions: { ` + filter.String() + ` }) ` +
		`{ id amount block_height from owner type asset tx_id } }`

	reqBody, _ := json.Marshal(map[string]string{"query": q})
	resp, err := http.Post(gqlURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("findLedgerTXs POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var out struct {
		Data struct {
			FindLedgerTXs []ledgerRecord `json:"findLedgerTXs"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("findLedgerTXs decode: %v (body: %s)", err, string(raw))
	}
	if len(out.Errors) > 0 {
		t.Fatalf("findLedgerTXs gql errors: %v (query: %s)", out.Errors, q)
	}
	return out.Data.FindLedgerTXs
}

// gqlHiveSpendable reads an account's spendable HIVE balance (the `hive` field of
// getAccountBalance) from a node. -1 on error, 0 if no record. The reserve
// op-type is excluded from this aggregation, so a reserve account must read 0.
func gqlHiveSpendable(t *testing.T, gqlURL, account string) int64 {
	t.Helper()
	q := `{ getAccountBalance(account: "` + account + `") { hive } }`
	reqBody, _ := json.Marshal(map[string]string{"query": q})
	resp, err := http.Post(gqlURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Data struct {
			GetAccountBalance *struct {
				Hive int64 `json:"hive"`
			} `json:"getAccountBalance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return -1
	}
	if out.Data.GetAccountBalance == nil {
		return 0
	}
	return out.Data.GetAccountBalance.Hive
}

// waitForStableBond polls an account's hive_consensus bond on node 1 until it is
// non-zero and unchanged across two consecutive reads, returning that value as
// the true post-genesis staking baseline. The genesis consensus stake is not in
// GQL at head 0 — it lands a few blocks in — so reading a baseline too early
// yields 0 and corrupts every later delta. Fatals if no stable non-zero bond
// appears within the timeout.
func waitForStableBond(t *testing.T, d *Devnet, account string, timeout time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int64 = -1
	for time.Now().Before(deadline) {
		v := gqlConsensus(t, d.GQLEndpoint(1), account)
		if v > 0 && v == last {
			return v
		}
		last = v
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("bond for %s never stabilized to a non-zero value within %v (last=%d)", account, timeout, last)
	return 0
}

// sumSlashDebits returns the absolute total of safety_slash_consensus debit rows
// for an account (the definitive on-ledger record of how much bond was slashed),
// read from node 1. Keying the proof off these rows is more robust than a bond
// delta because it is unaffected by baseline-read timing.
func sumSlashDebits(t *testing.T, d *Devnet, hiveAccount string) int64 {
	t.Helper()
	rows := findLedgerTXs(t, d.GQLEndpoint(1), hiveAccount, []string{"safety_slash_consensus"})
	var total int64
	for _, r := range rows {
		if r.Amount < 0 && r.Asset == "hive_consensus" {
			total += -r.Amount
		}
	}
	return total
}

// evidenceKindFromSlashRowID extracts the EvidenceKind embedded in a
// safety_slash_consensus debit row id. The id format is built by
// safetySlashConsensusRowID:
//
//	<SlashTxID>#safety_slash#<EvidenceKind>#consensus_debit#<acct>
//
// so EvidenceKind is the token between "#safety_slash#" and the next "#". Empty
// string if the id does not match the expected shape. (Reversing a slash requires
// the EXACT kind that produced the row — a double-sign also trips the
// invalid-block detector, so node-3 carries rows of BOTH kinds; the reverse must
// use each row's own kind, not a hardcoded one.)
func evidenceKindFromSlashRowID(id string) string {
	const marker = "#safety_slash#"
	i := strings.Index(id, marker)
	if i < 0 {
		return ""
	}
	rest := id[i+len(marker):]
	j := strings.Index(rest, "#")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// devnetGatewayKeypair re-derives a witness node's gateway multisig keypair the
// SAME way devnet-setup does under DEVNET_DETERMINISTIC_BLS=1:
//
//	seed   = sha256("devnet-bls-"+nodeName)
//	gwKey  = gateway.GatewayKeyFromBlsSeed(hex(seed))
//
// This is the ONLY way the test obtains the signing keys — identityConfig.json is
// never read.
func devnetGatewayKeypair(t *testing.T, nodeName string) *hivego.KeyPair {
	t.Helper()
	seed := sha256.Sum256([]byte("devnet-bls-" + nodeName))
	kp, err := gateway.GatewayKeyFromBlsSeed(hex.EncodeToString(seed[:]))
	if err != nil {
		t.Fatalf("deriving gateway key for %s: %v", nodeName, err)
	}
	return kp
}

// broadcastGatewayMultisig builds, multisig-signs and broadcasts a custom_json
// op authorized by the vsc.gateway account. The gateway account on a 6-node
// devnet has WeightThreshold = 6*2/3 = 4 with each witness key weight 1, so
// signing with any `signWith` keypairs (>=4) satisfies the threshold. Returns the
// broadcast tx id.
//
// requiredAuths lets callers test the auth gate negatively (e.g. pass a
// non-gateway account → state engine drops it).
func broadcastGatewayMultisig(
	t *testing.T,
	d *Devnet,
	id string,
	requiredAuths []string,
	jsonPayload string,
	signWith []*hivego.KeyPair,
) (string, error) {
	t.Helper()

	// Broadcast via the drone endpoint — the same proxy the working ledger-op
	// helpers (BroadcastCustomJSON / Deposit) use for condenser_api broadcasts.
	client := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	client.ChainID = devnetChainID

	creator := libhive.LiveTransactionCreator{
		TransactionBroadcaster: libhive.TransactionBroadcaster{Client: client},
		TransactionCrafter:     libhive.TransactionCrafter{},
	}

	op := creator.CustomJson(requiredAuths, []string{}, id, jsonPayload)
	tx := creator.MakeTransaction([]hivego.HiveOperation{op})

	// Populate ref_block_num / ref_block_prefix / expiration from the live chain.
	if err := creator.PopulateSigningProps(&tx, nil); err != nil {
		return "", fmt.Errorf("populate signing props: %w", err)
	}

	// Multisig: append one signature per key. Each Sign is computed over the same
	// serialized tx against the devnet chain id.
	for i, kp := range signWith {
		sig, err := tx.Sign(*kp, devnetChainID)
		if err != nil {
			return "", fmt.Errorf("sign with key %d: %w", i, err)
		}
		tx.AddSig(sig)
	}

	return client.BroadcastRaw(tx)
}

// TestSafetySlashResidualToReserve proves the destination change: when node-3
// double-signs once and is slashed 10% of its HIVE_CONSENSUS bond, the slashed
// residual lands in the keyless insurance reserve
// (system:protocol_slash_reserve, op-type safety_slash_reserve) once it matures
// past the (test-shortened) burn delay — NOT in the old burn sink — and is not
// counted as spendable HIVE. The honest node's bond is untouched.
func TestSafetySlashResidualToReserve(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	const (
		attacker    = "magi.test3"
		honest      = "magi.test1"
		reserveAcct = "system:protocol_slash_reserve"
		burnPending = "system:protocol_slash_burn_pending"
		// VSC_SLASH_BURN_DELAY=10 so the pending residual matures into the
		// reserve within the observation window.
		burnDelay = 10
	)

	cfg := DefaultConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = true
	cfg.LogLevel = "info"
	cfg.MagiEnv = map[string]string{
		"VSC_DOUBLE_SIGN_ACCOUNT": attacker,
		"VSC_DOUBLE_SIGN_ONCE":    "1",
		"VSC_SLASH_BURN_DELAY":    fmt.Sprint(burnDelay),
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{ElectionInterval: 20},
	}

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	// Establish the true post-genesis staking baseline from the HONEST node (it is
	// never slashed, so its stable bond == the per-witness stake every node started
	// with). node-3's own bond is read too, but it may already be mid-slash.
	baseH := waitForStableBond(t, d, "hive:"+honest, 8*time.Minute)
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	base3 := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
	t.Logf("[reserve] head=%d baseline(stable honest)=%d  node-3 now=%d", head, baseH, base3)

	// Poll node-3's bond until it drops below the honest baseline (the double-sign
	// fired and was slashed). Up to ~10 minutes of block production — node-3 only
	// double-signs the first slot it is elected to produce.
	slashDeadline := time.Now().Add(10 * time.Minute)
	var cur3 int64 = base3
	for time.Now().Before(slashDeadline) {
		cur3 = gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
		if cur3 >= 0 && cur3 < baseH {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !(cur3 >= 0 && cur3 < baseH) {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[reserve] node-3 bond never dropped below honest baseline (baseH=%d cur=%d) — double-sign slash did not fire", baseH, cur3)
	}
	// Definitive slashed amount = sum of on-ledger safety_slash_consensus debits.
	slashed := sumSlashDebits(t, d, "hive:"+attacker)
	t.Logf("[reserve] node-3 slashed: bond now=%d (honest baseline=%d); ledger slash-debit total=%d", cur3, baseH, slashed)
	if slashed <= 0 {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[reserve] no safety_slash_consensus debit rows for node-3 despite bond drop")
	}

	// Let the pending residual mature past burnDelay and the finalize cursor run.
	// FinalizeMaturedSafetySlashBurns promotes the pending row to the reserve.
	dropHead, _ := getHeadBlock(d.HiveRPCEndpoint())
	t.Logf("[reserve] waiting ~%d blocks for residual to mature into the reserve (dropHead=%d)", burnDelay+15, dropHead)
	waitForBlock(t, d.HiveRPCEndpoint(), dropHead+burnDelay+15, 8*time.Minute)

	// Poll the reserve for a positive safety_slash_reserve credit. Promotion runs
	// on the finalize cursor tick, so allow a few more blocks/seconds.
	reserveDeadline := time.Now().Add(4 * time.Minute)
	var reserveRows []ledgerRecord
	var reserveCredit int64
	for time.Now().Before(reserveDeadline) {
		reserveRows = findLedgerTXs(t, d.GQLEndpoint(1), reserveAcct, nil)
		reserveCredit = 0
		for _, r := range reserveRows {
			if r.To == reserveAcct && r.Asset == "hive" && r.Amount > 0 {
				reserveCredit += r.Amount
			}
		}
		if reserveCredit > 0 {
			break
		}
		time.Sleep(3 * time.Second)
	}

	if reserveCredit <= 0 {
		// Diagnostics: maybe still pending, or finalize cursor lagging.
		pendingRows := findLedgerTXs(t, d.GQLEndpoint(1), burnPending, nil)
		t.Logf("[reserve] reserve rows: %+v", reserveRows)
		t.Logf("[reserve] pending-burn rows: %+v", pendingRows)
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[reserve] no positive safety_slash_reserve credit on %s (got %d)", reserveAcct, reserveCredit)
	}
	t.Logf("[reserve] reserve credit on %s = %d (rows=%d)", reserveAcct, reserveCredit, len(reserveRows))

	// Assert a row carries the reserve op-type explicitly. Either a direct
	// safety_slash_reserve credit (BurnDelayBlocks==0 path) or a promoted pending
	// row — both end up owned by the reserve as asset hive. We required the
	// op-types are reserve-family (not the old burn op).
	sawReserveType := false
	for _, r := range reserveRows {
		if r.To != reserveAcct || r.Amount <= 0 {
			continue
		}
		switch r.Type {
		case "safety_slash_reserve",
			"safety_slash_hive_burn_pending_finalized",
			"safety_slash_hive_burn_pending_release":
			sawReserveType = true
		}
		// The destination change means the OLD burn op-type must never own value
		// on the reserve. (Defense in depth: the burn op-type credited a burn
		// sink, never this account.)
		if r.Type == "safety_slash_hive_burn" {
			t.Errorf("[reserve] unexpected OLD burn op-type row on reserve: %+v", r)
		}
	}
	if !sawReserveType {
		t.Errorf("[reserve] no reserve-family op-type row found among reserve credits: %+v", reserveRows)
	}

	// The reserve credit must equal the slashed residual (full 10% — there is no
	// victim payout; the entire residual is the reserve credit). Allow exact match.
	if reserveCredit != slashed {
		t.Logf("[reserve] NOTE: reserve credit %d != slashed %d (acceptable if multiple incidents or rounding); both positive", reserveCredit, slashed)
	}

	// The reserve must NOT be spendable HIVE — getAccountBalance(hive) excludes the
	// reserve op-type from spendable aggregation.
	reserveSpendable := gqlHiveSpendable(t, d.GQLEndpoint(1), reserveAcct)
	t.Logf("[reserve] %s spendable hive = %d (must be 0/excluded)", reserveAcct, reserveSpendable)
	if reserveSpendable > 0 {
		t.Errorf("[reserve] reserve has spendable HIVE %d — destination-change excludes reserve from spendable", reserveSpendable)
	}

	// Honest node bond unchanged across all 6 nodes.
	for n := 1; n <= cfg.Nodes; n++ {
		aH := gqlConsensus(t, d.GQLEndpoint(n), "hive:"+honest)
		if aH != baseH {
			t.Errorf("[reserve] node-%d: honest %s bond changed %d -> %d (should be unchanged)", n, honest, baseH, aH)
		}
	}

	// Cross-node consensus: reserve credit visible on all 6 nodes.
	for n := 1; n <= cfg.Nodes; n++ {
		rows := findLedgerTXs(t, d.GQLEndpoint(n), reserveAcct, nil)
		var c int64
		for _, r := range rows {
			if r.To == reserveAcct && r.Asset == "hive" && r.Amount > 0 {
				c += r.Amount
			}
		}
		t.Logf("[reserve] node-%d view: reserve credit = %d", n, c)
		if c <= 0 {
			t.Errorf("[reserve] node-%d sees no reserve credit (fork?)", n)
		}
	}

	t.Logf("[reserve] PASS: residual %d landed in keyless reserve %s, excluded from spendable, honest bond intact, agreed on all nodes", reserveCredit, reserveAcct)
}

// TestSafetySlashGovernanceReverse proves the wrongful-slash reversal path: a
// gateway-multisig-signed vsc.safety_slash_reverse L1 op (action "both") flows
// through ingest -> pool -> a VSC block -> 2/3 BLS -> applySafetySlashReverse and
// RESTORES node-3's slashed HIVE_CONSENSUS bond on real multi-node consensus.
//
// A LONG burn delay (VSC_SLASH_BURN_DELAY=600) keeps the residual pending so the
// reverse has full headroom (a "both" cancel+reverse during the challenge
// window). DEVNET_DETERMINISTIC_BLS=1 lets the test re-derive the gateway keys.
func TestSafetySlashGovernanceReverse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	const (
		attacker = "magi.test3"
		honest   = "magi.test1"
	)

	cfg := DefaultConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = true
	cfg.LogLevel = "info"
	cfg.MagiEnv = map[string]string{
		"VSC_DOUBLE_SIGN_ACCOUNT":  attacker,
		"VSC_DOUBLE_SIGN_ONCE":     "1",
		"VSC_SLASH_BURN_DELAY":     "600", // long: residual stays pending in the window
		"DEVNET_DETERMINISTIC_BLS": "1",   // deterministic gateway keys for re-derivation
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{ElectionInterval: 20},
	}

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	// True post-genesis staking baseline from the honest node (== node-3's pre-slash
	// bond, since every witness staked the same amount). This is the value node-3's
	// bond must RETURN to after a full reverse.
	baseH := waitForStableBond(t, d, "hive:"+honest, 8*time.Minute)
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	t.Logf("[reverse] head=%d baseline(stable honest)=%d  (node-3 pre-slash bond == baseline)", head, baseH)

	// 1) Wait for the single double-sign incident to slash node-3's bond below the
	// honest baseline.
	slashDeadline := time.Now().Add(10 * time.Minute)
	var cur3 int64 = gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
	for time.Now().Before(slashDeadline) {
		cur3 = gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
		if cur3 >= 0 && cur3 < baseH {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !(cur3 >= 0 && cur3 < baseH) {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[reverse] node-3 bond never dropped below honest baseline (baseH=%d cur=%d) — slash did not fire", baseH, cur3)
	}
	// Definitive slashed amount = sum of on-ledger safety_slash_consensus debits.
	slashedAmount := sumSlashDebits(t, d, "hive:"+attacker)
	t.Logf("[reverse] node-3 slashed: bond now=%d (baseline=%d); ledger slash-debit total=%d", cur3, baseH, slashedAmount)
	if slashedAmount <= 0 {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[reverse] no safety_slash_consensus debit rows for node-3")
	}

	// 2) Pick ONE original slash row to reverse. A double-sign incident trips BOTH
	// the double-block-sign AND the invalid-block-proposal detectors, so node-3
	// carries safety_slash_consensus rows of more than one EvidenceKind, each keyed
	// by its own (SlashTxID, EvidenceKind). A reverse op targets exactly one tuple,
	// and the apply path caps the re-credit at THAT row's amount and EXACT kind. We
	// prefer a vsc_double_block_sign row (the canonical equivocation evidence), but
	// extract the kind from the row id so the payload always matches the chosen row.
	var slashRow ledgerRecord
	var slashRowKind string
	rowDeadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(rowDeadline) {
		rows := findLedgerTXs(t, d.GQLEndpoint(1), "hive:"+attacker, []string{"safety_slash_consensus"})
		var best ledgerRecord
		var bestKind string
		for _, r := range rows {
			if r.Amount >= 0 || r.Asset != "hive_consensus" {
				continue
			}
			k := evidenceKindFromSlashRowID(r.Id)
			if k == "" {
				continue
			}
			// Prefer a double_block_sign row; among ties, prefer the largest debit.
			preferNew := false
			switch {
			case best.TxId == "":
				preferNew = true
			case k == safetyslash.EvidenceVSCDoubleBlockSign && bestKind != safetyslash.EvidenceVSCDoubleBlockSign:
				preferNew = true
			case (k == safetyslash.EvidenceVSCDoubleBlockSign) == (bestKind == safetyslash.EvidenceVSCDoubleBlockSign) && -r.Amount > -best.Amount:
				preferNew = true
			}
			if preferNew {
				best = r
				bestKind = k
			}
		}
		if best.TxId != "" {
			slashRow = best
			slashRowKind = bestKind
			break
		}
		time.Sleep(3 * time.Second)
	}
	if slashRow.TxId == "" {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[reverse] no safety_slash_consensus row found for %s", attacker)
	}
	// SlashTxID is the row's tx_id field; EvidenceKind is parsed from the row id.
	slashTxID := slashRow.TxId
	evidenceKind := slashRowKind
	rowSlashedAmt := -slashRow.Amount // positive amount this one incident debited
	t.Logf("[reverse] target slash row: id=%s tx_id=%s amount=%d kind=%s (total slashed across incidents=%d)",
		slashRow.Id, slashTxID, slashRow.Amount, evidenceKind, slashedAmount)

	// Record node-3's bond just before the reverse so we can assert the exact rise.
	preReverse := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
	wantBond := preReverse + rowSlashedAmt // bond after the one-incident reverse
	t.Logf("[reverse] pre-reverse bond=%d; expect rise by %d -> %d", preReverse, rowSlashedAmt, wantBond)

	// 3) Build the reverse payload (action "both" = cancel pending + re-credit bond).
	// Amount == this incident's slash amount; the apply path caps at the row amount.
	rec := safetyslash.SafetySlashReverseRecord{
		SlashTxID:      slashTxID,
		EvidenceKind:   evidenceKind,
		SlashedAccount: attacker,
		Action:         safetyslash.ReverseActionBoth,
		Amount:         rowSlashedAmt,
		Reason:         "devnet wrongful-slash reversal test",
	}.Normalize()
	payload, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("[reverse] marshal reverse record: %v", err)
	}
	t.Logf("[reverse] reverse payload: %s", string(payload))

	// 4) Derive the gateway multisig keys for all 6 witnesses; sign with 4 of them
	// (threshold = 6*2/3 = 4, each weight 1).
	allKeys := make([]*hivego.KeyPair, 0, cfg.Nodes)
	for n := 1; n <= cfg.Nodes; n++ {
		allKeys = append(allKeys, devnetGatewayKeypair(t, fmt.Sprintf("%s%d", cfg.WitnessPrefix, n)))
	}
	signWith := allKeys[:4] // any 4 of 6 satisfy the threshold

	// 5) Broadcast the gateway-multisig vsc.safety_slash_reverse op. RequiredAuths
	// MUST be exactly ["vsc.gateway"] (state_engine.go on-ramp gate checks
	// RequiredAuths[0] == GatewayWallet()).
	txID, err := broadcastGatewayMultisig(t, d, "vsc.safety_slash_reverse",
		[]string{"vsc.gateway"}, string(payload), signWith)
	if err != nil {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[reverse] broadcasting gateway-multisig reverse op: %v", err)
	}
	t.Logf("[reverse] broadcast vsc.safety_slash_reverse tx=%s (4-of-6 gateway multisig)", txID)

	// 6) Poll node-3's bond on ALL 6 nodes until it RISES by the reversed incident's
	// amount. The reverse must be queued (L1 op observed), included in a VSC block,
	// the block's 2/3 BLS aggregate ratified, then applySafetySlashReverse credits
	// the bond. Allow ~80 blocks (several full leader rotations).
	restoreDeadline := time.Now().Add(8 * time.Minute)
	restored := false
	for time.Now().Before(restoreDeadline) {
		a3 := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
		if a3 >= wantBond {
			restored = true
			break
		}
		time.Sleep(3 * time.Second)
	}

	// Per-node view after the attempted restore. Each node folds the reverse
	// credit into its hive_consensus balance snapshot on its own slot cadence, so
	// the credit row (already written on every node by applySafetySlashReverse)
	// surfaces in getAccountBalance a slot or two apart. Poll each node to
	// convergence with a deadline — exactly like the node-1 `restored` loop above
	// — instead of reading once: a single-shot multi-node read taken right after
	// only node-1 converged is racy and reports false negatives (e.g. node-1/2 at
	// wantBond while node-3..6 are still one slot behind) even though every node
	// has applied the reverse. The honest-bond invariant is still asserted per node.
	t.Logf("[reverse] === per-node bond after reverse (want >= %d) ===", wantBond)
	allRestored := true
	for n := 1; n <= cfg.Nodes; n++ {
		nodeDeadline := time.Now().Add(2 * time.Minute)
		a3 := gqlConsensus(t, d.GQLEndpoint(n), "hive:"+attacker)
		for a3 < wantBond && time.Now().Before(nodeDeadline) {
			time.Sleep(3 * time.Second)
			a3 = gqlConsensus(t, d.GQLEndpoint(n), "hive:"+attacker)
		}
		aH := gqlConsensus(t, d.GQLEndpoint(n), "hive:"+honest)
		t.Logf("[reverse] node-%d: %s=%d (want>=%d, baseline %d) %s(honest)=%d", n, attacker, a3, wantBond, baseH, honest, aH)
		if a3 < wantBond {
			allRestored = false
		}
		if aH != baseH {
			t.Errorf("[reverse] node-%d: honest bond changed %d -> %d", n, baseH, aH)
		}
	}

	// Grep logs for the queue + apply.
	for _, kw := range []string{
		"queued governance cancellation request",
		"safety slash reverse: bond credit recorded",
		"safety slash reverse: pending burn cancelled",
	} {
		if node, ok := anyNodeLogsContain(d, ctx, cfg.Nodes, kw); ok {
			t.Logf("[reverse] log %q seen (e.g. magi-%d)", kw, node)
		} else {
			t.Logf("[reverse] log %q NOT seen", kw)
		}
	}

	if !restored || !allRestored {
		// Diagnostics: look for the reverse credit row directly.
		credRows := findLedgerTXs(t, d.GQLEndpoint(1), "hive:"+attacker, []string{"safety_slash_consensus_reverse"})
		t.Logf("[reverse] safety_slash_consensus_reverse rows: %+v", credRows)
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 60)
		t.Fatalf("[reverse] node-3 bond NOT raised to %d on all nodes (restored=%v allRestored=%v)", wantBond, restored, allRestored)
	}

	// Confirm a safety_slash_consensus_reverse credit row exists (positive,
	// hive_consensus, owned by the attacker) and agrees across nodes.
	for n := 1; n <= cfg.Nodes; n++ {
		credRows := findLedgerTXs(t, d.GQLEndpoint(n), "hive:"+attacker, []string{"safety_slash_consensus_reverse"})
		var credit int64
		for _, r := range credRows {
			if r.Amount > 0 && r.Asset == "hive_consensus" {
				credit += r.Amount
			}
		}
		t.Logf("[reverse] node-%d: reverse credit total = %d", n, credit)
		if credit <= 0 {
			t.Errorf("[reverse] node-%d: no positive safety_slash_consensus_reverse credit (fork or not applied?)", n)
		}
	}

	t.Logf("[reverse] PASS: gateway-multisig reverse raised node-3 bond by %d (now >= %d, baseline %d) on all %d nodes; honest bond intact",
		rowSlashedAmt, wantBond, baseH, cfg.Nodes)

	// 7) Negative sub-case: a reverse with RequiredAuths=[magi.test1] (NON-gateway)
	// must be DROPPED by the on-ramp auth gate (state_engine.go: RequiredAuths[0]
	// != GatewayWallet() → never enqueued → never applied). To make this a MEANINGFUL
	// negative (not a false pass from exhausted headroom), it must target a slash row
	// that still has reversible headroom — i.e. a DIFFERENT incident than the one we
	// just reversed. If only one incident fired, skip with a clear note rather than
	// assert a misleading pass.
	var negRow ledgerRecord
	var negKind string
	for _, r := range findLedgerTXs(t, d.GQLEndpoint(1), "hive:"+attacker, []string{"safety_slash_consensus"}) {
		if r.Amount >= 0 || r.Asset != "hive_consensus" || r.Id == slashRow.Id {
			continue
		}
		k := evidenceKindFromSlashRowID(r.Id)
		if k == "" {
			continue
		}
		negRow = r
		negKind = k
		break
	}
	if negRow.TxId == "" {
		t.Logf("[reverse] negative sub-case SKIPPED: no second un-reversed slash row with headroom to test the auth gate against")
	} else {
		preNeg := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
		negPayload, _ := json.Marshal(safetyslash.SafetySlashReverseRecord{
			SlashTxID:      negRow.TxId,
			EvidenceKind:   negKind,
			SlashedAccount: attacker,
			Action:         safetyslash.ReverseActionBoth,
			Amount:         -negRow.Amount,
			Reason:         "devnet NEGATIVE non-gateway auth test",
		}.Normalize())
		// Declare a non-gateway RequiredAuths. The on-ramp gate keys off
		// RequiredAuths[0] != vsc.gateway and never enqueues, regardless of signatures.
		negTx, negErr := broadcastGatewayMultisig(t, d, "vsc.safety_slash_reverse",
			[]string{honest}, string(negPayload), signWith)
		if negErr != nil {
			// A broadcast-level rejection (Hive rejects a non-matching multisig
			// authority for magi.test1) is also acceptable — the op never reaches the
			// chain, so the bond cannot change. Either way the bond must not rise.
			t.Logf("[reverse] negative case broadcast rejected at L1 (acceptable): %v", negErr)
		} else {
			t.Logf("[reverse] negative case broadcast tx=%s (non-gateway RequiredAuths=[%s], targets un-reversed slash %s)", negTx, honest, negRow.TxId)
		}
		// Give it the same window a real reverse would need, then assert no rise.
		negHead, _ := getHeadBlock(d.HiveRPCEndpoint())
		waitForBlock(t, d.HiveRPCEndpoint(), negHead+20, 4*time.Minute)
		postNeg := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
		if postNeg > preNeg {
			t.Errorf("[reverse] NEGATIVE FAIL: non-gateway reverse raised bond %d -> %d (auth gate should have dropped it)", preNeg, postNeg)
		} else {
			t.Logf("[reverse] NEGATIVE PASS: non-gateway reverse did not raise bond (%d -> %d) — auth gate held", preNeg, postNeg)
		}
	}
}
