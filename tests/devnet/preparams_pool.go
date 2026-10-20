package devnet

// Pre-generated TSS pre-parameters for devnet nodes (node side:
// modules/tss/devnet_preparams.go). Each run copies the fixture sets into the
// devnet dir, which every node mounts at /data/devnet, and points the nodes at
// that copy. Nodes claim sets by renaming them, so the fixture itself is never
// consumed and every run starts from the full set.

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const preParamsPoolDir = "preparams-pool"

// seedPreParamsPool copies cfg.PreParamsFixtureDir's *.json sets into the devnet
// dir and sets VSC_DEVNET_PREPARAMS_POOL for every node. It returns the number of
// sets copied.
func seedPreParamsPool(cfg *Config, devnetDir string) (int, error) {
	entries, err := os.ReadDir(cfg.PreParamsFixtureDir)
	if err != nil {
		return 0, err
	}
	dst := filepath.Join(devnetDir, preParamsPoolDir)
	if err := os.MkdirAll(dst, 0o777); err != nil {
		return 0, err
	}
	// The container's app user is not the host user, and claiming renames files.
	if err := os.Chmod(dst, 0o777); err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if err := copyFile(filepath.Join(cfg.PreParamsFixtureDir, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return n, err
		}
		n++
	}
	if n == 0 {
		return 0, fmt.Errorf("no pre-params sets (*.json) in %s", cfg.PreParamsFixtureDir)
	}
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["VSC_DEVNET_PREPARAMS_POOL"] = "/data/devnet/" + preParamsPoolDir
	return n, nil
}

// checkPreParamsPool reports this run's pool use and returns an integrity failure
// for every set a node rejected as invalid. Exhaustion is not a failure (nodes
// fall back to live generation, which is correct), only slower, so it is logged.
func (d *Devnet) checkPreParamsPool() []string {
	if d.cfg.PreParamsFixtureDir == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(d.devnetDir, preParamsPoolDir))
	if err != nil {
		return []string{fmt.Sprintf("pre-params pool unreadable at teardown: %v", err)}
	}
	var claimed, unclaimed int
	var failures []string
	for _, e := range entries {
		switch name := e.Name(); {
		case strings.HasPrefix(name, "invalid-"):
			failures = append(failures, "node rejected pre-params set as invalid: "+name)
		case strings.HasPrefix(name, "claimed-"):
			claimed++
		case strings.HasSuffix(name, ".json"):
			unclaimed++
		}
	}
	log.Printf("[devnet] pre-params pool: %d sets claimed, %d left", claimed, unclaimed)
	if unclaimed == 0 {
		log.Printf("[devnet] WARNING: pre-params pool exhausted, nodes generated the rest live (correct, but slower); grow the fixture")
	}
	return failures
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
