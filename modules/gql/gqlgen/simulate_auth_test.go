package gqlgen

import (
	"context"
	"strings"
	"testing"

	"vsc-node/modules/gql/model"
)

// End-to-end reproduction of pentest finding F14.
//
// Bug: SimulateContractCalls accepted any value (including the
// empty list and obviously-fabricated account names) for
// input.RequiredAuths and input.RequiredPostingAuths, then derived
// caller from input.RequiredAuths[0] / RequiredPostingAuths[0]
// without sanity-checking either list. The pentest demonstrated:
//
//   - Empty `required_auths: []` simulations succeed.
//   - Non-existent accounts ("hive:i_dont_exist_at_all",
//     "hive:evil_hacker_9999") return success: true.
//
// Used as a probing primitive: an attacker can simulate as any
// user (real or made up) and observe contract behaviour.
//
// This test calls the actual resolver entry point with a queryResolver
// whose dependencies are all nil — pre-fix the resolver makes it
// past auth validation and panics on r.HiveBlocks.GetHighestBlock()
// (nil pointer); post-fix the validator fires first and we get a
// clean error before any data access.
//
// The test wraps the call in a deferred recover so a pre-fix
// panic is captured as a test failure with the specific bug
// shape, instead of crashing the test process.

func runSimulate(input SimulateContractCallsInput) (errMsg string, panicValue interface{}) {
	defer func() {
		if r := recover(); r != nil {
			panicValue = r
		}
	}()
	r := &queryResolver{Resolver: &Resolver{}}
	_, err := r.SimulateContractCalls(context.Background(), input)
	if err != nil {
		errMsg = err.Error()
	}
	return
}

func TestF14_SimulateValidatesAuth(t *testing.T) {
	t.Run("EmptyAuthsRejected", func(t *testing.T) {
		errMsg, panicValue := runSimulate(SimulateContractCallsInput{
			Calls: []SimulateContractCallInput{{
				ContractID: "vscWhatever",
				Action:     "noop",
				Payload:    "",
				RcLimit:    100,
			}},
			RequiredAuths:        []string{},
			RequiredPostingAuths: []string{},
		})
		if panicValue != nil {
			t.Fatalf(
				"F14 leak: empty auths reached unauthenticated data access (panic at %v).\n"+
					"  Pre-fix the resolver had no auth validator, so empty-auth simulations\n"+
					"  proceed straight into HiveBlocks lookups (here panicking on nil mock).\n"+
					"  Post-fix the auth validator must reject before any data access.",
				panicValue)
		}
		if errMsg == "" {
			t.Fatal("F14 leak: empty auths returned no error from the resolver")
		}
		if !strings.Contains(strings.ToLower(errMsg), "auth") {
			t.Errorf("expected auth-related error, got %q", errMsg)
		}
	})

	t.Run("InvalidAuthPrefixRejected", func(t *testing.T) {
		errMsg, panicValue := runSimulate(SimulateContractCallsInput{
			Calls: []SimulateContractCallInput{{
				ContractID: "vscWhatever",
				Action:     "noop",
				Payload:    "",
				RcLimit:    100,
			}},
			// "evil_hacker_9999" without a hive:/did: prefix is
			// a malformed authority — should never be accepted.
			RequiredAuths: []string{"evil_hacker_9999"},
		})
		if panicValue != nil {
			t.Fatalf("malformed auth reached data access (panic): %v", panicValue)
		}
		if errMsg == "" {
			t.Fatal("malformed auth returned no error")
		}
		if !strings.Contains(strings.ToLower(errMsg), "hive:") &&
			!strings.Contains(strings.ToLower(errMsg), "did:") {
			t.Errorf("expected error mentioning the required prefixes, got %q", errMsg)
		}
	})

	t.Run("ValidPrefixedAuthGetsPastValidation", func(t *testing.T) {
		// A properly-prefixed auth (real or not) must pass the
		// new validator. With the all-nil resolver here, the call
		// then panics on the next data access — that's expected
		// and proves the validator didn't reject valid format.
		_, panicValue := runSimulate(SimulateContractCallsInput{
			Calls: []SimulateContractCallInput{{
				ContractID: "vscWhatever",
				Action:     "noop",
				Payload:    "",
				RcLimit:    100,
			}},
			RequiredAuths: []string{"hive:legitimate_account"},
		})
		// We expect the panic here — auth validator passed, then
		// the all-nil HiveBlocks blew up. If panicValue is nil
		// the validator over-rejected (false positive).
		if panicValue == nil {
			t.Fatalf("validator unexpectedly rejected a valid hive:-prefixed auth")
		}
	})
}

// Compile-time sanity: keep the gqlgen input types referenced so
// type drift between schema and test surfaces immediately.
var _ = SimulateContractCallsInput{Calls: []SimulateContractCallInput{{RcLimit: model.Uint64(0)}}}
