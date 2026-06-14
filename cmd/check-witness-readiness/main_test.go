package main

import (
	"reflect"
	"testing"
)

func TestVersion_MeetsMin(t *testing.T) {
	cases := []struct {
		name  string
		v     version
		floor version
		want  bool
	}{
		{"exact match", version{0, 3, 0}, version{0, 3, 0}, true},
		{"major above", version{1, 0, 0}, version{0, 3, 0}, true},
		{"major below", version{0, 9, 9}, version{1, 0, 0}, false},
		{"consensus above", version{0, 4, 0}, version{0, 3, 0}, true},
		{"consensus below", version{0, 2, 9}, version{0, 3, 0}, false},
		{"non_consensus above", version{0, 3, 1}, version{0, 3, 0}, true},
		{"non_consensus below", version{0, 3, 0}, version{0, 3, 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.meetsMin(tc.floor); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

func TestBuildReport_AllReady(t *testing.T) {
	ws := []witness{
		{Account: "w1", Enabled: ptrBool(true), VersionMajor: 0, ProtocolVersion: 3, VersionNonConsensus: 0},
		{Account: "w2", Enabled: ptrBool(true), VersionMajor: 0, ProtocolVersion: 4, VersionNonConsensus: 0},
	}
	r := buildReport(ws, version{0, 3, 0}, false)
	if r.ready != 2 || r.total != 2 || len(r.behindAccounts) != 0 {
		t.Errorf("expected all 2/2 ready, got %+v", r)
	}
}

func TestBuildReport_SomeBehind(t *testing.T) {
	ws := []witness{
		{Account: "w1", Enabled: ptrBool(true), VersionMajor: 0, ProtocolVersion: 3, VersionNonConsensus: 0},
		{Account: "w2", Enabled: ptrBool(true), VersionMajor: 0, ProtocolVersion: 2, VersionNonConsensus: 99},
		{Account: "w3", Enabled: ptrBool(true), VersionMajor: 0, ProtocolVersion: 3, VersionNonConsensus: 0},
	}
	r := buildReport(ws, version{0, 3, 0}, false)
	if r.ready != 2 || r.total != 3 {
		t.Errorf("expected 2/3 ready, got %+v", r)
	}
	if !reflect.DeepEqual(r.behindAccounts, []string{"w2"}) {
		t.Errorf("expected ['w2'], got %v", r.behindAccounts)
	}
}

func TestBuildReport_SkipsDisabledByDefault(t *testing.T) {
	ws := []witness{
		{Account: "active-ready", Enabled: ptrBool(true), ProtocolVersion: 3},
		{Account: "disabled-old", Enabled: ptrBool(false), ProtocolVersion: 1},
		{Account: "disabled-nil", Enabled: nil, ProtocolVersion: 1},
	}
	r := buildReport(ws, version{0, 3, 0}, false)
	if r.total != 1 || r.ready != 1 {
		t.Errorf("expected only active-ready to count, got %+v", r)
	}
	if r.disabledCount != 2 {
		t.Errorf("expected disabledCount=2, got %d", r.disabledCount)
	}
}

func TestBuildReport_IncludeDisabledCountsThemAgainstReadiness(t *testing.T) {
	// With -includeDisabled the disabled witnesses with old versions
	// count against the fleet — useful if operators want to be strict
	// (e.g. "must upgrade EVERY witness before activation, even
	// disabled ones because they might re-enable in time for the
	// activation height").
	ws := []witness{
		{Account: "active-ready", Enabled: ptrBool(true), ProtocolVersion: 3},
		{Account: "disabled-old", Enabled: ptrBool(false), ProtocolVersion: 1},
	}
	r := buildReport(ws, version{0, 3, 0}, true)
	if r.total != 2 || r.ready != 1 {
		t.Errorf("expected 1/2 ready including disabled, got %+v", r)
	}
	if !reflect.DeepEqual(r.behindAccounts, []string{"disabled-old"}) {
		t.Errorf("expected disabled-old in behind list, got %v", r.behindAccounts)
	}
}

func TestBuildReport_StableBehindOrdering(t *testing.T) {
	// Mongo cursor order is undefined; the CLI sorts behind for stable
	// diffs across runs (and so the operator can grep / pipe the output
	// deterministically). Verify with reverse-input order.
	ws := []witness{
		{Account: "z-behind", Enabled: ptrBool(true), ProtocolVersion: 2},
		{Account: "a-behind", Enabled: ptrBool(true), ProtocolVersion: 2},
		{Account: "m-ready", Enabled: ptrBool(true), ProtocolVersion: 3},
	}
	r := buildReport(ws, version{0, 3, 0}, false)
	if !reflect.DeepEqual(r.behindAccounts, []string{"a-behind", "z-behind"}) {
		t.Errorf("expected sorted ['a-behind','z-behind'], got %v", r.behindAccounts)
	}
}
