package devnet

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// A run that ends without reaching Stop (Ctrl-C, a kill, a reboot) leaves its
// containers, volumes, the ~780 MB <project>-magi image and its data dir behind,
// and a container that is still up keeps the host ports the next run at the
// same DEVNET_PORT_OFFSET needs. A WSL restart took the FDV10 suite down this
// way mid-test, and an interrupted `go test -short` left 11 containers running.
// Two guards, both set up by Start:
//   - SIGINT, SIGTERM and SIGHUP tear every live devnet down before the test
//     binary exits (go test waits for it). A second signal exits at once.
//   - Each run records its owner process in <data dir>/owner, and Start reaps
//     the runs under <SourceDir>/.devnet whose owner is gone: killed, or from
//     before a reboot.
// A KeepRunning devnet is the exception: it is left up for inspection, so
// neither guard touches it.

const runOwnerFile = "owner"

// runOwner identifies the process that owns a devnet run. A pid alone is not
// enough: the pid may have been reused, so the process start time is kept too,
// and a run from before a reboot is dead whatever its pid.
type runOwner struct {
	Project string `json:"project"`
	Image   string `json:"image"`
	PID     int    `json:"pid"`
	Start   string `json:"start"`   // field 22 of /proc/<pid>/stat
	BootID  string `json:"boot_id"` // /proc/sys/kernel/random/boot_id
	Keep    bool   `json:"keep"`    // Config.KeepRunning: never reaped
}

var (
	procRoot     = "/proc"
	dockerOutput = func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).Output()
		return string(out), err
	}
)

func bootID() (string, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"))
	return strings.TrimSpace(string(b)), err
}

// procStartTime reads a process's start time. The command name in
// /proc/<pid>/stat is in parentheses and may itself contain spaces or
// parentheses, so the fields are counted from the last ')'.
func procStartTime(pid int) (string, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, fmt.Sprint(pid), "stat"))
	if err != nil {
		return "", err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", fmt.Errorf("malformed %s/%d/stat", procRoot, pid)
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3 (state)
	if len(f) < 20 {
		return "", fmt.Errorf("short %s/%d/stat", procRoot, pid)
	}
	return f[19], nil
}

func currentRunOwner(project, image string, keep bool) (runOwner, error) {
	boot, err := bootID()
	if err != nil {
		return runOwner{}, err
	}
	start, err := procStartTime(os.Getpid())
	if err != nil {
		return runOwner{}, err
	}
	return runOwner{Project: project, Image: image, PID: os.Getpid(), Start: start, BootID: boot, Keep: keep}, nil
}

// gone reports whether the owner process no longer exists. Anything it cannot
// read (no /proc) counts as alive: a live run must never be reaped.
func (o runOwner) gone() bool {
	boot, err := bootID()
	if err != nil {
		return false
	}
	if boot != o.BootID {
		return true
	}
	start, err := procStartTime(o.PID)
	if os.IsNotExist(err) {
		return true
	}
	return err == nil && start != o.Start
}

// writeRunOwner records this process as the owner of the run in dataDir.
func writeRunOwner(dataDir string, o runOwner) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dataDir, runOwnerFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dataDir, runOwnerFile))
}

// reapStaleRuns removes what dead runs under <sourceDir>/.devnet left behind.
// A dir without an owner file is skipped: it is either a run between New and
// Start or from before owner files existed.
func reapStaleRuns(sourceDir string) {
	root := filepath.Join(sourceDir, ".devnet")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, runOwnerFile))
		if err != nil {
			continue
		}
		var o runOwner
		if err := json.Unmarshal(b, &o); err != nil || o.Project == "" || !o.gone() {
			continue
		}
		if o.Keep {
			log.Printf("[devnet] leaving KeepRunning run %s (owner pid %d gone); remove it with: docker compose -p %s down -v", o.Project, o.PID, o.Project)
			continue
		}
		log.Printf("[devnet] reaping %s: its owner (pid %d) is gone", o.Project, o.PID)
		if err := removeComposeProject(o.Project); err != nil {
			log.Printf("[devnet] warning: reaping %s: %v", o.Project, err)
			continue // keep the owner file so the next Start tries again
		}
		if o.Image != "" {
			dockerOutput("image", "rm", "-f", o.Image) // best-effort
		}
		removeRunDataDir(dir)
	}
}

// removeComposeProject removes a compose project's containers, then its volumes
// and networks, found by the project label compose puts on each. `docker
// compose -p <project> down` cannot be trusted for this without the run's
// compose files: on containers missing some of compose's labels it removes
// nothing and still exits 0.
func removeComposeProject(project string) error {
	filter := "label=com.docker.compose.project=" + project
	for _, k := range [][2][]string{
		{{"ps", "-aq", "--filter", filter}, {"rm", "-fv"}},
		{{"volume", "ls", "-q", "--filter", filter}, {"volume", "rm", "-f"}},
		{{"network", "ls", "-q", "--filter", filter}, {"network", "rm"}},
	} {
		out, err := dockerOutput(k[0]...)
		if err != nil {
			return fmt.Errorf("docker %s: %w", strings.Join(k[0], " "), err)
		}
		if ids := strings.Fields(out); len(ids) > 0 {
			if _, err := dockerOutput(append(k[1], ids...)...); err != nil {
				return fmt.Errorf("docker %s: %w", strings.Join(k[1], " "), err)
			}
		}
	}
	return nil
}

// removeRunDataDir deletes a run's data dir. HAF writes files owned by its
// container uids, so a throwaway container removes the contents as root.
func removeRunDataDir(dir string) {
	dockerOutput("run", "--rm", "-v", dir+":/cleanup", "alpine", "rm", "-rf", "/cleanup")
	os.RemoveAll(dir)
}

// liveRuns are the devnets this process has started and not yet stopped.
var liveRuns struct {
	sync.Mutex
	runs    map[*Devnet]bool
	watched bool
}

func trackRun(d *Devnet) {
	liveRuns.Lock()
	defer liveRuns.Unlock()
	if liveRuns.runs == nil {
		liveRuns.runs = make(map[*Devnet]bool)
	}
	liveRuns.runs[d] = true
	if !liveRuns.watched {
		liveRuns.watched = true
		ch := make(chan os.Signal, 2)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		go stopRunsOnSignal(ch)
	}
}

func untrackRun(d *Devnet) {
	liveRuns.Lock()
	delete(liveRuns.runs, d)
	liveRuns.Unlock()
}

func stopRunsOnSignal(ch chan os.Signal) {
	sig := <-ch
	// A SIGTERM or SIGHUP to the whole process group (a CI cancel, a closing
	// session) kills the go command, which holds the read end of our output.
	// The next log line would then kill us with SIGPIPE mid-teardown; ignored,
	// the write just fails and the teardown runs to the end.
	signal.Ignore(syscall.SIGPIPE)
	liveRuns.Lock()
	var runs []*Devnet
	for d := range liveRuns.runs {
		if d.cfg.KeepRunning {
			log.Printf("[devnet] %v: leaving KeepRunning devnet %s up; remove it with: docker compose -p %s down -v", sig, d.projectName, d.projectName)
			continue
		}
		runs = append(runs, d)
	}
	liveRuns.Unlock()
	log.Printf("[devnet] %v: tearing down %d devnet(s) before exiting (signal again to exit now; the next Start reaps what is left)", sig, len(runs))
	go func() {
		<-ch
		log.Printf("[devnet] second signal: exiting without teardown")
		os.Exit(1)
	}()
	var wg sync.WaitGroup
	for _, d := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Stop()
		}()
	}
	wg.Wait()
	os.Exit(1)
}
