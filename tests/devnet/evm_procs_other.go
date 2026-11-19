//go:build evm_devnet && !linux

package devnet

import "syscall"

// dieWithTest: no parent-death signal outside Linux; Stop and the context
// still end the helper on a clean exit.
func dieWithTest() *syscall.SysProcAttr { return nil }
