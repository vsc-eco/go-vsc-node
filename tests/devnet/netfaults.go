package devnet

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
)

// netFaultTracker records the network faults the harness has put into node
// containers (iptables rules, tc root qdiscs) so the FastBlocks pacemaker holds
// real cadence while any is in effect. A partitioned connection stays "up" until
// its keepalive gives out and a reconnect waits on dial timers, all on wall
// clock; fast blocks there would let far more chain pass than on a live chain.
// It sees every rule because it hooks the two exec wrappers (iptables, tcExec),
// including tests that call them directly.
type netFaultTracker struct {
	mu    sync.Mutex
	rules map[string]map[string]int // container -> table/chain -> rules added
	tc    map[string]bool           // container -> root qdisc installed
}

// noteIptables records a successful iptables call.
func (f *netFaultTracker) noteIptables(container string, args []string) {
	table := "filter"
	if len(args) >= 2 && (args[0] == "-t" || args[0] == "--table") {
		table, args = args[1], args[2:]
	}
	if len(args) == 0 {
		return
	}
	chain := ""
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		chain = table + "/" + args[1]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rules == nil {
		f.rules = make(map[string]map[string]int)
	}
	chains := f.rules[container]
	if chains == nil {
		chains = make(map[string]int)
		f.rules[container] = chains
	}
	switch args[0] {
	case "-A", "--append", "-I", "--insert":
		if chain != "" {
			chains[chain]++
		}
	case "-D", "--delete":
		if chain != "" && chains[chain] > 0 {
			chains[chain]--
		}
	case "-F", "--flush":
		if chain != "" {
			delete(chains, chain)
			return
		}
		for c := range chains {
			if strings.HasPrefix(c, table+"/") {
				delete(chains, c)
			}
		}
	}
}

// noteTc records a successful tc call. Only the root qdisc matters: netem and
// filters hang off it, and deleting it removes them all.
func (f *netFaultTracker) noteTc(container string, args []string) {
	if len(args) < 2 || args[0] != "qdisc" || !slices.Contains(args, "root") {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tc == nil {
		f.tc = make(map[string]bool)
	}
	switch args[1] {
	case "add", "replace", "change":
		f.tc[container] = true
	case "del", "delete":
		delete(f.tc, container)
	}
}

// active names the faults in effect ("" when none).
func (f *netFaultTracker) active() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for container, chains := range f.rules {
		for chain, n := range chains {
			if n > 0 {
				out = append(out, fmt.Sprintf("%s:iptables %s", shortContainer(container), chain))
			}
		}
	}
	for container := range f.tc {
		out = append(out, shortContainer(container)+":tc")
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// shortContainer turns "<project>-magi-3" into "magi-3".
func shortContainer(name string) string {
	if i := strings.LastIndex(name, "magi-"); i >= 0 {
		return name[i:]
	}
	return name
}
