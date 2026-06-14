// gen-validator-set-payload emits the setValidatorSet payload string
// the dash-mapping-contract expects, snapshotted from a magi node's
// witnessNodes(height) GraphQL endpoint. Replaces the manual workflow
// of hand-collecting (DID, pubkey, PoP, account) tuples from each
// witness operator — point this at a magi GQL endpoint, pipe stdout
// straight into the contract-deployer.
//
// Authoritative source remains the contract state (mapping admin still
// has to call setValidatorSet); this tool ONLY auto-fills the payload
// from the on-chain witness announcements. Operators can still hand-
// edit before broadcasting (the payload is plain text) for non-witness
// signers or to exclude a specific witness mid-rotation.
//
// Usage:
//
//	gen-validator-set-payload \
//	    -gqlURL https://magi-testnet.example.com/api/v1/graphql \
//	    -epoch 65
//	# stdout: 65;did:key:z3tE...=03ab...=041f...=witness1|did:key:z3tE...=...|...
//
// Then:
//
//	contract-deployer call \
//	    -contractId vsc1B<mapping> \
//	    -action setValidatorSet \
//	    -payload "$(gen-validator-set-payload -gqlURL ... -epoch 65)"
//
// Filtering rules (in order):
//
//  1. enabled == true (skips paused / decommissioned witnesses)
//
//  2. did_keys contains an entry with t="consensus" + ct="DID-BLS"
//     + non-empty key + non-empty pop (the consensus-BLS marker
//     written by modules/announcements/announcements.go). Witnesses
//     that announced before PoP support are skipped — they cannot
//     pass the contract's PoP verification anyway, so including
//     them would just be a costly L1 broadcast that the contract
//     rejects.
//
//  3. -verifyPoP (default true): locally verifies each PoP against
//     `key` + `account` using dids.VerifyBlsPoP before emitting.
//     Catches a malformed-but-published PoP early — saves an L1
//     RC + chain-output-error cycle. -verifyPoP=false skips when
//     you intentionally want to mirror the contract behaviour
//     bit-for-bit (e.g. reproducing a historical payload).
//
// Failure mode: returns a non-zero exit code with a diagnostic on
// stderr if (a) the GQL query fails, (b) the response has GraphQL
// errors, (c) zero witnesses survived filtering. The empty-result
// case prints a per-filter breakdown (skipped_disabled / skipped_no_
// consensus_bls / skipped_no_pop / skipped_pop_invalid) so operators
// don't need to enable debug logging to diagnose.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"vsc-node/lib/dids"
)

const (
	consensusKeyT  = "consensus"
	consensusKeyCt = "DID-BLS"
)

type witness struct {
	Account string `json:"account"`
	Enabled *bool  `json:"enabled"`
	DidKeys []key  `json:"did_keys"`
}

type key struct {
	T   string `json:"t"`
	Ct  string `json:"ct"`
	Key string `json:"key"`
	PoP string `json:"pop"`
}

type runArgs struct {
	gqlURL    string
	epoch     uint64
	height    uint64
	verifyPoP bool
	timeout   time.Duration
}

type filterCounts struct {
	total              int
	skippedDisabled    int
	skippedNoBLSKey    int
	skippedNoPoP       int
	skippedPoPInvalid  int
	skippedParseError  int
}

func main() {
	args := parseFlags()
	ctx, cancel := context.WithTimeout(context.Background(), args.timeout)
	defer cancel()

	witnesses, err := fetchWitnesses(ctx, args.gqlURL, args.height)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetching witnesses:", err)
		os.Exit(1)
	}

	payload, counts, err := buildPayload(witnesses, args.epoch, args.verifyPoP)
	if err != nil {
		fmt.Fprintln(os.Stderr, "building payload:", err)
		fmt.Fprintf(os.Stderr, "  filter stats: total=%d skipped_disabled=%d skipped_no_consensus_bls=%d skipped_no_pop=%d skipped_pop_invalid=%d skipped_parse_error=%d\n",
			counts.total, counts.skippedDisabled, counts.skippedNoBLSKey, counts.skippedNoPoP, counts.skippedPoPInvalid, counts.skippedParseError)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "ok: built payload from %d witnesses (filter stats: total=%d skipped_disabled=%d skipped_no_consensus_bls=%d skipped_no_pop=%d skipped_pop_invalid=%d skipped_parse_error=%d)\n",
		counts.total-counts.skippedDisabled-counts.skippedNoBLSKey-counts.skippedNoPoP-counts.skippedPoPInvalid-counts.skippedParseError,
		counts.total, counts.skippedDisabled, counts.skippedNoBLSKey, counts.skippedNoPoP, counts.skippedPoPInvalid, counts.skippedParseError)

	fmt.Println(payload)
}

func parseFlags() runArgs {
	gqlURL := flag.String("gqlURL", "", "magi GraphQL endpoint (required)")
	epoch := flag.Uint64("epoch", 0, "validator-set epoch number to encode into the payload (required)")
	height := flag.Uint64("height", ^uint64(0), "L1 block height to query witnessNodes at; default ^uint64(0) (latest known announcement per account)")
	verifyPoP := flag.Bool("verifyPoP", true, "locally verify each PoP against key + account before emitting; catches malformed PoPs before the L1 broadcast")
	timeout := flag.Duration("timeout", 30*time.Second, "HTTP timeout for the GraphQL query")
	flag.Parse()

	if *gqlURL == "" {
		fmt.Fprintln(os.Stderr, "-gqlURL is required")
		os.Exit(2)
	}
	if !flagPassed("epoch") {
		fmt.Fprintln(os.Stderr, "-epoch is required (no implicit default — wrong epoch in the payload silently invalidates every attestation until next setValidatorSet)")
		os.Exit(2)
	}
	return runArgs{
		gqlURL:    *gqlURL,
		epoch:     *epoch,
		height:    *height,
		verifyPoP: *verifyPoP,
		timeout:   *timeout,
	}
}

// flagPassed reports whether the named flag was set on the command line,
// distinguishing "user passed -epoch=0 deliberately" from "user forgot
// -epoch". Without this, the implicit zero default would silently produce
// a payload for epoch 0 — which on a running contract means every
// attestation would fail until the next setValidatorSet.
func flagPassed(name string) bool {
	passed := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = true
		}
	})
	return passed
}

func fetchWitnesses(ctx context.Context, gqlURL string, atHeight uint64) ([]witness, error) {
	query := `query Snapshot($h: Uint64!) {
  witnessNodes(height: $h) {
    account
    enabled
    did_keys {
      t
      ct
      key
      pop
    }
  }
}`
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]any{"h": fmt.Sprintf("%d", atHeight)},
	})
	if err != nil {
		return nil, fmt.Errorf("marshalling query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", gqlURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting query: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("upstream http %d", resp.StatusCode)
	}

	var out struct {
		Data struct {
			WitnessNodes []witness `json:"witnessNodes"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("upstream GraphQL: %s", out.Errors[0].Message)
	}
	return out.Data.WitnessNodes, nil
}

// buildPayload assembles the contract's setValidatorSet payload:
//
//	<epoch>;<did>=<pubkey_hex>=<pop_hex>=<account>|...
//
// Sorts entries by account name so the output is deterministic regardless
// of the upstream mongo cursor order — makes diffs across runs meaningful.
func buildPayload(witnesses []witness, epoch uint64, verifyPoP bool) (string, filterCounts, error) {
	counts := filterCounts{total: len(witnesses)}

	type entry struct {
		account string
		text    string
	}
	entries := make([]entry, 0, len(witnesses))

	for _, w := range witnesses {
		if w.Enabled == nil || !*w.Enabled {
			counts.skippedDisabled++
			continue
		}
		blsKey := findConsensusBLSKey(w.DidKeys)
		if blsKey == nil || blsKey.Key == "" {
			counts.skippedNoBLSKey++
			continue
		}
		if blsKey.PoP == "" {
			counts.skippedNoPoP++
			continue
		}
		// Derive the raw pubkey bytes from the BLS DID. The contract's
		// payload parser expects the 48-byte compressed G1 as 96 hex
		// chars; the witness's `key` field stores the did:key:... DID
		// form, so we decode the multibase + emit the underlying
		// pubkey bytes.
		did := dids.BlsDID(blsKey.Key)
		pub := did.Identifier()
		if pub == nil {
			counts.skippedParseError++
			continue
		}
		if verifyPoP {
			if err := dids.VerifyBlsPoP(did, w.Account, blsKey.PoP); err != nil {
				counts.skippedPoPInvalid++
				continue
			}
		}
		// PoP from announcements is base64-rawurl; contract wants hex.
		// VerifyBlsPoP already decoded successfully (when verifyPoP=true),
		// so we know the format is valid. Re-decode here for emit.
		popBytes, err := base64URLRawDecode(blsKey.PoP)
		if err != nil {
			counts.skippedParseError++
			continue
		}
		pubBytes := pub.Serialize()
		entries = append(entries, entry{
			account: w.Account,
			text: fmt.Sprintf("%s=%s=%s=%s",
				did.String(),
				hex.EncodeToString(pubBytes[:]),
				hex.EncodeToString(popBytes),
				w.Account),
		})
	}

	if len(entries) == 0 {
		return "", counts, fmt.Errorf("no usable witnesses after filtering — refusing to emit an empty payload (would burn admin RC + silently disable all attestations)")
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].account < entries[j].account })
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = e.text
	}
	return fmt.Sprintf("%d;%s", epoch, strings.Join(parts, "|")), counts, nil
}

// findConsensusBLSKey returns the consensus BLS key entry (the one the
// announcements module writes with t=consensus + ct=DID-BLS), or nil if
// none is found.
func findConsensusBLSKey(keys []key) *key {
	for i := range keys {
		if keys[i].T == consensusKeyT && keys[i].Ct == consensusKeyCt {
			return &keys[i]
		}
	}
	return nil
}

// base64URLRawDecode wraps encoding/base64.RawURLEncoding.DecodeString in
// a helper so the dependency is centralised. Strips padding tolerance:
// the announcements module emits unpadded raw-url, but operators who
// paste from elsewhere sometimes get padded variants; accept both.
func base64URLRawDecode(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64RawURLDecode(s)
}
