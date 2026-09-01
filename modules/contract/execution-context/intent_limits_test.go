package contract_execution_context

import (
	"math"
	"os"
	"strings"
	"testing"

	"vsc-node/modules/db/vsc/contracts"
)

// buildTokenLimits decides how much a contract may pull from the caller. These
// tests pin BOTH halves of the gate:
//
//   - with the gate OFF the behaviour must be byte-identical to the pre-fix code,
//     including the math.MaxInt64 clamp on an out-of-range limit. That is what
//     makes shipping this safe on a live chain: an un-activated node computes the
//     same state it always did, so there is nothing to fork over.
//   - with the gate ON an unparseable limit records NO ceiling, so PullBalance's
//     own missing-limit path refuses the draw.
//
// If the OFF case ever changes, the change is a consensus break and this test is
// the thing that should stop it.

func allow(token, limit string, extra ...string) contracts.Intent {
	args := map[string]string{"limit": limit, "token": token}
	for i := 0; i+1 < len(extra); i += 2 {
		args[extra[i]] = extra[i+1]
	}
	return contracts.Intent{Type: "transfer.allow", Args: args}
}

func limitFor(t *testing.T, m map[string]*int64, token string) (int64, bool) {
	t.Helper()
	p, ok := m[token]
	if !ok || p == nil {
		return 0, false
	}
	return *p, true
}

func TestIntentLimits_GateOff_PreservesLegacyBehaviourExactly(t *testing.T) {
	// The dangerous one: out of range. strconv.ParseInt returns MaxInt64 AND an
	// error; the legacy code discarded the error and kept the value.
	got := buildTokenLimits([]contracts.Intent{allow("hbd", "99999999999999999999")}, false)
	v, ok := limitFor(t, got, "hbd")
	if !ok {
		t.Fatal("legacy behaviour recorded no limit — that IS the behaviour change this gate exists to avoid")
	}
	if v != math.MaxInt64 {
		t.Fatalf("legacy limit = %d, want math.MaxInt64 (%d). Changing this without the gate forks the chain", v, int64(math.MaxInt64))
	}

	// A syntax error already failed closed, and must keep doing so.
	got = buildTokenLimits([]contracts.Intent{allow("hbd", "not-a-number")}, false)
	if v, ok := limitFor(t, got, "hbd"); !ok || v != 0 {
		t.Fatalf("legacy syntax-error limit = (%d, present=%t), want (0, true)", v, ok)
	}
}

func TestIntentLimits_GateOn_RefusesAnUnparseableLimit(t *testing.T) {
	for _, bad := range []string{
		"99999999999999999999", // out of range -> was MaxInt64
		"1.2.3",                // more than one decimal point
		"not-a-number",         // pure syntax
		"",                     // empty
	} {
		got := buildTokenLimits([]contracts.Intent{allow("hbd", bad)}, true)
		if _, ok := limitFor(t, got, "hbd"); ok {
			t.Fatalf("strict mode recorded a ceiling for limit %q — it must record none so the draw is refused", bad)
		}
	}
}

func TestIntentLimits_GateOn_LeavesValidLimitsUntouched(t *testing.T) {
	// The gate must only affect the failure path. A well-formed limit resolves
	// identically either way, or activating it would change every honest tx too.
	for _, tc := range []struct {
		limit, decimals string
		want            int64
	}{
		{"1.000", "3", 1000},
		{"25.5", "3", 25500},
		{"0", "3", 0},
	} {
		on := buildTokenLimits([]contracts.Intent{allow("hbd", tc.limit, "decimals", tc.decimals)}, true)
		off := buildTokenLimits([]contracts.Intent{allow("hbd", tc.limit, "decimals", tc.decimals)}, false)
		vOn, okOn := limitFor(t, on, "hbd")
		vOff, okOff := limitFor(t, off, "hbd")
		if !okOn || !okOff || vOn != vOff || vOn != tc.want {
			t.Fatalf("limit %q: on=(%d,%t) off=(%d,%t), want both %d", tc.limit, vOn, okOn, vOff, okOff, tc.want)
		}
	}
}

func TestIntentLimits_FirstIntentWinsPerToken_BothModes(t *testing.T) {
	// Not the largest, not the last — array order. Pinned because it is the kind
	// of thing a reader assumes wrongly, and because a future "take the smaller"
	// change would be a consensus change too.
	intents := []contracts.Intent{
		allow("hbd", "1.000", "decimals", "3"),
		allow("hbd", "500.000", "decimals", "3"),
	}
	for _, strict := range []bool{false, true} {
		v, ok := limitFor(t, buildTokenLimits(intents, strict), "hbd")
		if !ok || v != 1000 {
			t.Fatalf("strict=%t: limit = (%d, present=%t), want the FIRST intent's 1000", strict, v, ok)
		}
	}
}

func TestIntentLimits_IgnoresNonTransferAllowAndIncompleteIntents(t *testing.T) {
	intents := []contracts.Intent{
		{Type: "something.else", Args: map[string]string{"limit": "5.000", "token": "hbd"}},
		{Type: "transfer.allow", Args: map[string]string{"token": "hbd"}},   // no limit
		{Type: "transfer.allow", Args: map[string]string{"limit": "5.000"}}, // no token
	}
	for _, strict := range []bool{false, true} {
		if got := buildTokenLimits(intents, strict); len(got) != 0 {
			t.Fatalf("strict=%t: expected no limits from unknown/incomplete intents, got %v", strict, got)
		}
	}
}

// ★ THE WIRING, not just the helper.
//
// The tests above call buildTokenLimits directly, which proves the function but
// says nothing about what New() passes it. That distinction is not academic: a
// mutant that hard-codes `true` at New's call site — i.e. ships the strict rule
// UNGATED, which is the fork — passed every test above. This is the test that
// catches it.
//
// New with no options must produce the legacy limits, MaxInt64 clamp included.
func TestIntentLimits_NewDefaultsToLegacyBehaviour(t *testing.T) {
	env := Environment{Intents: []contracts.Intent{allow("hbd", "99999999999999999999")}}
	ctx := New(env, 0, 0, 0, nil, nil, 0)

	v, ok := limitFor(t, ctx.tokenLimits, "hbd")
	if !ok {
		t.Fatal("New() recorded no limit for an out-of-range value — the strict rule is active BY DEFAULT, which changes contract execution for every node that installs this build and forks the chain")
	}
	if v != math.MaxInt64 {
		t.Fatalf("New() limit = %d, want math.MaxInt64 (%d) — the default path must stay byte-identical until a coordinated activation", v, int64(math.MaxInt64))
	}
}

// And the option must actually reach the parse — otherwise activating the gate
// would be a silent no-op and the hole would stay open with everyone believing
// it was closed.
func TestIntentLimits_NewHonoursTheOption(t *testing.T) {
	env := Environment{Intents: []contracts.Intent{allow("hbd", "99999999999999999999")}}
	ctx := New(env, 0, 0, 0, nil, nil, 0, WithStrictIntentLimits(true))

	if _, ok := limitFor(t, ctx.tokenLimits, "hbd"); ok {
		t.Fatal("WithStrictIntentLimits(true) did not reach the limit parse — the gate is inert, so turning it on would close nothing")
	}
}

// ★ THE GATE MUST PROPAGATE INTO NESTED CALLS.
//
// A nested contract call builds its OWN tokenLimits from its own intents
// (ContractCall passes opts.Intents into a fresh New). If the gate did not
// propagate, one transaction would parse limits under the strict rule at the top
// level and the legacy rule one frame down — two different ceilings for the same
// signed intent, decided by call depth. That is a state divergence inside a single
// transaction, which is exactly what the tryCatch gate propagates to avoid.
//
// This test asserts the source does the propagation, in the same
// read-the-source-and-derive style used elsewhere in this repo for cross-file
// guarantees that cannot be reached through a unit-testable seam (constructing a
// real nested ContractCall needs a ledger, a session and a wasm runtime).
func TestIntentLimits_GatePropagatesIntoNestedContractCalls(t *testing.T) {
	src, err := os.ReadFile("execution-context.go")
	if err != nil {
		t.Fatalf("cannot read own source: %v", err)
	}
	text := string(src)

	// The nested New() inside ContractCall is the only place WithTryCatch is
	// passed a ctx field rather than being defined; find it and require our gate
	// alongside it.
	const marker = "WithTryCatch(ctx.tryCatchActive)"
	idx := strings.Index(text, marker)
	if idx < 0 {
		t.Fatalf("could not find the nested-call option list (%q) — this test's anchor moved; fix the anchor, do not delete the test", marker)
	}
	window := text[idx : idx+400]
	if !strings.Contains(window, "WithStrictIntentLimits(ctx.strictIntentLimits)") {
		t.Fatal("the nested contract call does not propagate WithStrictIntentLimits — a nested call would build its tokenLimits under the LEGACY rule while the top level used the strict one, so one transaction would enforce two different ceilings depending on call depth")
	}
}
