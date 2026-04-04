package devnet

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// Partition blocks all network traffic between two magi nodes
// (bidirectional). Both nodes must have NET_ADMIN capability.
// nodeA and nodeB are 1-indexed node numbers.
func (d *Devnet) Partition(ctx context.Context, nodeA, nodeB int) error {
	nameA := fmt.Sprintf("magi-%d", nodeA)
	nameB := fmt.Sprintf("magi-%d", nodeB)

	ipA, err := d.containerIP(ctx, nameA)
	if err != nil {
		return fmt.Errorf("getting IP for %s: %w", nameA, err)
	}
	ipB, err := d.containerIP(ctx, nameB)
	if err != nil {
		return fmt.Errorf("getting IP for %s: %w", nameB, err)
	}

	log.Printf("[devnet] partitioning %s (%s) <-> %s (%s)", nameA, ipA, nameB, ipB)

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
	nameA := fmt.Sprintf("magi-%d", nodeA)
	nameB := fmt.Sprintf("magi-%d", nodeB)

	ipA, err := d.containerIP(ctx, nameA)
	if err != nil {
		return fmt.Errorf("getting IP for %s: %w", nameA, err)
	}
	ipB, err := d.containerIP(ctx, nameB)
	if err != nil {
		return fmt.Errorf("getting IP for %s: %w", nameB, err)
	}

	log.Printf("[devnet] healing %s <-> %s", nameA, nameB)

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
	name := fmt.Sprintf("magi-%d", node)
	log.Printf("[devnet] disconnecting %s", name)
	return d.iptables(ctx, name, "-A", "INPUT", "-j", "DROP")
}

// Reconnect restores all network traffic to a previously
// disconnected magi node.
func (d *Devnet) Reconnect(ctx context.Context, node int) error {
	name := fmt.Sprintf("magi-%d", node)
	log.Printf("[devnet] reconnecting %s", name)
	// Flush all rules to restore connectivity
	return d.iptables(ctx, name, "-F", "INPUT")
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
		fmt.Sprintf("{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"),
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
