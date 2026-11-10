package main

import (
	"context"
	"fmt"
	"strings"
)

// checkUnwatchedTokens: NEW-BOT-1. The bot scans ERC-20 deposits only for the
// tokens it is configured with (TOKENS, and USDC when NETWORK is exactly
// "mainnet"), while the bridge's registry (tsyms, ts-{symbol}) decides what it
// credits. Health only compared the watched tokens against the registry, so a
// token registered on the bridge but missing from the bot's list had every
// deposit go unscanned with no alarm. This is the other direction: each
// registered token the bot does not watch is an alarm. It reads state only.
func checkUnwatchedTokens(ctx context.Context, warn func(string, ...any), gql *vscGraphQL, contractID string, tokens map[string]string) {
	st, err := gql.fetchContractState(ctx, contractID, []string{"tsyms"})
	if err != nil {
		warn("NEW-BOT-1: cannot read the bridge's registered tokens", "err", err)
		return
	}
	var keys []string
	for _, s := range strings.Split(st["tsyms"], "|") {
		if s != "" {
			keys = append(keys, "ts-"+s)
		}
	}
	if len(keys) == 0 {
		return
	}
	refs, err := gql.fetchContractState(ctx, contractID, keys)
	if err != nil {
		warn("NEW-BOT-1: cannot read the bridge's token registry", "err", err)
		return
	}
	watched := map[string]bool{}
	for a := range tokens {
		watched[strings.ToLower(strings.TrimPrefix(a, "0x"))] = true
	}
	for _, k := range keys {
		ref := refs[k] // {chainId}-{address hex}
		i := strings.LastIndex(ref, "-")
		if i < 0 {
			continue
		}
		addr := strings.ToLower(strings.TrimPrefix(ref[i+1:], "0x"))
		if addr != "" && !watched[addr] {
			warn(fmt.Sprintf("NEW-BOT-1: token %s (0x%s) is registered on the bridge but this bot does not watch it (TOKENS): its deposits are never scanned or mapped", strings.TrimPrefix(k, "ts-"), addr))
		}
	}
}
