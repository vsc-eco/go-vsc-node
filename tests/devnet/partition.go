package devnet

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// containerName returns the Docker container name for a magi node,
// scoped to this devnet's project name.
func (d *Devnet) containerName(node int) string {
	return fmt.Sprintf("%s-magi-%d", d.projectName, node)
}

// Partition blocks all network traffic between two magi nodes
// (bidirectional). Both nodes must have NET_ADMIN capability.
// nodeA and nodeB are 1-indexed node numbers.
func (d *Devnet) Partition(ctx context.Context, nodeA, nodeB int) error {
	nameA := d.containerName(nodeA)
	nameB := d.containerName(nodeB)

	ipA, err := d.containerIP(ctx, nameA)
	if err != nil {
		return fmt.Errorf("getting IP for %s: %w", nameA, err)
	}
	ipB, err := d.containerIP(ctx, nameB)
	if err != nil {
		return fmt.Errorf("getting IP for %s: %w", nameB, err)
	}

	log.Printf("[devnet] partitioning magi-%d (%s) <-> magi-%d (%s)", nodeA, ipA, nodeB, ipB)

	// Block B's traffic arriving at A
	if err := d.iptables(ctx, nameA, "-A", "INPUT", "-s", ipB, "-j", "DROP"); err != nil {
		return err
	}
	if err := d.iptables(ctx, nameA, "-A", "OUTPUT", "-d", ipB, "-j", "DROP"); err != nil {
		return err
	}
	// Block A's traffic arriving at B
	if err := d.iptables(ctx, nameB, "-A", "INPUT", "-s", ipA, "-j", "DROP"); err != nil {
		return err
	}
	if err := d.iptables(ctx, nameB, "-A", "OUTPUT", "-d", ipA, "-j", "DROP"); err != nil {
		return err
	}
	return nil
}

// Heal restores network traffic between two previously partitioned
// magi nodes. Safe to call even if no partition exists.
func (d *Devnet) Heal(ctx context.Context, nodeA, nodeB int) error {
	nameA := d.containerName(nodeA)
	nameB := d.containerName(nodeB)

	ipA, err := d.containerIP(ctx, nameA)
	if err != nil {
		return fmt.Errorf("getting IP for %s: %w", nameA, err)
	}
	ipB, err := d.containerIP(ctx, nameB)
	if err != nil {
		return fmt.Errorf("getting IP for %s: %w", nameB, err)
	}

	log.Printf("[devnet] healing magi-%d <-> magi-%d", nodeA, nodeB)

	// Remove rules (ignore errors — rule may not exist)
	d.iptables(ctx, nameA, "-D", "INPUT", "-s", ipB, "-j", "DROP")
	d.iptables(ctx, nameA, "-D", "OUTPUT", "-d", ipB, "-j", "DROP")
	d.iptables(ctx, nameB, "-D", "INPUT", "-s", ipA, "-j", "DROP")
	d.iptables(ctx, nameB, "-D", "OUTPUT", "-d", ipA, "-j", "DROP")
	return nil
}

// Disconnect drops all network traffic to/from a magi node,
// isolating it from every other container.
func (d *Devnet) Disconnect(ctx context.Context, node int) error {
	name := d.containerName(node)
	log.Printf("[devnet] disconnecting magi-%d", node)
	return d.iptables(ctx, name, "-A", "INPUT", "-j", "DROP")
}

// Reconnect restores all network traffic to a previously
// disconnected magi node.
func (d *Devnet) Reconnect(ctx context.Context, node int) error {
	name := d.containerName(node)
	log.Printf("[devnet] reconnecting magi-%d", node)
	// Flush all rules to restore connectivity
	return d.iptables(ctx, name, "-F", "INPUT")
}

// AddLatency adds network delay to traffic between two specific nodes
// (bidirectional). The delay is in milliseconds with optional jitter.
// This is more realistic than a full partition — it causes readiness
// check timeouts on some nodes but not others, triggering the SSID
// mismatch bug where checkParticipantReadiness produces different
// party lists on different nodes.
func (d *Devnet) AddLatency(ctx context.Context, nodeA, nodeB int, delayMs, jitterMs int) error {
	nameA := d.containerName(nodeA)
	nameB := d.containerName(nodeB)

	ipA, err := d.containerIP(ctx, nameA)
	if err != nil {
		return fmt.Errorf("getting IP for %s: %w", nameA, err)
	}
	ipB, err := d.containerIP(ctx, nameB)
	if err != nil {
		return fmt.Errorf("getting IP for %s: %w", nameB, err)
	}

	log.Printf("[devnet] adding %dms±%dms latency: magi-%d <-> magi-%d", delayMs, jitterMs, nodeA, nodeB)

	// Add latency on A for traffic to/from B
	if err := d.addNetemForIP(ctx, nameA, ipB, delayMs, jitterMs); err != nil {
		return fmt.Errorf("adding latency on magi-%d: %w", nodeA, err)
	}
	// Add latency on B for traffic to/from A
	if err := d.addNetemForIP(ctx, nameB, ipA, delayMs, jitterMs); err != nil {
		return fmt.Errorf("adding latency on magi-%d: %w", nodeB, err)
	}
	return nil
}

// AddOutboundLatency adds one-directional delay to traffic FROM nodeA
// TO nodeB. Traffic from B to A is unaffected. This is useful when
// you want nodeA to appear slow to nodeB without making nodeB appear
// slow to nodeA.
func (d *Devnet) AddOutboundLatency(ctx context.Context, fromNode, toNode int, delayMs, jitterMs int) error {
	nameFrom := d.containerName(fromNode)
	ipTo, err := d.containerIP(ctx, d.containerName(toNode))
	if err != nil {
		return fmt.Errorf("getting IP for magi-%d: %w", toNode, err)
	}

	log.Printf("[devnet] adding %dms±%dms outbound latency: magi-%d → magi-%d", delayMs, jitterMs, fromNode, toNode)
	return d.addNetemForIP(ctx, nameFrom, ipTo, delayMs, jitterMs)
}

// RemoveLatency removes any tc latency rules from a node, restoring
// normal network behavior.
func (d *Devnet) RemoveLatency(ctx context.Context, node int) error {
	name := d.containerName(node)
	log.Printf("[devnet] removing latency from magi-%d", node)
	// Delete the root qdisc — this removes all tc rules.
	// Ignore errors (may not have any rules).
	d.tcExec(ctx, name, "qdisc", "del", "dev", "eth0", "root")
	return nil
}

// addNetemForIP sets up tc netem delay for traffic to a specific IP.
// Uses a prio qdisc with a u32 filter to match only the target IP.
func (d *Devnet) addNetemForIP(ctx context.Context, container, targetIP string, delayMs, jitterMs int) error {
	// Create a prio qdisc as root (3 bands by default)
	if err := d.tcExec(ctx, container,
		"qdisc", "add", "dev", "eth0", "root", "handle", "1:", "prio",
		"bands", "3", "priomap", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0",
	); err != nil {
		// May already exist from a previous call; try replacing
		d.tcExec(ctx, container, "qdisc", "del", "dev", "eth0", "root")
		if err := d.tcExec(ctx, container,
			"qdisc", "add", "dev", "eth0", "root", "handle", "1:", "prio",
			"bands", "3", "priomap", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0",
		); err != nil {
			return err
		}
	}

	// Add netem delay on band 2 (1:2)
	delay := fmt.Sprintf("%dms", delayMs)
	jitter := fmt.Sprintf("%dms", jitterMs)
	if err := d.tcExec(ctx, container,
		"qdisc", "add", "dev", "eth0", "parent", "1:2", "handle", "20:",
		"netem", "delay", delay, jitter,
	); err != nil {
		return err
	}

	// Filter: match destination IP → send to band 2 (netem)
	if err := d.tcExec(ctx, container,
		"filter", "add", "dev", "eth0", "parent", "1:0", "protocol", "ip",
		"u32", "match", "ip", "dst", targetIP+"/32", "flowid", "1:2",
	); err != nil {
		return err
	}

	return nil
}

// tcExec runs a tc command inside a container.
func (d *Devnet) tcExec(ctx context.Context, container string, args ...string) error {
	fullArgs := append([]string{"exec", "-u", "root", container, "tc"}, args...)
	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tc in %s (%v): %s", container, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// iptables runs an iptables command inside a container.
func (d *Devnet) iptables(ctx context.Context, container string, args ...string) error {
	fullArgs := append([]string{"exec", "-u", "root", container, "iptables"}, args...)
	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables in %s (%v): %s", container, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// containerIP returns the IP address of a container on the devnet network.
func (d *Devnet) containerIP(ctx context.Context, container string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
		container,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("no IP found for container %s", container)
	}
	return ip, nil
}
