package devnet

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Helpers for driving the magi-market NFT marketplace on a devnet.
//
// The marketplace is three cooperating contracts — a payment token, an NFT
// collection, and the market itself — so nothing about it can be exercised by
// deploying one wasm. These helpers build all three from the magi-market
// repository, deploy them, and read their state back through GraphQL.

// MagiMarketWasm holds the host paths of the three contracts a marketplace
// devnet needs.
type MagiMarketWasm struct {
	Market string
	Nft    string
	Token  string
}

// magiMarketDir locates the magi-market checkout.
//
// MAGI_MARKET_DIR wins if set. Otherwise look beside the go-vsc-node checkout,
// which is where it sits in the usual layout, so the common case needs no
// configuration.
func magiMarketDir() (string, error) {
	if d := os.Getenv("MAGI_MARKET_DIR"); d != "" {
		if _, err := os.Stat(filepath.Join(d, "Makefile")); err != nil {
			return "", fmt.Errorf("MAGI_MARKET_DIR=%s has no Makefile: %w", d, err)
		}
		return d, nil
	}
	root := findSourceRoot()
	for _, cand := range []string{
		filepath.Join(filepath.Dir(root), "magi-market"),
		filepath.Join(filepath.Dir(filepath.Dir(root)), "magi-market"),
	} {
		if _, err := os.Stat(filepath.Join(cand, "Makefile")); err == nil {
			return cand, nil
		}
	}
	return "", fmt.Errorf("magi-market checkout not found; set MAGI_MARKET_DIR")
}

// BuildMagiMarketContracts builds market + nft + token wasm.
//
// `make artifacts` is the marketplace repo's own target: it builds the market
// from source and fetches/builds magi_nft and magi_token from vsc-eco main.
// Reusing it means this test can never drift from what that repo's own suite
// runs against.
func BuildMagiMarketContracts(ctx context.Context) (*MagiMarketWasm, error) {
	dir, err := magiMarketDir()
	if err != nil {
		return nil, err
	}

	log.Printf("[devnet] building magi-market contracts in %s (a few minutes on a cold cache)...", dir)
	cmd := exec.CommandContext(ctx, "make", "-C", dir, "artifacts")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("building magi-market artifacts: %w", err)
	}

	art := filepath.Join(dir, "test", "artifacts")
	w := &MagiMarketWasm{
		Market: filepath.Join(art, "main.wasm"),
		Nft:    filepath.Join(art, "nft.wasm"),
		Token:  filepath.Join(art, "token.wasm"),
	}
	for _, p := range []string{w.Market, w.Nft, w.Token} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("built wasm missing: %s: %w", p, err)
		}
	}
	log.Printf("[devnet] magi-market contracts built")
	return w, nil
}

// WitnessAccount is the `hive:` address of a node's witness — the account that
// signs when a call is made through that node.
func (d *Devnet) WitnessAccount(node int) string {
	return "hive:" + d.cfg.WitnessPrefix + strconv.Itoa(node)
}

// MarketRcLimit is the rc_limit the production SDK sends.
//
// The devnet's default (500000) would let calls succeed that no real user could
// afford, which would make a green devnet run say nothing about whether buckets
// fit a real transaction. Using the production value means these tests fail if
// a bucket operation stops being affordable.
//
// 10000 is also the safe choice for a different reason: rc_limit above the free
// tier reserves HBD against RC in PullBalance, which can strand a caller's own
// balance.
const MarketRcLimit = 10000

// CallMarketContract invokes a contract with the production rc_limit and waits
// for the transaction to reach a terminal status, returning whether it applied.
func (d *Devnet) CallMarketContract(ctx context.Context, node int, contractId, action, payload string) (string, string, error) {
	txId, err := d.CallContractWithIntents(ctx, node, contractId, action, payload, nil, MarketRcLimit)
	if err != nil {
		return "", "", fmt.Errorf("%s on %s: %w", action, contractId, err)
	}
	// 6 minutes, not 2-3. "INCLUDED" means the transaction is in a Hive block
	// but the state engine has not processed it yet, and that gap widens under
	// load — a run has already been lost to a call that was progressing
	// normally, just not fast enough for a tighter deadline.
	status, err := d.WaitForTxStatus(ctx, node, txId, 6*time.Minute)
	return txId, status, err
}

// WaitForTxStatus polls a transaction until it reaches a terminal status.
func (d *Devnet) WaitForTxStatus(ctx context.Context, node int, txId string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		status, err := d.FindTransactionStatus(ctx, node, txId)
		if err == nil {
			switch status {
			case "PROCESSED", "CONFIRMED", "FAILED":
				return status, nil
			}
			last = status
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("tx %s never reached a terminal status (last=%q)", txId, last)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// ContractCallResult is the outcome of one contract execution as the chain
// recorded it.
type ContractCallResult struct {
	Ok  bool
	Ret string
}

// ContractCallResults returns what the contract actually returned for a
// transaction — including the abort message when it refused.
//
// A "FAILED" status alone says nothing about WHY, and the node's default log
// level does not surface contract output, so a failing devnet call is otherwise
// a dead end. The contract's own message is the whole diagnosis.
func (d *Devnet) ContractCallResults(ctx context.Context, node int, txId string) ([]ContractCallResult, error) {
	const q = `query($t:String!){findContractOutput(filterOptions:{byInput:$t}){results{ret ok}}}`
	var out struct {
		FindContractOutput []struct {
			Results []struct {
				Ret string `json:"ret"`
				Ok  bool   `json:"ok"`
			} `json:"results"`
		} `json:"findContractOutput"`
	}
	if err := d.gqlQuery(ctx, node, q, map[string]any{"t": txId}, &out); err != nil {
		return nil, err
	}
	var res []ContractCallResult
	for _, o := range out.FindContractOutput {
		for _, r := range o.Results {
			res = append(res, ContractCallResult{Ok: r.Ok, Ret: r.Ret})
		}
	}
	return res, nil
}

// ContractCallError returns the contract's message for a failed call, or "" if
// the call succeeded or no output has been indexed yet.
func (d *Devnet) ContractCallError(ctx context.Context, node int, txId string) string {
	// A failed tx with NO failing contract output did not abort — it never ran.
	// Distinguishing the two is the whole point: a contract refusal names its
	// own reason, while a chain-level rejection (most often exhausted RC, since
	// an account's budget is its VSC-ledger hbd balance plus a 10k free tier)
	// leaves no message at all. Reporting an empty string for the second case
	// sends the reader hunting through contract logic that never executed.
	deadline := time.Now().Add(45 * time.Second)
	for {
		res, err := d.ContractCallResults(ctx, node, txId)
		if err == nil && len(res) > 0 {
			for _, r := range res {
				if !r.Ok {
					return r.Ret
				}
			}
			return "(tx failed but every contract result is ok — rejected outside the contract, e.g. insufficient RC)"
		}
		if time.Now().After(deadline) {
			return "(no contract output — the contract never executed; most likely insufficient RC)"
		}
		time.Sleep(3 * time.Second)
	}
}

// decodeNftBalance decodes an NFT balance from contract state.
//
// The two contracts here do NOT store integers the same way. The market writes
// uint64s as decimal text, so bucket counters read back with ParseUint. The NFT
// contract writes RAW LITTLE-ENDIAN BYTES — a balance of 4 is the single byte
// 0x04, not "4" — which ParseUint rejects, silently yielding 0 and making
// delivered cards look undelivered. This mirrors decodeNftU64 in the market
// contract, which is how the market itself reads these balances.
func decodeNftBalance(s string) uint64 {
	b := []byte(s)
	if len(b) == 0 || len(b) > 8 {
		return 0
	}
	var buf [8]byte
	copy(buf[:], b) // least-significant byte first
	return binary.LittleEndian.Uint64(buf[:])
}

// NftBalance reads a holder's balance of one token id straight from the NFT
// contract's state, using the same key the market itself reads.
func (d *Devnet) NftBalance(ctx context.Context, node int, nftContract, account, tokenId string) (uint64, error) {
	key := "bal|" + account + "|" + tokenId
	st, err := d.GetStateByKeys(ctx, node, nftContract, []string{key})
	if err != nil {
		return 0, err
	}
	return decodeNftBalance(stateString(st[key])), nil
}

// BucketField reads one field of a bucket's state (see the layout comment in
// magi-market's internal.go).
func (d *Devnet) BucketField(ctx context.Context, node int, market string, bucketId uint64, field string) (string, error) {
	key := "bkt|" + strconv.FormatUint(bucketId, 10) + "|" + field
	st, err := d.GetStateByKeys(ctx, node, market, []string{key})
	if err != nil {
		return "", err
	}
	return stateString(st[key]), nil
}

// BucketUnits is the units remaining across a whole bucket.
func (d *Devnet) BucketUnits(ctx context.Context, node int, market string, bucketId uint64) (uint64, error) {
	v, err := d.BucketField(ctx, node, market, bucketId, "u")
	if err != nil {
		return 0, err
	}
	return parseUint(v), nil
}

// stateString normalises a getStateByKeys value, which arrives as a JSON string
// for contract values and as nil for keys that were never written.
func stateString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatUint(uint64(t), 10)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func stateUint(v any) uint64 {
	return parseUint(stateString(v))
}

func parseUint(s string) uint64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
