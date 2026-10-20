package devnet

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The call sequences are the ones partition.go and poa8_review_devnet_test.go
// send.
func TestNetFaultTrackerFollowsTheHarnessCalls(t *testing.T) {
	const a, b = "devnet-test-x-magi-1", "devnet-test-x-magi-2"
	var f netFaultTracker
	if got := f.active(); got != "" {
		t.Fatalf("fresh tracker: %q", got)
	}

	// Partition(1, 2), then Heal(1, 2).
	for _, c := range []string{a, b} {
		f.noteIptables(c, []string{"-A", "INPUT", "-s", "10.0.0.9", "-j", "DROP"})
		f.noteIptables(c, []string{"-A", "OUTPUT", "-d", "10.0.0.9", "-j", "DROP"})
	}
	if got := f.active(); got != "magi-1:iptables filter/INPUT,magi-1:iptables filter/OUTPUT,magi-2:iptables filter/INPUT,magi-2:iptables filter/OUTPUT" {
		t.Fatalf("after Partition: %q", got)
	}
	for _, c := range []string{a, b} {
		f.noteIptables(c, []string{"-D", "INPUT", "-s", "10.0.0.9", "-j", "DROP"})
		f.noteIptables(c, []string{"-D", "OUTPUT", "-d", "10.0.0.9", "-j", "DROP"})
	}
	if got := f.active(); got != "" {
		t.Fatalf("after Heal: %q", got)
	}

	// Disconnect(1) from 4 peers, Reconnect(1) flushes both chains.
	for i := 0; i < 4; i++ {
		f.noteIptables(a, []string{"-A", "INPUT", "-s", "10.0.0.9", "-j", "DROP"})
		f.noteIptables(a, []string{"-A", "OUTPUT", "-d", "10.0.0.9", "-j", "DROP"})
	}
	f.noteIptables(a, []string{"-F", "INPUT"})
	if got := f.active(); got != "magi-1:iptables filter/OUTPUT" {
		t.Fatalf("after flushing INPUT only: %q", got)
	}
	f.noteIptables(a, []string{"-F", "OUTPUT"})
	if got := f.active(); got != "" {
		t.Fatalf("after Reconnect: %q", got)
	}

	// A test's own rule, cleared by a bare flush of the table.
	f.noteIptables(b, []string{"-A", "OUTPUT", "-d", "10.0.0.9", "-m", "length", "--length", "1000:65535", "-j", "DROP"})
	f.noteIptables(b, []string{"-t", "nat", "-A", "PREROUTING", "-j", "ACCEPT"})
	f.noteIptables(b, []string{"-F"})
	if got := f.active(); got != "magi-2:iptables nat/PREROUTING" {
		t.Fatalf("a bare -F flushes the filter table only: %q", got)
	}
	f.noteIptables(b, []string{"-t", "nat", "-F"})
	if got := f.active(); got != "" {
		t.Fatalf("after flushing nat: %q", got)
	}

	// An extra delete never goes negative and hides a later rule.
	f.noteIptables(a, []string{"-D", "INPUT", "-s", "10.0.0.9", "-j", "DROP"})
	f.noteIptables(a, []string{"-A", "INPUT", "-s", "10.0.0.9", "-j", "DROP"})
	if got := f.active(); got != "magi-1:iptables filter/INPUT" {
		t.Fatalf("delete before add: %q", got)
	}
	f.noteIptables(a, []string{"-F", "INPUT"})

	// AddLatency / AddOutboundLatency, then RemoveLatency.
	f.noteTc(a, []string{"qdisc", "add", "dev", "eth0", "root", "handle", "1:", "prio"})
	f.noteTc(a, []string{"qdisc", "add", "dev", "eth0", "parent", "1:2", "handle", "20:", "netem", "delay", "200ms", "20ms"})
	f.noteTc(a, []string{"filter", "add", "dev", "eth0", "parent", "1:0", "protocol", "ip", "u32", "match", "ip", "dst", "10.0.0.9/32", "flowid", "1:2"})
	if got := f.active(); got != "magi-1:tc" {
		t.Fatalf("after AddLatency: %q", got)
	}
	// Removing only a child qdisc leaves the root (and any other netem) in place.
	f.noteTc(a, []string{"qdisc", "del", "dev", "eth0", "parent", "1:2", "handle", "20:"})
	if got := f.active(); got != "magi-1:tc" {
		t.Fatalf("after deleting a child qdisc: %q", got)
	}
	f.noteTc(a, []string{"qdisc", "del", "dev", "eth0", "root"})
	if got := f.active(); got != "" {
		t.Fatalf("after RemoveLatency: %q", got)
	}
}

// The real exec wrappers feed the tracker and the pacemaker reads it, so a fault
// a test injects (through Partition or a direct d.iptables call) holds real
// cadence. docker is a stand-in on PATH that accepts every command.
func TestNetFaultsReachThePacemaker(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pcfg := d.pacemakerConfig()
	if pcfg.lead != &testClockLead {
		t.Fatalf("the pacemaker must drive the package test clock")
	}
	held := pcfg.held
	// With no fault in effect the hold is the test itself, running its own code
	// rather than waiting on the test clock (harnessHold); a fault names itself.
	if held == nil || held() != "test running" {
		t.Fatalf("fresh devnet: pacemaker hold must be wired and name only the running test")
	}
	ctx := context.Background()
	name := d.containerName(3)
	if err := d.iptables(ctx, name, "-A", "OUTPUT", "-d", "10.0.0.9", "-j", "DROP"); err != nil {
		t.Fatal(err)
	}
	if got := held(); got != "magi-3:iptables filter/OUTPUT" {
		t.Fatalf("after d.iptables -A: pacemaker sees %q", got)
	}
	if err := d.iptables(ctx, name, "-F", "OUTPUT"); err != nil {
		t.Fatal(err)
	}
	if err := d.tcExec(ctx, name, "qdisc", "add", "dev", "eth0", "root", "handle", "1:", "prio"); err != nil {
		t.Fatal(err)
	}
	if got := held(); got != "magi-3:tc" {
		t.Fatalf("after d.tcExec qdisc add root: pacemaker sees %q", got)
	}
	if err := d.RemoveLatency(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if got := held(); got != "test running" {
		t.Fatalf("after RemoveLatency: pacemaker sees %q", got)
	}
}
