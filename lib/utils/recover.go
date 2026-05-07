package utils

import (
	"runtime/debug"

	"vsc-node/lib/vsclog"
)

// Pentest finding N-L4: pubsub message dispatchers and RPC handlers
// process untrusted P2P input. A nil-deref or out-of-bounds in any
// handler — or in any library it calls — propagates up the goroutine
// stack and kills the process. Each entry point that runs untrusted
// input on a goroutine MUST defer-recover; this helper centralises it.
//
// Usage:
//
//	go utils.RecoverGoroutine("pubsub.dispatch", func() {
//	    handle(msg)  // may panic on malicious input
//	})
//
// The helper logs the panic plus stack trace on the "recover" module
// logger and swallows it. Synchronous handlers that need to return
// the panic as an error should use a local defer-recover instead;
// this helper is for fire-and-forget goroutines.

var recoverLog = vsclog.Module("recover")

// RecoverGoroutine runs fn with a deferred recover. label identifies
// the call site in logs. If fn panics, the panic is logged with stack
// trace and swallowed.
func RecoverGoroutine(label string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			recoverLog.Error("goroutine panic recovered",
				"label", label,
				"panic", r,
				"stack", string(debug.Stack()))
		}
	}()
	fn()
}
