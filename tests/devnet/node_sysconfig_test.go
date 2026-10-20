package devnet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

// One node can run with its own parameters: its file is the shared overrides
// with its own fields laid over them, and only that node is pointed at it.
func TestNodeSysConfigOverridesLayerOverTheShared(t *testing.T) {
	shared := params.DevnetTssParams
	shared.ReshareTimeout = 2 * time.Minute
	own := params.DevnetTssParams
	own.ReshareTimeout = 20 * time.Second

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		TssParams:       &shared,
		ConsensusParams: &params.ConsensusParams{ConsensusVersionFloorConsensus: 11},
	}
	cfg.NodeSysConfigOverrides = map[int]*systemconfig.SysConfigOverrides{3: {TssParams: &own}}

	dir := t.TempDir()
	if err := writeSysConfigOverrides(cfg, dir); err != nil {
		t.Fatal(err)
	}
	read := func(name string) systemconfig.SysConfigOverrides {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var o systemconfig.SysConfigOverrides
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	if got := read("sysconfig.json").TssParams.ReshareTimeout; got != 2*time.Minute {
		t.Errorf("shared reshare timeout = %s, want 2m", got)
	}
	n3 := read(nodeSysConfigFile(3))
	if n3.TssParams == nil || n3.TssParams.ReshareTimeout != 20*time.Second {
		t.Errorf("magi-3 reshare timeout = %+v, want its own 20s", n3.TssParams)
	}
	if n3.ConsensusParams == nil || n3.ConsensusParams.ConsensusVersionFloorConsensus != 11 {
		t.Errorf("magi-3 lost the shared consensus params: %+v", n3.ConsensusParams)
	}

	out := filepath.Join(dir, "nodes.yml")
	if err := writeNodesOverride(cfg, dir, "p", "img", out); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	yml := string(raw)
	if strings.Count(yml, `"/data/devnet/sysconfig-3.json"`) != 1 {
		t.Errorf("magi-3 is not pointed at its own sysconfig file:\n%s", yml)
	}
	if strings.Count(yml, `"/data/devnet/sysconfig.json"`) != 4 {
		t.Errorf("the other 4 nodes must use the shared file:\n%s", yml)
	}
}
