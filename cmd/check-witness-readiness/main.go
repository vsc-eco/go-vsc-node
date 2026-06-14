// check-witness-readiness queries a magi GraphQL endpoint for the active
// witness fleet's announced version (major + consensus + non_consensus)
// and reports whether the fleet is ready to flip the chain-active
// consensus version to a target floor.
//
// Pre-activation use: before broadcasting vsc.propose_consensus_version
// to bump the consensus floor (e.g. 0.2.0 → 0.3.0 to activate the
// trusted-forwarders-from-contract path), run this tool against a magi
// node and verify the fleet's published version triples exceed the
// target. If any witness is behind, activation will produce divergent
// outputs from that witness (chain-fork risk) — better to surface that
// fleet-wide BEFORE proposing the version bump.
//
// Output: human-readable summary on stderr, plus a list of
// "behind" witnesses on stdout (one account name per line). Exits
// non-zero when 1+ witnesses are behind the target.
//
// Usage:
//
//	check-witness-readiness \
//	    -gqlURL https://magi-testnet.example.com/api/v1/graphql \
//	    -targetMajor 0 -targetConsensus 3 -targetNonConsensus 0
//
//	# stderr: "summary: 12/15 witnesses at-or-above 0.3.0; 3 behind"
//	# stdout: witness4
//	#         witness7
//	#         witness11
//	# exit code 1
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"
)

type version struct {
	Major        uint64
	Consensus    uint64
	NonConsensus uint64
}

// String returns the human-readable major.consensus.non_consensus form
// the announcements + monitoring expect.
func (v version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Consensus, v.NonConsensus)
}

// meetsMin reports whether v is ≥ floor under the lexicographic ordering
// magi's consensusversion.Cmp uses (major first, then consensus, then
// non-consensus). Below the floor is "behind".
func (v version) meetsMin(floor version) bool {
	if v.Major != floor.Major {
		return v.Major > floor.Major
	}
	if v.Consensus != floor.Consensus {
		return v.Consensus > floor.Consensus
	}
	return v.NonConsensus >= floor.NonConsensus
}

type witness struct {
	Account             string `json:"account"`
	Enabled             *bool  `json:"enabled"`
	VersionMajor        uint64 `json:"version_major"`
	ProtocolVersion     uint64 `json:"protocol_version"`
	VersionNonConsensus uint64 `json:"version_non_consensus"`
}

type runArgs struct {
	gqlURL       string
	target       version
	height       uint64
	includeDisabled bool
	timeout      time.Duration
}

func main() {
	args := parseFlags()
	ctx, cancel := context.WithTimeout(context.Background(), args.timeout)
	defer cancel()

	witnesses, err := fetchWitnesses(ctx, args.gqlURL, args.height)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetching witnesses:", err)
		os.Exit(2)
	}

	report := buildReport(witnesses, args.target, args.includeDisabled)
	for _, acct := range report.behindAccounts {
		fmt.Println(acct)
	}
	fmt.Fprintf(os.Stderr,
		"summary: %d/%d %switnesses at-or-above %s; %d behind%s\n",
		report.ready, report.total, report.enabledOnlyTag(), args.target,
		len(report.behindAccounts), report.disabledNote(),
	)
	if len(report.behindAccounts) > 0 {
		os.Exit(1)
	}
}

type reportData struct {
	ready          int
	total          int
	behindAccounts []string
	disabledCount  int
	includeDisabled bool
}

func (r reportData) enabledOnlyTag() string {
	if r.includeDisabled {
		return ""
	}
	return "enabled "
}

func (r reportData) disabledNote() string {
	if r.includeDisabled || r.disabledCount == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d disabled witnesses excluded — pass -includeDisabled to count them)", r.disabledCount)
}

func buildReport(witnesses []witness, target version, includeDisabled bool) reportData {
	r := reportData{includeDisabled: includeDisabled}
	for _, w := range witnesses {
		enabled := w.Enabled != nil && *w.Enabled
		if !enabled {
			r.disabledCount++
			if !includeDisabled {
				continue
			}
		}
		r.total++
		v := version{
			Major:        w.VersionMajor,
			Consensus:    w.ProtocolVersion,
			NonConsensus: w.VersionNonConsensus,
		}
		if v.meetsMin(target) {
			r.ready++
		} else {
			r.behindAccounts = append(r.behindAccounts, w.Account)
		}
	}
	sort.Strings(r.behindAccounts)
	return r
}

func parseFlags() runArgs {
	gqlURL := flag.String("gqlURL", "", "magi GraphQL endpoint (required)")
	major := flag.Uint64("targetMajor", 0, "minimum acceptable version.Major")
	consensus := flag.Uint64("targetConsensus", 0, "minimum acceptable version.Consensus")
	nonConsensus := flag.Uint64("targetNonConsensus", 0, "minimum acceptable version.NonConsensus")
	height := flag.Uint64("height", ^uint64(0), "L1 block height to snapshot the witness fleet at; default ^uint64(0) (latest announcement per account)")
	includeDisabled := flag.Bool("includeDisabled", false, "count disabled witnesses against readiness; default false (disabled witnesses don't sign, so they don't gate activation)")
	timeout := flag.Duration("timeout", 30*time.Second, "HTTP timeout for the GraphQL query")
	flag.Parse()

	if *gqlURL == "" {
		fmt.Fprintln(os.Stderr, "-gqlURL is required")
		os.Exit(2)
	}
	return runArgs{
		gqlURL: *gqlURL,
		target: version{
			Major:        *major,
			Consensus:    *consensus,
			NonConsensus: *nonConsensus,
		},
		height:          *height,
		includeDisabled: *includeDisabled,
		timeout:         *timeout,
	}
}

func fetchWitnesses(ctx context.Context, gqlURL string, atHeight uint64) ([]witness, error) {
	query := `query Readiness($h: Uint64!) {
  witnessNodes(height: $h) {
    account
    enabled
    version_major
    protocol_version
    version_non_consensus
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
