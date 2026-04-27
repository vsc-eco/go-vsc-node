// Package node_config exposes the operator-selectable role for this node.
//
// witness  — minimal persistence; aggressive pruning for everything safe to drop.
// full     — default; keeps GraphQL-served history, runs safe local pruning.
// archive  — keeps everything; runs no pruning at all.
//
// The role is read once at startup from the VSC_NODE_ROLE env var.
package node_config

import (
	"os"
	"strings"
)

type NodeRole string

const (
	RoleWitness NodeRole = "witness"
	RoleFull    NodeRole = "full"
	RoleArchive NodeRole = "archive"
)

// FromEnv returns the role configured via VSC_NODE_ROLE. Unset or unrecognized
// values default to RoleFull.
func FromEnv() NodeRole {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VSC_NODE_ROLE"))) {
	case string(RoleWitness):
		return RoleWitness
	case string(RoleArchive):
		return RoleArchive
	default:
		return RoleFull
	}
}

// PrunesRcs returns true if this role should periodically prune old rcs records.
func (r NodeRole) PrunesRcs() bool {
	return r != RoleArchive
}

// PrunesExpiredUnconfirmedTxs returns true if this role should periodically prune
// expired UNCONFIRMED transactions. All roles do — those records are useless.
func (r NodeRole) PrunesExpiredUnconfirmedTxs() bool {
	return true
}

// PrunesConfirmedTxs returns true if this role should periodically prune
// terminal-state transactions (CONFIRMED, FAILED, INCLUDED). Witness-only:
// full and archive nodes serve historical tx queries via GraphQL; witnesses
// don't, so they can drop everything past a recent window.
func (r NodeRole) PrunesConfirmedTxs() bool {
	return r == RoleWitness
}
