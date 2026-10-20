package devnet

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// ephemeralPortRangeFile is where Linux keeps the range it picks source ports
// from for outgoing connections.
const ephemeralPortRangeFile = "/proc/sys/net/ipv4/ip_local_port_range"

// hostPorts names every host port the devnet publishes (see compose.go).
func (c *Config) hostPorts() map[string]int {
	ports := map[string]int{
		"hive":         c.HivePort,
		"mongo":        c.MongoPort,
		"drone":        c.DronePort,
		"bitcoind rpc": c.BitcoindRPCPort,
		"dashd rpc":    c.DashdRPCPort,
	}
	for i := 1; i <= c.Nodes; i++ {
		ports[fmt.Sprintf("magi-%d gql", i)] = c.GQLBasePort + i - 1
		ports[fmt.Sprintf("magi-%d p2p", i)] = c.P2PBasePort + i - 1
	}
	return ports
}

// checkHostPorts refuses host ports inside the kernel's ephemeral port range.
//
// While a test has a node stopped, nothing listens on that node's host ports,
// and any outgoing connection may take one as its source port. Dials to the
// stopped node's own port eventually pick that same port as the source and
// connect the socket to itself (TCP simultaneous open). Go notices and closes
// it, the socket then sits in TIME_WAIT for 60s, and restarting the node fails
// with "failed to bind host port ...: address already in use". The FastBlocks
// pacemaker probes every node every ~15ms, which walks the range in about a
// minute: the FDV5 suite lost magi-5 (port 33084) this way in two tests in a
// row at DEVNET_PORT_OFFSET=15000. The kernel never hands out a port outside
// the range as a source port.
func checkHostPorts(ports map[string]int, rangeFile string) error {
	raw, err := os.ReadFile(rangeFile)
	if err != nil {
		return nil // not Linux: nothing to check against
	}
	var lo, hi int
	if _, err := fmt.Sscan(string(raw), &lo, &hi); err != nil {
		return nil
	}
	var bad []string
	for name, port := range ports {
		if port >= lo && port <= hi {
			bad = append(bad, fmt.Sprintf("%s %d", name, port))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("host ports inside the ephemeral port range %d-%d (%s): an outgoing connection can take one while its node is stopped and the restart fails to bind; lower DEVNET_PORT_OFFSET so every host port stays below %d",
		lo, hi, strings.Join(bad, ", "), lo)
}
