package devnet

// Unit tests for the FastBlocks / pre-params-pool harness plumbing. No Docker.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped hived config must keep the line enableHivedDebugPlugins extends;
// otherwise FastBlocks would fail every run at startup.
func TestEnableHivedDebugPluginsOnShippedConfig(t *testing.T) {
	shipped, err := testdataFS.ReadFile("testdata/config.ini")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.ini"), shipped, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := enableHivedDebugPlugins(dir); err != nil {
		t.Fatalf("shipped config.ini: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "config.ini"))
	if !strings.Contains(string(got), "plugin = witness debug_node debug_node_api ") {
		t.Fatalf("debug plugins not enabled:\n%s", got)
	}
	if strings.Count(string(got), "debug_node_api") != 1 {
		t.Fatal("debug_node_api added more than once")
	}
	if strings.Contains(string(shipped), "debug_node") {
		t.Fatal("the shipped config must not enable debug plugins for normal runs")
	}
}

func TestEnableHivedDebugPluginsRefusesUnknownConfig(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "config.ini"), []byte("plugin = p2p webserver\n"), 0o644)
	if err := enableHivedDebugPlugins(dir); err == nil {
		t.Fatal("want an error when the plugin line is not recognized")
	}
}

func TestConfigRefusesFastBlocksWithOldCodeNodes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FastBlocks = true
	cfg.OldCodeNodes = []int{2}
	if err := cfg.Validate(); err == nil {
		t.Fatal("FastBlocks with OldCodeNodes must be refused: old images have no quiescence probe")
	}
	cfg.FastBlocks = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("OldCodeNodes alone must stay valid: %v", err)
	}
}

func TestSeedPreParamsPool(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for _, name := range []string{"set-0001.json", "set-0002.json", "README.md"} {
		_ = os.WriteFile(filepath.Join(src, name), []byte("{}"), 0o644)
	}
	cfg := &Config{PreParamsFixtureDir: src}
	n, err := seedPreParamsPool(cfg, dst)
	if err != nil || n != 2 {
		t.Fatalf("want 2 sets seeded, got %d (%v)", n, err)
	}
	if cfg.MagiEnv["VSC_DEVNET_PREPARAMS_POOL"] != "/data/devnet/"+preParamsPoolDir {
		t.Fatalf("pool env not set: %v", cfg.MagiEnv)
	}
	if _, err := os.Stat(filepath.Join(dst, preParamsPoolDir, "README.md")); err == nil {
		t.Fatal("non-set files must not be copied into the pool")
	}
	// The fixture is copied, never consumed.
	if _, err := os.Stat(filepath.Join(src, "set-0001.json")); err != nil {
		t.Fatal("seeding must not move fixture files")
	}
}

func TestSeedPreParamsPoolRefusesEmptyFixture(t *testing.T) {
	cfg := &Config{PreParamsFixtureDir: t.TempDir()}
	if _, err := seedPreParamsPool(cfg, t.TempDir()); err == nil {
		t.Fatal("an empty fixture must be an error, not a silent fall back to live generation")
	}
}

func TestCheckPreParamsPool(t *testing.T) {
	devnetDir := t.TempDir()
	pool := filepath.Join(devnetDir, preParamsPoolDir)
	_ = os.MkdirAll(pool, 0o755)
	for _, name := range []string{"claimed-magi-1-set-0001.json", "set-0002.json"} {
		_ = os.WriteFile(filepath.Join(pool, name), []byte("{}"), 0o644)
	}
	d := &Devnet{cfg: &Config{PreParamsFixtureDir: "/fixture"}, devnetDir: devnetDir}
	if f := d.checkPreParamsPool(); len(f) != 0 {
		t.Fatalf("a healthy pool is not a failure: %v", f)
	}
	_ = os.WriteFile(filepath.Join(pool, "invalid-magi-2-set-0003.json"), []byte("{}"), 0o644)
	if f := d.checkPreParamsPool(); len(f) != 1 || !strings.Contains(f[0], "invalid-magi-2-set-0003.json") {
		t.Fatalf("an invalid set must be an integrity failure, got %v", f)
	}
	_ = os.RemoveAll(pool)
	if f := d.checkPreParamsPool(); len(f) != 1 {
		t.Fatalf("an unreadable pool must be an integrity failure, got %v", f)
	}
}

func TestIntegrityErrorIsDetectable(t *testing.T) {
	var err error = &IntegrityError{Project: "p", Failures: []string{"x"}}
	var ie *IntegrityError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "x") {
		t.Fatal("IntegrityError must be detectable with errors.As")
	}
}

// The package TestMain must fail the run when any integrity failure was recorded,
// even though the test that recorded it passed: re-run this test binary with only
// a child test that records one and passes.
func TestIntegrityBackstopFailsTheRun(t *testing.T) {
	if os.Getenv("INTEGRITY_BACKSTOP_CHILD") == "1" {
		t.Skip("running as the child")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestIntegrityBackstopChild$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "INTEGRITY_BACKSTOP_CHILD=1")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("a run with a recorded integrity failure exited %v, want non-zero:\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestIntegrityBackstopChild") {
		t.Fatalf("the child test itself must pass, so only TestMain can fail the run:\n%s", out)
	}
	if !strings.Contains(string(out), "FAST-MODE INTEGRITY FAILURES") {
		t.Fatalf("TestMain must name the failures:\n%s", out)
	}
}

func TestIntegrityBackstopChild(t *testing.T) {
	if os.Getenv("INTEGRITY_BACKSTOP_CHILD") != "1" {
		t.Skip("only runs as TestIntegrityBackstopFailsTheRun's child process")
	}
	recordIntegrityFailure("backstop-selftest", "deliberate failure")
}

func TestApplyFastModeEnv(t *testing.T) {
	type want struct {
		fast bool
		dir  string
	}
	cases := []struct {
		name     string
		fastEnv  string
		dirEnv   string
		cfg      Config
		expected want
	}{
		{"unset", "", "", Config{}, want{false, ""}},
		{"suite switch", "1", "", Config{}, want{true, ""}},
		{"other value is off", "true", "", Config{}, want{false, ""}},
		{"real cadence test", "1", "", Config{RealCadence: true}, want{false, ""}},
		{"old-code nodes", "1", "", Config{OldCodeNodes: []int{2}}, want{false, ""}},
		{"test chose fast itself", "", "", Config{FastBlocks: true}, want{true, ""}},
		{"fixture at real cadence too", "", "/fx", Config{}, want{false, "/fx"}},
		{"fixture with fast", "1", "/fx", Config{}, want{true, "/fx"}},
		{"test's own fixture wins", "1", "/fx", Config{PreParamsFixtureDir: "/mine"}, want{true, "/mine"}},
		{"live pre-params test ignores the suite fixture", "1", "/fx", Config{LivePreParams: true}, want{true, ""}},
		{"live pre-params beats the test's own fixture", "", "", Config{LivePreParams: true, PreParamsFixtureDir: "/mine"}, want{false, ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("DEVNET_FASTBLOCKS", c.fastEnv)
			t.Setenv("DEVNET_PREPARAMS_DIR", c.dirEnv)
			cfg := c.cfg
			applyFastModeEnv(&cfg)
			if got := (want{cfg.FastBlocks, cfg.PreParamsFixtureDir}); got != c.expected {
				t.Fatalf("got FastBlocks=%v fixture=%q, want %v %q", got.fast, got.dir, c.expected.fast, c.expected.dir)
			}
		})
	}
}
