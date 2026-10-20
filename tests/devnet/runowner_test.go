package devnet

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeProc builds a /proc with one boot id and the given pid -> stat lines.
func fakeProc(t *testing.T, boot string, stats map[int]string) {
	t.Helper()
	root := t.TempDir()
	if boot != "" {
		dir := filepath.Join(root, "sys", "kernel", "random")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "boot_id"), []byte(boot+"\n"), 0o644)
	}
	for pid, stat := range stats {
		os.MkdirAll(filepath.Join(root, fmt.Sprint(pid)), 0o755)
		os.WriteFile(filepath.Join(root, fmt.Sprint(pid), "stat"), []byte(stat), 0o644)
	}
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
}

// statLine is a /proc/<pid>/stat line whose start time (field 22) is start.
func statLine(pid int, comm, start string) string {
	f := make([]string, 50)
	for i := range f {
		f[i] = "0"
	}
	f[0] = "S"
	f[19] = start
	return fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(f, " "))
}

func TestRunOwnerGone(t *testing.T) {
	owner := runOwner{Project: "p", PID: 100, Start: "5000", BootID: "boot-a"}
	cases := []struct {
		name  string
		boot  string
		stats map[int]string
		gone  bool
	}{
		{"owner running", "boot-a", map[int]string{100: statLine(100, "devnet.test", "5000")}, false},
		{"command name with spaces and parens", "boot-a", map[int]string{100: statLine(100, "a (b) c", "5000")}, false},
		{"owner exited", "boot-a", nil, true},
		{"pid reused by another process", "boot-a", map[int]string{100: statLine(100, "bash", "7777")}, true},
		{"from before a reboot", "boot-b", map[int]string{100: statLine(100, "devnet.test", "5000")}, true},
		{"no /proc to tell: never reaped", "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeProc(t, c.boot, c.stats)
			if got := owner.gone(); got != c.gone {
				t.Fatalf("gone() = %v, want %v", got, c.gone)
			}
		})
	}
}

func TestThisProcessOwnsALiveRun(t *testing.T) {
	o, err := currentRunOwner("p", "p-magi", false)
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	if o.gone() {
		t.Fatalf("this process's own run reads as gone: %+v", o)
	}
}

// reapFixture makes <src>/.devnet/<name> for each owner (nil: no owner file).
func reapFixture(t *testing.T, owners map[string]*runOwner) string {
	t.Helper()
	src := t.TempDir()
	for name, o := range owners {
		dir := filepath.Join(src, ".devnet", name)
		os.MkdirAll(filepath.Join(dir, "haf-data"), 0o755)
		if o != nil {
			if err := writeRunOwner(dir, *o); err != nil {
				t.Fatal(err)
			}
		}
	}
	return src
}

// stubDocker records docker calls; each list (ps/ls) call reports one id.
func stubDocker(t *testing.T, fail func(args []string) bool) *[]string {
	t.Helper()
	var calls []string
	old := dockerOutput
	dockerOutput = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if fail != nil && fail(args) {
			return "", errors.New("docker failed")
		}
		if args[0] == "ps" || (len(args) > 1 && args[1] == "ls") {
			return "id-" + args[0] + "\n", nil
		}
		return "", nil
	}
	t.Cleanup(func() { dockerOutput = old })
	return &calls
}

func TestReapStaleRunsRemovesOnlyDeadRuns(t *testing.T) {
	fakeProc(t, "boot-a", map[int]string{200: statLine(200, "devnet.test", "9000")})
	src := reapFixture(t, map[string]*runOwner{
		"dead":     {Project: "devnet-test-dead", Image: "devnet-test-dead-magi", PID: 100, Start: "5000", BootID: "boot-a"},
		"rebooted": {Project: "devnet-test-rebooted", Image: "devnet-test-rebooted-magi", PID: 200, Start: "9000", BootID: "boot-old"},
		"alive":    {Project: "devnet-test-alive", Image: "devnet-test-alive-magi", PID: 200, Start: "9000", BootID: "boot-a"},
		"kept":     {Project: "devnet-test-kept", PID: 100, Start: "5000", BootID: "boot-a", Keep: true},
		"starting": nil,
	})
	calls := stubDocker(t, nil)
	reapStaleRuns(src)

	for _, name := range []string{"dead", "rebooted"} {
		p := "devnet-test-" + name
		f := "--filter label=com.docker.compose.project=" + p
		want := []string{
			"ps -aq " + f, "rm -fv id-ps",
			"volume ls -q " + f, "volume rm -f id-volume",
			"network ls -q " + f, "network rm id-network",
			"image rm -f " + p + "-magi",
			"run --rm -v " + filepath.Join(src, ".devnet", name) + ":/cleanup alpine rm -rf /cleanup",
		}
		for _, w := range want {
			if !strings.Contains(strings.Join(*calls, "\n"), w) {
				t.Errorf("reaping %s: no docker call %q in\n%s", name, w, strings.Join(*calls, "\n"))
			}
		}
		if _, err := os.Stat(filepath.Join(src, ".devnet", name)); !os.IsNotExist(err) {
			t.Errorf("reaping %s left its data dir", name)
		}
	}
	for _, name := range []string{"alive", "kept", "starting"} {
		if strings.Contains(strings.Join(*calls, "\n"), "devnet-test-"+name) {
			t.Errorf("%s must not be touched: %s", name, strings.Join(*calls, "\n"))
		}
		if _, err := os.Stat(filepath.Join(src, ".devnet", name, "haf-data")); err != nil {
			t.Errorf("%s lost its data dir: %v", name, err)
		}
	}
	if len(*calls) != 16 {
		t.Errorf("want 16 docker calls (8 per dead run), got %d:\n%s", len(*calls), strings.Join(*calls, "\n"))
	}
}

// When listing the project's containers fails nothing else is removed, so the
// owner record stays and the next Start tries again.
func TestReapStaleRunsRetriesAfterAFailedRemoval(t *testing.T) {
	fakeProc(t, "boot-a", nil)
	src := reapFixture(t, map[string]*runOwner{
		"dead": {Project: "devnet-test-dead", Image: "devnet-test-dead-magi", PID: 100, Start: "5000", BootID: "boot-a"},
	})
	calls := stubDocker(t, func(args []string) bool { return args[0] == "ps" })
	reapStaleRuns(src)
	if len(*calls) != 1 {
		t.Fatalf("after a failed container listing nothing else may run, got:\n%s", strings.Join(*calls, "\n"))
	}
	if _, err := os.Stat(filepath.Join(src, ".devnet", "dead", runOwnerFile)); err != nil {
		t.Fatalf("the owner record must survive a failed reap: %v", err)
	}
}

// End to end against the docker daemon: a leftover the way a killed run leaves
// it (a compose project still up, with its volume and network, the image tag,
// the data dir) is gone after the next reap.
func TestReapStaleRunsRemovesARealLeftover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping docker test in short mode")
	}
	requireDocker(t)
	if exec.Command("docker", "image", "inspect", "alpine").Run() != nil {
		t.Skip("alpine image not present")
	}
	b := make([]byte, 4)
	rand.Read(b)
	p := "devnet-reaptest-" + hex.EncodeToString(b)
	docker := func(args ...string) string {
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	compose := filepath.Join(t.TempDir(), "compose.yml")
	os.WriteFile(compose, []byte("services:\n  x:\n    image: alpine\n    command: [\"sleep\", \"600\"]\n    volumes: [\"db:/data\"]\nvolumes:\n  db: {}\n"), 0o644)
	t.Cleanup(func() { // in case the reaper missed something
		exec.Command("docker", "compose", "-p", p, "-f", compose, "down", "-v").Run()
		exec.Command("docker", "image", "rm", "-f", p+"-magi").Run()
	})
	docker("compose", "-p", p, "-f", compose, "up", "-d")
	docker("tag", "alpine", p+"-magi")

	src := reapFixture(t, map[string]*runOwner{
		"x": {Project: p, Image: p + "-magi", PID: 1, Start: "0", BootID: "a-previous-boot"},
	})
	reapStaleRuns(src)

	if out := docker("ps", "-a", "-q", "--filter", "label=com.docker.compose.project="+p); out != "" {
		t.Errorf("container left: %s", out)
	}
	if out := docker("volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+p); out != "" {
		t.Errorf("volume left: %s", out)
	}
	if out := docker("network", "ls", "-q", "--filter", "label=com.docker.compose.project="+p); out != "" {
		t.Errorf("network left: %s", out)
	}
	if exec.Command("docker", "image", "inspect", p+"-magi").Run() == nil {
		t.Errorf("image %s-magi left", p)
	}
	if _, err := os.Stat(filepath.Join(src, ".devnet", "x")); !os.IsNotExist(err) {
		t.Errorf("data dir left")
	}
}

// signalChild runs TestSignalTeardownChild in a child test binary whose PATH
// holds only a fake docker that records its calls (and sleeps sleepSecs), and
// returns once the child has a live devnet registered. With closedPipe the
// child writes into a pipe whose read end is closed by then, as when the go
// command has already died; its output is lost.
func signalChild(t *testing.T, sleepSecs int, closedPipe bool) (*exec.Cmd, string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	calls := filepath.Join(dir, "docker-calls")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\n/bin/sleep %d\n", calls, sleepSecs)
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	f, _ := os.Create(out)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalTeardownChild$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "RUNOWNER_SIGNAL_CHILD="+dir, "PATH="+bin)
	cmd.Stdout, cmd.Stderr = f, f
	var pr, pw *os.File
	if closedPipe {
		pr, pw, _ = os.Pipe()
		cmd.Stdout, cmd.Stderr = pw, pw
		go io.Copy(io.Discard, pr)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); f.Close() })
	if closedPipe {
		pw.Close()
		defer pr.Close()
	}
	for i := 0; ; i++ {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if i == 200 {
			b, _ := os.ReadFile(out)
			t.Fatalf("child never registered its devnet:\n%s", b)
		}
		time.Sleep(50 * time.Millisecond) // wall: child start-up
	}
	return cmd, out, calls
}

func waitExit(t *testing.T, cmd *exec.Cmd, within time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 0
	case <-time.After(within): // wall: child exit
		t.Fatalf("child still running %v after the signal", within)
		return 0
	}
}

// Ctrl-C (SIGINT) and kill (SIGTERM) tear a live devnet down before the test
// binary exits, instead of leaving its containers running.
func TestSignalTearsDownLiveDevnets(t *testing.T) {
	if os.Getenv("RUNOWNER_SIGNAL_CHILD") != "" {
		t.Skip("running as the child")
	}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd, out, calls := signalChild(t, 0, false)
			cmd.Process.Signal(sig)
			code := waitExit(t, cmd, 20*time.Second)
			o, _ := os.ReadFile(out)
			c, _ := os.ReadFile(calls)
			if code != 1 {
				t.Errorf("exit code %d, want 1", code)
			}
			if !strings.Contains(string(o), "tearing down 1 devnet(s)") || !strings.Contains(string(o), "[devnet] stopped") {
				t.Errorf("no teardown in the child's output:\n%s", o)
			}
			if !strings.Contains(string(c), "-p devnet-test-signalchild") || !strings.Contains(string(c), "down -v --remove-orphans") {
				t.Errorf("teardown never ran compose down for the child's project; docker calls:\n%s", c)
			}
			if strings.Contains(string(o), "--- PASS") {
				t.Errorf("the child test ran to completion; the signal did not end it:\n%s", o)
			}
		})
	}
}

// A signal to the whole process group also kills the go command, which reads
// the test binary's output. The teardown must still run.
func TestSignalTeardownSurvivesAClosedOutputPipe(t *testing.T) {
	if os.Getenv("RUNOWNER_SIGNAL_CHILD") != "" {
		t.Skip("running as the child")
	}
	cmd, _, calls := signalChild(t, 0, true)
	cmd.Process.Signal(syscall.SIGTERM)
	code := waitExit(t, cmd, 20*time.Second)
	c, _ := os.ReadFile(calls)
	if code != 1 || !strings.Contains(string(c), "down -v --remove-orphans") {
		t.Fatalf("exit %d; teardown docker calls:\n%s", code, c)
	}
}

// A second signal exits at once, without waiting for a slow teardown.
func TestSecondSignalExitsAtOnce(t *testing.T) {
	if os.Getenv("RUNOWNER_SIGNAL_CHILD") != "" {
		t.Skip("running as the child")
	}
	cmd, out, _ := signalChild(t, 60, false) // every docker call hangs for a minute
	cmd.Process.Signal(os.Interrupt)
	for i := 0; ; i++ {
		if o, _ := os.ReadFile(out); strings.Contains(string(o), "tearing down") {
			break
		}
		if i == 200 {
			t.Fatal("the first signal never started a teardown")
		}
		time.Sleep(50 * time.Millisecond) // wall: child log
	}
	cmd.Process.Signal(os.Interrupt)
	code := waitExit(t, cmd, 10*time.Second)
	o, _ := os.ReadFile(out)
	if code != 1 || !strings.Contains(string(o), "second signal: exiting without teardown") {
		t.Fatalf("exit %d:\n%s", code, o)
	}
}

func TestSignalTeardownChild(t *testing.T) {
	dir := os.Getenv("RUNOWNER_SIGNAL_CHILD")
	if dir == "" {
		t.Skip("only runs as a signal test's child process")
	}
	cfg := DefaultConfig()
	cfg.DataDir = filepath.Join(dir, "devnet")
	cfg.ProjectName = "devnet-test-signalchild"
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	trackRun(d) // what Start does before its first docker call
	os.WriteFile(filepath.Join(dir, "ready"), nil, 0o644)
	time.Sleep(30 * time.Second) // wall: wait for the parent's signal
}

// A signal's teardown and the test's own cleanup can both call Stop; the
// teardown runs once and both get its result.
func TestStopRunsTeardownOnce(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "docker-calls")
	os.WriteFile(filepath.Join(dir, "docker"), []byte(fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\n/bin/sleep 0.2\n", calls)), 0o755)
	t.Setenv("PATH", dir)
	cfg := DefaultConfig()
	cfg.DataDir = filepath.Join(dir, "devnet")
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- d.Stop() }()
	}
	e1, e2 := <-errs, <-errs
	if e1 != e2 {
		t.Errorf("the two Stop calls returned %v and %v", e1, e2)
	}
	c, _ := os.ReadFile(calls)
	if n := strings.Count(string(c), "down -v --remove-orphans"); n != 1 {
		t.Fatalf("compose down ran %d times, want 1:\n%s", n, c)
	}
}
