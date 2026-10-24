package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// B11 / B12 / C4 / D7: the bot had no alarm for running out of RC (every map
// and confirm stops), for a drained gas reserve (withdrawals are refused), for
// a vault holding less ETH, or less of a watched ERC-20 (TOKENS), than the
// bridge has credited, or for the vault key nearing expiry (it retires ~14
// days after expiring and then can never sign again; renewKey), and nothing
// to poll for its health. botHealth checks those every healthCheckEvery, logs
// each problem as a warning, and, with HEALTH_ADDR set, serves them on GET /health
// (200 ok, 503 unhealthy), as the BTC mapping bot's /health does.
//
//	HEALTH_ADDR            listen address, e.g. 127.0.0.1:8091 (unset: no endpoint)
//	RC_ALARM               warn below this many RC on the bot's L2 identity (default 20000)
//	GAS_RESERVE_ALARM_WEI  warn below this gas reserve "gr" (default 0.1 ETH: twice the
//	                       contract's MinGasReserve, below which every unmap is refused)

const healthCheckEvery = 5 * time.Minute

// keyExpiryWarnEpochs is how early the bot warns that the vault key expires:
// 28 epochs, 7 days at 6 hours an epoch.
const keyExpiryWarnEpochs = 28

type botHealth struct {
	mu          sync.Mutex
	rcAlarm     int64
	reserveWarn *big.Int
	staleAfter  time.Duration

	lastTickAt  time.Time
	lastTickErr string
	scanned     uint64
	withdrawals int
	pendingMaps int
	deadLetters int
	checkedAt   time.Time
	alarms      []string
}

func newBotHealth(poll time.Duration) *botHealth {
	h := &botHealth{rcAlarm: 20_000, reserveWarn: big.NewInt(1e17), staleAfter: 5 * poll}
	if v, err := strconv.ParseInt(os.Getenv("RC_ALARM"), 10, 64); err == nil {
		h.rcAlarm = v
	}
	if v, ok := new(big.Int).SetString(os.Getenv("GAS_RESERVE_ALARM_WEI"), 10); ok {
		h.reserveWarn = v
	}
	if h.staleAfter < time.Minute {
		h.staleAfter = time.Minute
	}
	return h
}

// recordTick notes a finished tick.
func (h *botHealth) recordTick(now time.Time, err error, cp *Checkpoint) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastTickAt = now
	h.lastTickErr = ""
	if err != nil {
		h.lastTickErr = err.Error()
	}
	h.scanned = cp.LastScannedBlock
	h.withdrawals = len(cp.SentWithdrawals)
	h.pendingMaps = len(cp.PendingMaps)
	h.deadLetters = len(cp.DeadLetters)
}

// due reports whether the alarms are due a check.
func (h *botHealth) due(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return now.Sub(h.checkedAt) >= healthCheckEvery
}

// check reads the bot's RC, the bridge's gas reserve and ETH custody, and the
// vault's L1 balance, and keeps (and logs) every alarm. A read that fails is
// itself reported, never taken as healthy.
func (h *botHealth) check(ctx context.Context, now time.Time, gql *vscGraphQL, rpc *ethRPC, contractID, tssKeyID, did, vault string, tokens map[string]string) {
	var alarms []string
	warn := func(msg string, args ...any) {
		alarms = append(alarms, msg)
		slog.Warn(msg, args...)
	}

	if rc, known, err := gql.fetchAccountRC(ctx, did); err != nil {
		warn("B11: cannot read the bot's RC", "err", err)
	} else if known && rc < h.rcAlarm {
		warn(fmt.Sprintf("B11: bot RC %d is below %d: maps and confirms will stop", rc, h.rcAlarm), "did", did)
	}

	if status, expiry, epoch, err := gql.fetchKeyExpiry(ctx, tssKeyID); err != nil {
		warn("D7: cannot read the vault key's expiry", "err", err)
	} else if status == "deprecated" {
		warn("D7: the vault key has expired: run renewKey now; it retires ~14 days after expiring and then can never sign", "key", tssKeyID)
	} else if status == "active" && expiry > 0 && expiry <= epoch+keyExpiryWarnEpochs {
		warn(fmt.Sprintf("D7: the vault key expires in %d epochs (~%dh): run renewKey", expiry-min(expiry, epoch), 6*(expiry-min(expiry, epoch))), "key", tssKeyID)
	}

	st, err := gql.fetchContractState(ctx, contractID, []string{"gr", "s-eth", "chainid"})
	if err != nil {
		warn("B12: cannot read the bridge's gas reserve and custody", "err", err)
	} else {
		reserve, _ := new(big.Int).SetString(st["gr"], 10)
		if reserve == nil {
			reserve = new(big.Int)
		}
		if reserve.Cmp(h.reserveWarn) < 0 {
			warn(fmt.Sprintf("B12: gas reserve %s wei is below %s: withdrawals are refused below 0.05 ETH (seed it with a to_reserve=1 deposit)", reserve, h.reserveWarn))
		}
		// The bridge lowers custody when it queues a withdrawal, before the L1
		// transfer, and credits a reverted one back only after it mined, so
		// the vault never rightly holds less than custody. (A pending
		// withdrawal is not added to the vault's side: it is already out of
		// custody, and adding it could only hide a shortfall.)
		custody, ok := supplyActive(st["s-eth"])
		if !ok {
			warn("C4: cannot parse the bridge's ETH custody", "s-eth", st["s-eth"])
		} else if bal, berr := rpc.getBalance(vault); berr != nil {
			warn("C4: cannot read the vault's L1 balance", "err", berr)
		} else if short := new(big.Int).Sub(custody, bal); short.Sign() > 0 {
			warn(fmt.Sprintf("C4: vault %s holds %s wei, %s short of the %s wei of ETH the bridge has credited", vault, bal, short, custody))
		}
		checkTokenCustody(ctx, warn, gql, rpc, contractID, st["chainid"], vault, tokens)
	}

	h.mu.Lock()
	h.alarms = alarms
	h.checkedAt = now
	h.mu.Unlock()
}

// checkTokenCustody is C4 for each watched ERC-20: the vault's balanceOf must
// cover the token's custody, as for ETH (custody drops at unmap, a reverted
// transfer is credited back after it mined). The token's symbol, which names
// its custody record s-<symbol>, is read from the bridge's own registry entry
// t-<chainid>-<address hex>, so no symbol is configured on the bot.
func checkTokenCustody(ctx context.Context, warn func(string, ...any), gql *vscGraphQL, rpc *ethRPC, contractID, chainID, vault string, tokens map[string]string) {
	if len(tokens) == 0 {
		return
	}
	if chainID == "" {
		chainID = "1" // the bridge's chainId() when none is set
	}
	addrs := make([]string, 0, len(tokens))
	for a := range tokens {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	keys := make([]string, len(addrs))
	for i, a := range addrs {
		keys[i] = "t-" + chainID + "-" + strings.TrimPrefix(a, "0x")
	}
	reg, err := gql.fetchContractState(ctx, contractID, keys)
	if err != nil {
		warn("C4: cannot read the bridge's token registry", "err", err)
		return
	}
	for i, a := range addrs {
		sym := strings.Split(reg[keys[i]], "|")[0]
		if sym == "" {
			warn(fmt.Sprintf("C4: token %s is not registered on the bridge: its deposits are refused", a))
			continue
		}
		st, err := gql.fetchContractState(ctx, contractID, []string{"s-" + sym})
		if err != nil {
			warn(fmt.Sprintf("C4: cannot read the bridge's %s custody", sym), "err", err)
			continue
		}
		custody, ok := supplyActive(st["s-"+sym])
		if !ok {
			warn(fmt.Sprintf("C4: cannot parse the bridge's %s custody", sym), "s-"+sym, st["s-"+sym])
			continue
		}
		bal, err := rpc.erc20BalanceOf(a, vault)
		if err != nil {
			warn(fmt.Sprintf("C4: cannot read the vault's %s balance", sym), "token", a, "err", err)
			continue
		}
		if short := new(big.Int).Sub(custody, bal); short.Sign() > 0 {
			warn(fmt.Sprintf("C4: vault %s holds %s %s, %s short of the %s %s the bridge has credited", vault, bal, sym, short, custody, sym))
		}
	}
}

// supplyActive is the custody field (Active, the first) of a bridge supply
// record "active|user|fee|basefee": 0 when there is no record (nothing
// bridged yet), false when the record does not parse.
func supplyActive(rec string) (*big.Int, bool) {
	if rec == "" {
		return new(big.Int), true
	}
	v, ok := new(big.Int).SetString(strings.Split(rec, "|")[0], 10)
	return v, ok
}

type healthReport struct {
	Status           string   `json:"status"` // starting, ok, unhealthy
	LastTickAt       string   `json:"lastTickAt,omitempty"`
	LastScannedBlock uint64   `json:"lastScannedBlock"`
	Withdrawals      int      `json:"pendingWithdrawals"`
	PendingMaps      int      `json:"pendingMaps"`
	DeadLetters      int      `json:"deadLetters"`
	CheckedAt        string   `json:"checkedAt,omitempty"`
	Issues           []string `json:"issues,omitempty"`
}

func (h *botHealth) report(now time.Time) healthReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := healthReport{LastScannedBlock: h.scanned, Withdrawals: h.withdrawals, PendingMaps: h.pendingMaps, DeadLetters: h.deadLetters}
	if h.lastTickAt.IsZero() {
		r.Status = "starting"
		return r
	}
	r.LastTickAt = h.lastTickAt.UTC().Format(time.RFC3339)
	if !h.checkedAt.IsZero() {
		r.CheckedAt = h.checkedAt.UTC().Format(time.RFC3339)
	}
	if age := now.Sub(h.lastTickAt); age > h.staleAfter {
		r.Issues = append(r.Issues, fmt.Sprintf("no tick for %ds", int64(age.Seconds())))
	}
	if h.lastTickErr != "" {
		r.Issues = append(r.Issues, "last tick failed: "+h.lastTickErr)
	}
	if h.deadLetters > 0 {
		r.Issues = append(r.Issues, fmt.Sprintf("%d deposit(s) dead-lettered: map them by hand", h.deadLetters))
	}
	r.Issues = append(r.Issues, h.alarms...)
	r.Status = "ok"
	if len(r.Issues) > 0 {
		r.Status = "unhealthy"
	}
	return r
}

func (h *botHealth) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		r := h.report(time.Now())
		w.Header().Set("Content-Type", "application/json")
		if r.Status == "unhealthy" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(r)
	}
}

// serveHealth serves GET /health on addr until the process exits.
func serveHealth(addr string, h *botHealth) {
	mux := http.NewServeMux()
	mux.Handle("GET /health", h.handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	slog.Info("health endpoint listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("health endpoint stopped", "err", err)
	}
}

// fetchAccountRC returns account's available RC; known is false when the
// node has no RC record for it (reported as unknown, not as zero).
func (g *vscGraphQL) fetchAccountRC(ctx context.Context, account string) (int64, bool, error) {
	data, err := g.query(ctx, `query($a: String!){ getAccountRC(account: $a){ amount } }`, map[string]interface{}{"a": account})
	if err != nil {
		return 0, false, err
	}
	var parsed struct {
		GetAccountRC *struct {
			Amount int64 `json:"amount"`
		} `json:"getAccountRC"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return 0, false, err
	}
	if parsed.GetAccountRC == nil {
		return 0, false, nil
	}
	return parsed.GetAccountRC.Amount, true, nil
}

// getBalance returns addr's L1 balance in wei at the latest block.
func (e *ethRPC) getBalance(addr string) (*big.Int, error) {
	data, err := e.call("eth_getBalance", fmt.Sprintf(`"%s", "latest"`, addr))
	if err != nil {
		return nil, err
	}
	var hexBal string
	if err := json.Unmarshal(data, &hexBal); err != nil {
		return nil, err
	}
	bal, ok := new(big.Int).SetString(strings.TrimPrefix(hexBal, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("eth_getBalance: %q is not a hex quantity", hexBal)
	}
	return bal, nil
}

// erc20BalanceOf returns holder's balance of token at the latest block. A
// reply that is not one 32-byte word (no contract at the address, say) is an
// error.
func (e *ethRPC) erc20BalanceOf(token, holder string) (*big.Int, error) {
	data := "0x70a08231" + strings.Repeat("0", 24) + strings.TrimPrefix(strings.ToLower(holder), "0x")
	raw, err := e.call("eth_call", fmt.Sprintf(`{"to":"%s","data":"%s"}, "latest"`, token, data))
	if err != nil {
		return nil, err
	}
	var word string
	if err := json.Unmarshal(raw, &word); err != nil {
		return nil, err
	}
	word = strings.TrimPrefix(word, "0x")
	bal, ok := new(big.Int).SetString(word, 16)
	if len(word) != 64 || !ok {
		return nil, fmt.Errorf("balanceOf: %q is not a uint256", word)
	}
	return bal, nil
}

// fetchKeyExpiry returns the TSS key's status and expiry epoch, and the
// network's current epoch.
func (g *vscGraphQL) fetchKeyExpiry(ctx context.Context, keyID string) (status string, expiry, epoch uint64, err error) {
	data, err := g.query(ctx, `query($id: String!){ getTssKey(keyId: $id){ status expiry_epoch } localNodeInfo { epoch } }`, map[string]interface{}{"id": keyID})
	if err != nil {
		return "", 0, 0, err
	}
	var parsed struct {
		GetTssKey *struct {
			Status      string `json:"status"`
			ExpiryEpoch uint64 `json:"expiry_epoch"`
		} `json:"getTssKey"`
		LocalNodeInfo *struct {
			Epoch uint64 `json:"epoch"`
		} `json:"localNodeInfo"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", 0, 0, err
	}
	if parsed.GetTssKey == nil || parsed.LocalNodeInfo == nil {
		return "", 0, 0, fmt.Errorf("no key %s or node info", keyID)
	}
	return parsed.GetTssKey.Status, parsed.GetTssKey.ExpiryEpoch, parsed.LocalNodeInfo.Epoch, nil
}
