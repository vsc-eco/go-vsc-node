package main

import (
	"errors"
	"fmt"
	"testing"
)

// A23 regression.
//
// A Mongo disconnect ~40 minutes into normal operation made Run() return
// "incomplete read of message header", which main then logged as a "startup
// failure" before exiting. Exiting is correct -- the plugin aggregate has
// unwound and re-running Run() without a full re-init is not safe -- but the
// label sent whoever read the page looking for a bad config, and nothing in the
// line said a restart would fix it.
//
// isTransientInfraErr picks between the two messages, so what it must get right
// is the observed error text, including when it arrives wrapped through the
// aggregate.
func TestA23_ClassifiesTheObservedDisconnect(t *testing.T) {
	// Verbatim from the A23 devnet run.
	observed := errors.New("incomplete read of message header")

	cases := []error{
		observed,
		// How it actually reaches main: wrapped by the plugin aggregate.
		fmt.Errorf("startup failure: %w", observed),
		fmt.Errorf("plugin %q: %w", "tss", fmt.Errorf("db: %w", observed)),
	}
	for _, err := range cases {
		if !isTransientInfraErr(err) {
			t.Errorf("transient error misclassified as fatal: %v", err)
		}
	}
}

func TestA23_ClassifiesOtherInfraFaults(t *testing.T) {
	for _, msg := range []string{
		"server selection error: context deadline exceeded",
		"connection reset by peer",
		"socket was unexpectedly closed",
		"no reachable servers",
		"topology is closed",
		"dial tcp 127.0.0.1:27017: connect: connection refused",
	} {
		if !isTransientInfraErr(errors.New(msg)) {
			t.Errorf("should be transient: %q", msg)
		}
	}
}

// The other direction matters just as much: a genuine misconfiguration must NOT
// be advertised as "will recover on restart", or the operator waits for a
// recovery that never comes while the node crash-loops.
func TestA23_DoesNotClassifyConfigErrorsAsTransient(t *testing.T) {
	for _, msg := range []string{
		"invalid consensus config: MinMembers must be > 0",
		"missing required env var VSC_HIVE_ACCOUNT",
		"failed to parse genesis file: unexpected end of JSON input",
		"identity key not found on disk",
		"port 8080 already bound by another process",
	} {
		if isTransientInfraErr(errors.New(msg)) {
			t.Errorf("config error wrongly marked restartable: %q", msg)
		}
	}
}

func TestA23_NilIsNotTransient(t *testing.T) {
	if isTransientInfraErr(nil) {
		t.Fatal("nil must not be classified as a transient failure")
	}
}
