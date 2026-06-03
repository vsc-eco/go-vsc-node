package devnet

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// MagiPeerMultiaddr returns the in-docker libp2p multiaddr for
// magi-N. Computed by reading the witness's libp2p private key from
// `<devnetDir>/data-N/config/identityConfig.json`, deriving the peer
// ID, and concatenating with the dns4 hostname + the magi default
// libp2p port (10720, set by modules/p2p/config.go NewConfig).
//
// Format: `/dns4/magi-N/tcp/10720/p2p/<peerID>`
//
// Used by the IS-login devnet E2E to bootstrap the IS service into
// magi-1's libp2p gossip so attestation requests reach the witness
// via the real islock-attestation/v1 topic instead of the test-only
// /test/attestation HTTP injection bypass.
func (d *Devnet) MagiPeerMultiaddr(node int) (string, error) {
	donorPath := filepath.Join(d.devnetDir,
		fmt.Sprintf("data-%d", node), "config", "identityConfig.json")
	donorBytes, err := os.ReadFile(donorPath)
	if err != nil {
		return "", fmt.Errorf("reading magi-%d identityConfig at %s: %w", node, donorPath, err)
	}
	var donor struct {
		Libp2pPrivKey string `json:"Libp2pPrivKey"`
	}
	if err := json.Unmarshal(donorBytes, &donor); err != nil {
		return "", fmt.Errorf("parsing magi-%d identityConfig: %w", node, err)
	}
	if donor.Libp2pPrivKey == "" {
		return "", fmt.Errorf("magi-%d Libp2pPrivKey is empty", node)
	}
	privBytes, err := hex.DecodeString(donor.Libp2pPrivKey)
	if err != nil {
		return "", fmt.Errorf("decoding magi-%d Libp2pPrivKey: %w", node, err)
	}
	priv, err := crypto.UnmarshalEd25519PrivateKey(privBytes)
	if err != nil {
		return "", fmt.Errorf("unmarshalling magi-%d ed25519 priv: %w", node, err)
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return "", fmt.Errorf("deriving magi-%d peer ID: %w", node, err)
	}
	// Default p2p port — magi nodes listen on `/ip4/0.0.0.0/tcp/<port>`
	// but the harness assigns p2pPort = cfg.P2PBasePort + i - 1 from
	// inside the container the listen port is always the config default
	// (cfg.P2PBasePort is the host-side mapping). The container itself
	// listens on the in-config port (10720 + offset).
	port := 10720 + node - 1
	return fmt.Sprintf("/dns4/magi-%d/tcp/%d/p2p/%s", node, port, id.String()), nil
}
