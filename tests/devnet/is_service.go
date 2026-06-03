package devnet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// IsServiceOpts wires the operator-tunable surface of the IS service
// for a devnet test. All hex fields are unpadded lowercase. Trio
// constraint enforced by the binary: l2GqlURL + l2PrivKey +
// l2DashMappingContract must all be set or all unset; any partial
// combo aborts on startup with a clear error (see cmd/is-service/
// main.go L143-165 + audit `no-l2-config-trio-validation`).
type IsServiceOpts struct {
	// PrimaryPubkey + BackupPubkey are 33-byte hex compressed
	// secp256k1 keys for the bridge TSS. For devnet tests these can
	// be any well-formed compressed pubkey — the IS service stamps
	// them into the deposit address but the test driver doesn't
	// exercise the multisig spending path. GenerateIsTestKeys()
	// fills these in with random valid pubkeys.
	PrimaryPubkey string
	BackupPubkey  string
	// L2GqlURL points at one of the magi nodes' GraphQL endpoints
	// (in-docker — http://magi-1:8080/api/v1/graphql).
	L2GqlURL string
	// L2PrivKeyHex is the secp256k1 key the IS service signs L2
	// submissions with. The derived did:pkh:eip155:0x... account
	// pays RC for each mapInstantSendV2 broadcast. For a smoke test
	// that only exercises /session/start (not actual L2 submission)
	// any well-formed priv key suffices; the derived account need
	// not be HBD-funded.
	L2PrivKeyHex string
	// L2DashContract is the deployed dash-mapping-contract id
	// (vsc1...). Required by the trio.
	L2DashContract string
	// AddressSignerSecret is the HMAC secret used to sign
	// (deposit_address, instruction) tuples returned by
	// /session/start. **DEV/TEST ONLY** — production must replace
	// with an HSM/KMS asymmetric signer per spec §5.7. The IS
	// service refuses to start when this is empty.
	AddressSignerSecret string
	// BootstrapPeers is the comma-separated libp2p multiaddr list
	// for the IS service to dial the validator islock-attestation
	// gossip topic. Leave empty for the noop broadcaster — sessions
	// will stall at ATTESTING but /session/start still works.
	BootstrapPeers string
}

// GenerateIsTestKeys returns three randomly-generated, well-formed
// hex strings: primary pubkey (33-byte compressed), backup pubkey
// (33-byte compressed), and an L2 priv key (32-byte). Useful for
// IS-service devnet smoke tests where the keys' actual ownership
// doesn't matter.
func GenerateIsTestKeys() (primaryPubHex, backupPubHex, l2PrivHex string, err error) {
	primaryPubHex, err = randomCompressedPubkeyHex()
	if err != nil {
		return "", "", "", fmt.Errorf("primary key: %w", err)
	}
	backupPubHex, err = randomCompressedPubkeyHex()
	if err != nil {
		return "", "", "", fmt.Errorf("backup key: %w", err)
	}
	var privBytes [32]byte
	if _, err := rand.Read(privBytes[:]); err != nil {
		return "", "", "", fmt.Errorf("l2 priv: %w", err)
	}
	l2PrivHex = hex.EncodeToString(privBytes[:])
	return primaryPubHex, backupPubHex, l2PrivHex, nil
}

func randomCompressedPubkeyHex() (string, error) {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return "", err
	}
	priv := secp256k1.PrivKeyFromBytes(seed[:])
	pub := priv.PubKey()
	return hex.EncodeToString(pub.SerializeCompressed()), nil
}

// StartIsService brings up the is-service container against the
// running devnet. The harness pushes the IsServiceOpts fields into
// the docker-compose env so the service definition (see
// docker-compose.yml) picks them up. Returns nil once the HTTP port
// is accepting connections (poll /healthz with a 30s budget).
//
// Idempotent: subsequent calls with the same opts no-op.
func (d *Devnet) StartIsService(ctx context.Context, opts IsServiceOpts) error {
	if !d.started {
		return fmt.Errorf("devnet not started")
	}
	if opts.PrimaryPubkey == "" || opts.BackupPubkey == "" {
		return fmt.Errorf("PrimaryPubkey and BackupPubkey are required")
	}
	if opts.L2GqlURL == "" || opts.L2PrivKeyHex == "" || opts.L2DashContract == "" {
		return fmt.Errorf("L2GqlURL, L2PrivKeyHex, L2DashContract are required (trio)")
	}
	if opts.AddressSignerSecret == "" {
		return fmt.Errorf("AddressSignerSecret is required (IS service refuses to start without it)")
	}

	log.Printf("[devnet] starting is-service (contract=%s)...", opts.L2DashContract)

	// IS_* env vars are referenced via ${VAR:-} substitutions in the
	// docker-compose.yml is-service block. They have to be set in the
	// docker subprocess's env (not via `-e`, which is a `docker run`
	// flag — `docker compose up` doesn't accept it).
	extraEnv := []string{
		"IS_PRIMARY_PUBKEY=" + opts.PrimaryPubkey,
		"IS_BACKUP_PUBKEY=" + opts.BackupPubkey,
		"IS_L2_GQL_URL=" + opts.L2GqlURL,
		"IS_L2_PRIVKEY=" + opts.L2PrivKeyHex,
		"IS_L2_DASH_CONTRACT=" + opts.L2DashContract,
		"IS_ADDRESS_SIGNER_SECRET=" + opts.AddressSignerSecret,
	}
	if opts.BootstrapPeers != "" {
		extraEnv = append(extraEnv, "IS_BOOTSTRAP_PEERS="+opts.BootstrapPeers)
	}

	args := []string{
		"--profile", "is-service",
		"--profile", "dashd", // is-service depends_on dashd
		"up", "-d", "is-service",
	}
	if err := d.composeWithEnv(ctx, extraEnv, args...); err != nil {
		return fmt.Errorf("starting is-service: %w", err)
	}

	// Poll the container's mapped HTTP port until it accepts a
	// /session/start probe (with a deliberately malformed body so
	// we get a 4xx rather than starting a real session, just to
	// confirm the server is up).
	return d.waitForIsServiceReady(ctx, 30*time.Second)
}

// StopIsService stops the is-service container (one-shot teardown).
// Safe to call even if the service was never started.
func (d *Devnet) StopIsService(ctx context.Context) error {
	return d.compose(ctx, "--profile", "is-service", "stop", "is-service")
}

// IsServiceLogs returns the is-service container's docker logs.
// Uses `docker logs` directly against the container name (rather
// than `docker compose logs`) because the compose-level command
// requires the profile + every ${VAR:-} reference resolved in the
// subprocess env, and StartIsService's env passing doesn't carry
// over to a separate logs subprocess.
func (d *Devnet) IsServiceLogs(ctx context.Context) (string, error) {
	containerName := fmt.Sprintf("%s-is-service-1", d.projectName)
	cmd := exec.CommandContext(ctx, "docker", "logs", containerName)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// IsServiceHttpURL returns the host-mapped URL of the IS service.
// IS_SERVICE_PORT can be overridden via env; default 13030.
func (d *Devnet) IsServiceHttpURL() string {
	return "http://127.0.0.1:13030"
}

func (d *Devnet) waitForIsServiceReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := d.IsServiceHttpURL() + "/session/start"
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("is-service did not become ready within %s", timeout)
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			// Any HTTP response (even 400 for empty body) means the
			// server is listening + handling requests.
			log.Printf("[devnet] is-service ready (probe status=%d)", resp.StatusCode)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// IsSessionStartReq mirrors the cmd/is-service /session/start
// request body. Op is one of "auth" | "call"; Args is the URL-
// instruction params for op=call (ignored for auth).
type IsSessionStartReq struct {
	Op   string                 `json:"op"`
	Args map[string]interface{} `json:"args,omitempty"`
}

// IsSessionStartResp mirrors the cmd/is-service /session/start
// response body. JSON keys match handlers.go:SessionStartResponse
// (camelCase, not snake_case). Only the fields the smoke test
// consumes are projected here.
type IsSessionStartResp struct {
	Sid                   string `json:"sid"`
	DepositAddress        string `json:"depositAddress"`
	DepositInstructionHex string `json:"depositInstructionHex"`
	AddressSignature      string `json:"addressSignature"`
	RequiredAmountDuffs   int64  `json:"requiredAmountDuffs"`
	ExpiresAt             string `json:"expiresAt"`
	StatusURL             string `json:"statusUrl"`
}

// IsStartSession POSTs /session/start and returns the parsed
// response. Returns an error with the raw response body on non-2xx.
func (d *Devnet) IsStartSession(ctx context.Context, body IsSessionStartReq) (*IsSessionStartResp, error) {
	url := d.IsServiceHttpURL() + "/session/start"
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshalling body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting /session/start: %w", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("/session/start returned %d: %s", resp.StatusCode, string(rawBody))
	}
	var out IsSessionStartResp
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return nil, fmt.Errorf("decoding response: %w (raw=%s)", err, string(rawBody))
	}
	return &out, nil
}
