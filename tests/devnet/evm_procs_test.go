//go:build evm_devnet && linux

package devnet

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// SUITE-4: two anvils orphaned by a signalled run kept mining on 28545 and
// 28547. A later wave given 28545 could not bind it, StartAnvil's readiness
// check was answered by the orphan, and the bot started from block 0 of a
// 20,000-block chain at 100 blocks a tick (phase H W5, both arms).

func TestSUITE4_AnvilPortSkipsAPortInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	busy := l.Addr().(*net.TCPAddr).Port
	saved := atomic.LoadInt32(&anvilPortSeq)
	defer atomic.StoreInt32(&anvilPortSeq, saved)
	atomic.StoreInt32(&anvilPortSeq, int32(busy-1))
	if got := nextAnvilPort(); got == strconv.Itoa(busy) {
		t.Fatalf("nextAnvilPort returned port %d, which is in use", busy)
	}
	if portFree(strconv.Itoa(busy)) {
		t.Fatal("portFree says a listening port is free")
	}
}

// The helper process started with dieWithTest dies with its parent.
func TestSUITE4_HelperDiesWithTheTest(t *testing.T) {
	if os.Getenv("SUITE4_PARENT") == "1" {
		child := exec.Command("sleep", "300")
		child.SysProcAttr = dieWithTest()
		if err := child.Start(); err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		fmt.Println("CHILD", child.Process.Pid)
		os.Exit(0) // the parent goes away without stopping the child
	}
	parent := exec.Command(os.Args[0], "-test.run=^TestSUITE4_HelperDiesWithTheTest$")
	parent.Env = append(os.Environ(), "SUITE4_PARENT=1")
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	pid := 0
	for sc := bufio.NewScanner(out); sc.Scan(); {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[0] == "CHILD" {
			pid, _ = strconv.Atoi(f[1])
		}
	}
	_ = parent.Wait()
	if pid == 0 {
		t.Fatal("the parent never reported its child")
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) { // wall: an OS process exiting, not chain time
		if err := syscall.Kill(pid, 0); err != nil {
			return // gone
		}
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil && strings.Contains(string(b), ") Z ") {
			return // dead, waiting to be reaped
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("child %d outlived its parent", pid)
}
