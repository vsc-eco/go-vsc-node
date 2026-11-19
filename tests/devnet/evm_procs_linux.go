//go:build evm_devnet && linux

package devnet

import "syscall"

// dieWithTest has a helper process (anvil, the bot) killed when the test
// binary exits, so a run that is signalled or crashes leaves no orphan behind
// to answer on its port in the next run.
func dieWithTest() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
