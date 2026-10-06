package devnet

import "testing"

// Lanes run side by side at distinct DEVNET_PORT_OFFSET values. The offset was
// applied twice for every devnet built from tssTestConfig (once in DefaultConfig,
// again on top), so a lane at 3000 bound the ports of a lane at 6000 and the other
// lane's regression died at setup (2026-10-06: "Bind for 127.0.0.1:24057 failed:
// port is already allocated").
func TestDevnetPortOffsetAppliedOnce(t *testing.T) {
	t.Setenv("DEVNET_PORT_OFFSET", "")
	base := DefaultConfig()
	t.Setenv("DEVNET_PORT_OFFSET", "3000")
	for name, cfg := range map[string]*Config{"DefaultConfig": DefaultConfig(), "tssTestConfig": tssTestConfig()} {
		for port, got := range map[string][2]int{
			"GQLBasePort":     {base.GQLBasePort, cfg.GQLBasePort},
			"P2PBasePort":     {base.P2PBasePort, cfg.P2PBasePort},
			"MongoPort":       {base.MongoPort, cfg.MongoPort},
			"HivePort":        {base.HivePort, cfg.HivePort},
			"DronePort":       {base.DronePort, cfg.DronePort},
			"BitcoindRPCPort": {base.BitcoindRPCPort, cfg.BitcoindRPCPort},
			"DashdRPCPort":    {base.DashdRPCPort, cfg.DashdRPCPort},
		} {
			if got[1] != got[0]+3000 {
				t.Errorf("%s %s = %d, want base %d + 3000", name, port, got[1], got[0])
			}
		}
	}
}
