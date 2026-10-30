package mapper

import (
	"errors"
	"fmt"
	"testing"
)

// P2: isTooBigForPool must recognise an oversize L2 tx from BOTH sources - the bot's own
// pre-flight (errTxTooLarge, which fires before the node ever sees the tx) and the node's
// refusal string. Before the fix it matched only the node string, so a map the bot stopped
// itself (a different message) slipped past the map handler's escape and the deposit's block
// was retried forever instead of being recorded for the operator.
func TestP2_IsTooBigForPool_BotPreflightAndNode(t *testing.T) {
	// The bot's own pre-flight (wrapped sentinel) - the case that regressed.
	preflight := fmt.Errorf("%w: 20000 bytes (limit %d)", errTxTooLarge, 16384)
	if !isTooBigForPool(preflight) {
		t.Fatal("bot pre-flight oversize (errTxTooLarge) must be recognised; a miss holds the deposit block forever")
	}
	// Wrapped one more layer, as callWithRetry / CallContract may wrap with %w.
	if !isTooBigForPool(fmt.Errorf("broadcast L2 tx: %w", preflight)) {
		t.Fatal("a wrapped pre-flight oversize must still be recognised")
	}
	// The node's refusal string (unchanged behaviour).
	if !isTooBigForPool(errors.New("transaction size too big 20000 > 16384")) {
		t.Fatal("the node's oversize refusal must still be recognised")
	}
	// Anything else is not an oversize, and nil is never one.
	if isTooBigForPool(errors.New("some other failure")) {
		t.Fatal("a non-size error must not be taken as oversize")
	}
	if isTooBigForPool(nil) {
		t.Fatal("nil is not an oversize")
	}
}
