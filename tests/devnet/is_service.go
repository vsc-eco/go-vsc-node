package devnet

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"vsc-node/lib/dids"
	islock "vsc-node/modules/islock-attestation"

	blsAPI "github.com/protolambda/bls12-381-util"
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
	// TestBypassDashdISLock toggles the IS service's
	// -testBypassDashdISLock flag. When true, the dashd watcher treats
	// every observed tx as IS-locked. **TEST-ONLY** — the IS service
	// args.go refuses to honour it unless -network=devnet. Used by
	// tests/devnet to drive the IS_OBSERVED transition without an
	// LLMQ quorum.
	TestBypassDashdISLock bool
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
	// L2 trio is all-or-nothing: IS service refuses to start with a
	// partial config (cmd/is-service/main.go L143-165). Allow ALL
	// unset to use the log-only submitter, which still advances the
	// orchestrator through L2_SUBMITTED → ON_CHAIN — convenient for
	// tests that exercise the state machine without standing up an
	// L2 GraphQL endpoint or funding an L2 RC account.
	hasL2URL := opts.L2GqlURL != ""
	hasL2Key := opts.L2PrivKeyHex != ""
	hasL2Contract := opts.L2DashContract != ""
	if hasL2URL || hasL2Key || hasL2Contract {
		if !(hasL2URL && hasL2Key && hasL2Contract) {
			return fmt.Errorf("L2GqlURL, L2PrivKeyHex, L2DashContract must be all set or all unset (trio)")
		}
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
	if opts.TestBypassDashdISLock {
		extraEnv = append(extraEnv, "IS_TEST_BYPASS_ISLOCK=true")
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

// readMagiBlsKey reads magi-N's BlsPrivKeySeed from its identityConfig.json,
// deserialises the BLS private key, and derives the matching pubkey + DID.
// Used by attestation-injection + validator-set-payload helpers below.
func (d *Devnet) readMagiBlsKey(node int) (
	priv *dids.BlsPrivKey,
	pub *blsAPI.Pubkey,
	did dids.BlsDID,
	err error,
) {
	donorPath := filepath.Join(d.devnetDir,
		fmt.Sprintf("data-%d", node), "config", "identityConfig.json")
	donorBytes, err := os.ReadFile(donorPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("reading magi-%d identityConfig at %s: %w", node, donorPath, err)
	}
	var donor struct {
		BlsPrivKeySeed string `json:"BlsPrivKeySeed"`
	}
	if err := json.Unmarshal(donorBytes, &donor); err != nil {
		return nil, nil, "", fmt.Errorf("parsing magi-%d identityConfig: %w", node, err)
	}
	if len(donor.BlsPrivKeySeed) != 64 {
		return nil, nil, "", fmt.Errorf("magi-%d BlsPrivKeySeed must be 32 hex bytes, got len=%d",
			node, len(donor.BlsPrivKeySeed))
	}
	seedBytes, err := hex.DecodeString(donor.BlsPrivKeySeed)
	if err != nil {
		return nil, nil, "", fmt.Errorf("decoding magi-%d BlsPrivKeySeed: %w", node, err)
	}
	var seed [32]byte
	copy(seed[:], seedBytes)
	priv = &dids.BlsPrivKey{}
	if err := priv.Deserialize(&seed); err != nil {
		return nil, nil, "", fmt.Errorf("deserializing magi-%d BLS priv: %w", node, err)
	}
	pub, err = blsAPI.SkToPk(priv)
	if err != nil {
		return nil, nil, "", fmt.Errorf("deriving magi-%d BLS pubkey: %w", node, err)
	}
	did, err = dids.NewBlsDID(pub)
	if err != nil {
		return nil, nil, "", fmt.Errorf("deriving magi-%d BLS DID: %w", node, err)
	}
	return priv, pub, did, nil
}

// BuildValidatorSetPayload constructs the 4-field per-entry payload
// the dash-mapping-contract's setValidatorSet expects:
//
//	<epoch>;<did>=<pk_hex>=<pop_hex>=<account>|...
//
// Mirrors the format produced by dash-mapping-contract/cmd/
// gen-validator-set-payload — generates an account-bound PoP via
// dids.GenerateBlsPoP, decodes the base64-rawurl PoP and hex-
// encodes it (the contract's verifier accepts hex). The `nodes`
// list selects which magi witnesses to include; the account name
// for each is `<witnessPrefix><N>` (e.g. "magi.test1").
func (d *Devnet) BuildValidatorSetPayload(epoch uint64, nodes []int) (string, error) {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		priv, pub, did, err := d.readMagiBlsKey(n)
		if err != nil {
			return "", err
		}
		account := fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, n)
		popB64, err := dids.GenerateBlsPoP(priv, account)
		if err != nil {
			return "", fmt.Errorf("magi-%d PoP: %w", n, err)
		}
		popRaw, err := base64.RawURLEncoding.DecodeString(popB64)
		if err != nil {
			return "", fmt.Errorf("magi-%d PoP decode: %w", n, err)
		}
		pkBytes := pub.Serialize()
		entry := fmt.Sprintf("%s=%s=%s=%s",
			did.String(),
			hex.EncodeToString(pkBytes[:]),
			hex.EncodeToString(popRaw),
			account)
		parts = append(parts, entry)
	}
	return fmt.Sprintf("%d;%s", epoch, strings.Join(parts, "|")), nil
}

// IsForceAttestation reads magi-N's BLS private-key seed from its
// data-dir, constructs the canonical IsLockAttestationRequest using
// the same formula the orchestrator computes, signs the canonical
// message, and POSTs the resulting IsLockAttestationResponse to the
// IS service's test-only /test/attestation/{sid} endpoint.
//
// Mirrors the orchestrator's request shape from cmd/is-service/
// orchestrator.go:248 (sha256d(rawTxBytes) reversed → RawTxHashHex,
// sha256(instruction) → InstructionHashHex, epoch=0 in log-only
// mode, chainId="vsc-devnet"). Pass the exact same `rawTxHex` and
// `instruction` values that were registered on /session/start +
// /test/observed so the canonical messages match byte-for-byte.
//
// On success the orchestrator's collector receives the attestation
// and (assuming the quorumThreshold is met — default 1) aggregates
// + submits via the log-only submitter, advancing the session to
// L2_SUBMITTED → ON_CHAIN.
func (d *Devnet) IsForceAttestation(
	ctx context.Context,
	sid, txid, rawTxHex, instruction string,
	donorNode int,
) error {
	// 1. Read donor magi's identityConfig.json. The file lives at
	//    <devnetDir>/data-N/config/identityConfig.json (the config
	//    package appends /config to the dataDir).
	donorPath := filepath.Join(d.devnetDir,
		fmt.Sprintf("data-%d", donorNode), "config", "identityConfig.json")
	donorBytes, err := os.ReadFile(donorPath)
	if err != nil {
		return fmt.Errorf("reading donor identityConfig at %s: %w", donorPath, err)
	}
	var donor struct {
		BlsPrivKeySeed string `json:"BlsPrivKeySeed"`
	}
	if err := json.Unmarshal(donorBytes, &donor); err != nil {
		return fmt.Errorf("parsing donor identityConfig: %w", err)
	}
	if len(donor.BlsPrivKeySeed) != 64 {
		return fmt.Errorf("donor BlsPrivKeySeed must be 32 hex bytes, got len=%d", len(donor.BlsPrivKeySeed))
	}
	seedBytes, err := hex.DecodeString(donor.BlsPrivKeySeed)
	if err != nil {
		return fmt.Errorf("decoding BlsPrivKeySeed: %w", err)
	}
	var seed [32]byte
	copy(seed[:], seedBytes)
	priv := &dids.BlsPrivKey{}
	if err := priv.Deserialize(&seed); err != nil {
		return fmt.Errorf("deserializing BLS priv: %w", err)
	}
	pub, err := blsAPI.SkToPk(priv)
	if err != nil {
		return fmt.Errorf("derive pubkey: %w", err)
	}
	did, err := dids.NewBlsDID(pub)
	if err != nil {
		return fmt.Errorf("derive DID: %w", err)
	}

	// 2. Build the canonical IsLockAttestationRequest. epoch=0
	//    matches the log-only submitter's EpochFor stub. chainId
	//    matches the IS service's -chainID derived from
	//    -network=devnet (vsc-devnet).
	rawTxBytes, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return fmt.Errorf("decoding rawTxHex: %w", err)
	}
	first := sha256.Sum256(rawTxBytes)
	internal := sha256.Sum256(first[:])
	displayHash := islock.ReverseBytesCopy(internal[:])
	instrHash := sha256.Sum256([]byte(instruction))

	req := islock.IsLockAttestationRequest{
		TxId:               txid,
		RawTxHashHex:       hex.EncodeToString(displayHash),
		InstructionHashHex: hex.EncodeToString(instrHash[:]),
		Epoch:              0,
		ChainId:            "vsc-devnet",
	}

	// 3. Sign the canonical message + build the response.
	resp, err := islock.BuildResponse(req, did.String(), priv)
	if err != nil {
		return fmt.Errorf("BuildResponse: %w", err)
	}

	// 4. POST to /test/attestation/{sid}.
	body, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshalling attestation response: %w", err)
	}
	url := d.IsServiceHttpURL() + "/test/attestation/" + sid
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("posting /test/attestation: %w", err)
	}
	defer httpResp.Body.Close()
	respBody, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return fmt.Errorf("/test/attestation returned %d: %s", httpResp.StatusCode, string(respBody))
	}
	return nil
}

// IsForceObserved POSTs to the IS service's test-only
// /test/observed/{sid} endpoint to synthesise an onISLockObserved
// callback. txid is optional; if empty the IS service synthesises
// one. rawTxHex feeds the orchestrator's canonical-message
// computation — caller must supply the same bytes when signing the
// attestation later (otherwise the per-sig BLS verify rejects).
func (d *Devnet) IsForceObserved(ctx context.Context, sid, txid string) error {
	return d.IsForceObservedWithRawTx(ctx, sid, txid, "")
}

// IsForceObservedWithRawTx is the rawTxHex-aware variant.
func (d *Devnet) IsForceObservedWithRawTx(ctx context.Context, sid, txid, rawTxHex string) error {
	url := d.IsServiceHttpURL() + "/test/observed/" + sid
	body := map[string]string{}
	if txid != "" {
		body["txid"] = txid
	}
	if rawTxHex != "" {
		body["rawTxHex"] = rawTxHex
	}
	bodyJSON, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyJSON))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting /test/observed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("/test/observed returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// IsSessionStatusResp mirrors cmd/is-service/handlers.go's
// SessionStatusResponse. State is the orchestrator phase string
// ("WAITING_FOR_IS", "IS_OBSERVED", "ATTESTING", "ON_CHAIN", etc.) —
// see modules/is-service/session.go for the full set.
type IsSessionStatusResp struct {
	Sid           string `json:"sid"`
	State         string `json:"state"`
	DashTxId      string `json:"dashTxId,omitempty"`
	SenderAddress string `json:"senderAddress,omitempty"`
	ForwardedAt   string `json:"forwardedAt,omitempty"`
	SessionToken  string `json:"sessionToken,omitempty"`
	L2TxId        string `json:"l2TxId,omitempty"`
	ForwardError  string `json:"forwardError,omitempty"`
	ExpiresAt     string `json:"expiresAt"`
}

// IsGetStatus GETs /session/{sid}/status and returns the parsed
// response. Returns an error with the raw response body on non-2xx.
func (d *Devnet) IsGetStatus(ctx context.Context, sid string) (*IsSessionStatusResp, error) {
	url := d.IsServiceHttpURL() + "/session/" + sid + "/status"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting /session/status: %w", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("/session/status returned %d: %s", resp.StatusCode, string(rawBody))
	}
	var out IsSessionStatusResp
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return nil, fmt.Errorf("decoding response: %w (raw=%s)", err, string(rawBody))
	}
	return &out, nil
}

// WaitForIsSessionState polls /session/status until `state` matches
// or the timeout expires. Returns the final response (regardless of
// whether the target state was reached) so the caller can log
// terminal-error fields. Returns an error if the timeout expires
// without reaching `state` or any other state in `acceptableStates`.
func (d *Devnet) WaitForIsSessionState(ctx context.Context, sid string, acceptableStates []string, timeout time.Duration) (*IsSessionStatusResp, error) {
	deadline := time.Now().Add(timeout)
	var last *IsSessionStatusResp
	accept := make(map[string]struct{}, len(acceptableStates))
	for _, s := range acceptableStates {
		accept[s] = struct{}{}
	}
	for {
		resp, err := d.IsGetStatus(ctx, sid)
		if err == nil {
			last = resp
			if _, ok := accept[resp.State]; ok {
				return resp, nil
			}
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf(
				"session %s did not reach one of %v within %s (last state: %v)",
				sid, acceptableStates, timeout, lastState(last))
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func lastState(r *IsSessionStatusResp) string {
	if r == nil {
		return "<no response>"
	}
	return r.State
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
