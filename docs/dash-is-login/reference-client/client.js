// reference IS-login client — self-contained, no framework.
// Drop into Altera's LoginModal by replacing the DOM-juggling with
// Svelte/Preact bindings; the protocol logic below is the part you keep.
//
// Protocol summary:
//
//   POST {baseUrl}/session/start
//     body { op, args?, sid? }
//     resp { sid, depositAddress, depositInstructionHex, addressSignature,
//            requiredAmountDuffs, expiresAt, statusUrl }
//
//   GET  {baseUrl}/session/{sid}/status
//     resp { sid, state, dashTxId?, forwardedAt?, sessionToken?,
//            forwardError?, expiresAt }
//
//   POST {baseUrl}/session/{sid}/cancel
//
// States:
//   WAITING_FOR_IS → IS_OBSERVED → ATTESTING → ON_CHAIN
//   (any state) → EXPIRED on TTL or cancel
//
// Address fingerprint: first 6 chars of the address signature, shown
// alongside the QR so the user can sanity-check that the address they
// see is the address the IS service signed.

const els = {
  baseUrl: document.getElementById("baseUrl"),
  op: document.getElementById("op"),
  callArgs: document.getElementById("callArgs"),
  contract: document.getElementById("contract"),
  method: document.getElementById("method"),
  argsB64: document.getElementById("argsB64"),
  amount: document.getElementById("amount"),
  startBtn: document.getElementById("start"),
  cancelBtn: document.getElementById("cancel"),
  config: document.getElementById("config"),
  session: document.getElementById("session"),
  result: document.getElementById("result"),
  qr: document.getElementById("qr"),
  depositAddr: document.getElementById("depositAddr"),
  fingerprint: document.getElementById("fingerprint"),
  amountOut: document.getElementById("amount-out"),
  expiresAt: document.getElementById("expiresAt"),
  state: document.getElementById("state"),
  dashTxId: document.getElementById("dashTxId"),
  onChainAt: document.getElementById("onChainAt"),
  addressSignature: document.getElementById("addressSignature"),
  sessionToken: document.getElementById("sessionToken"),
};

let pollTimer = null;
let activeSid = null;

els.op.addEventListener("change", () => {
  els.callArgs.hidden = els.op.value !== "call";
});

els.startBtn.addEventListener("click", async () => {
  els.startBtn.disabled = true;
  try {
    const body = { op: els.op.value };
    if (els.op.value === "call") {
      const amt = parseInt(els.amount.value || "0", 10);
      body.args = {
        contract: els.contract.value.trim(),
        method: els.method.value.trim(),
        args: els.argsB64.value.trim(),
        ...(amt > 0 ? { amount: amt } : {}),
      };
    }
    const resp = await fetch(els.baseUrl.value + "/session/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!resp.ok) {
      const e = await resp.json().catch(() => ({ error: resp.statusText }));
      throw new Error(e.error || resp.statusText);
    }
    const sess = await resp.json();
    showSession(sess);
    startPolling(sess.sid);
  } catch (err) {
    alert("session/start failed: " + err.message);
  } finally {
    els.startBtn.disabled = false;
  }
});

// NOTE: this reference client is a minimal showcase. Production
// clients MUST:
//
//   - send X-Cancel-Token (the addressSignature from /session/start)
//     so the server can authenticate the cancel
//   - distinguish 409 (in-flight refusal — keep polling, do NOT tear
//     down local UI) from 204 (cancelled) and 404/401 (already gone)
//   - bound the post-409 polling window with a client-side deadline
//   - hold the cancel token in JS memory (closure / module state),
//     NOT in the DOM (textContent) — DOM-stored secrets are
//     readable by any same-origin script. Round-9 audit
//     R9-INFO-REFCLIENT-DOM-01: this reference reads the token
//     from els.addressSignature.textContent on purpose so the
//     foot-gun is visible, but production callers MUST keep it
//     out of the DOM.
//
// See altera-app/src/lib/auth/dash/{isClient.ts,session.svelte.ts}
// for a production implementation that handles all three outcomes
// + the R6-SEC-01 trap-deadline + the R9-SEC-01 AbortController
// timeouts. Round-8 audit R8-DRIFT-REF-CLIENT caught the gap below;
// the minimal version stays minimal but is labelled so integrators
// don't ship this verbatim.
els.cancelBtn.addEventListener("click", async () => {
  if (!activeSid) return;
  // Capture the addressSignature returned by /session/start when
  // showing the session and pass it back here — required by the
  // server's R4-003 / TC2-01 auth gate.
  const cancelToken = els.addressSignature.textContent;
  const resp = await fetch(els.baseUrl.value + "/session/" + activeSid + "/cancel", {
    method: "POST",
    headers: cancelToken ? { "X-Cancel-Token": cancelToken } : {},
  });
  if (resp.status === 409) {
    // Server is mid-attestation. A production client would keep
    // polling /status until terminal (bounded by a client deadline).
    // This minimal reference simply surfaces the conflict so the
    // integrator sees it.
    console.warn("cancel refused (409 in-flight) — production clients should keep polling /status");
    return;
  }
  stopPolling();
  els.session.hidden = true;
  els.config.hidden = false;
});

function showSession(sess) {
  activeSid = sess.sid;
  els.config.hidden = true;
  els.session.hidden = false;
  els.depositAddr.textContent = sess.depositAddress;
  els.fingerprint.textContent = sess.addressSignature.slice(0, 6);
  els.amountOut.textContent = sess.requiredAmountDuffs;
  els.expiresAt.textContent = sess.expiresAt;
  els.addressSignature.textContent = sess.addressSignature;

  // DashPay deep-link URI per BIP-21-style "dash:" scheme.
  // Amount is in DASH (decimal), not duffs.
  const dash = (sess.requiredAmountDuffs / 1e8).toFixed(8);
  const uri = `dash:${sess.depositAddress}?amount=${dash}&IS=1`;
  renderQR(uri);
}

function renderQR(uri) {
  els.qr.innerHTML = "";
  // QRCode global comes from vendor/qrcode.min.js.
  // eslint-disable-next-line no-undef
  new QRCode(els.qr, { content: uri, padding: 2, width: 256, height: 256 });
}

function startPolling(sid) {
  stopPolling();
  pollTimer = setInterval(() => pollStatus(sid), 2000);
  pollStatus(sid); // immediate first poll
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
}

async function pollStatus(sid) {
  try {
    const resp = await fetch(
      els.baseUrl.value + "/session/" + sid + "/status",
    );
    if (resp.status === 404) {
      stopPolling();
      return;
    }
    const s = await resp.json();
    els.state.textContent = s.state;
    els.dashTxId.textContent = s.dashTxId || "—";
    els.onChainAt.textContent = s.forwardedAt || "—";
    if (s.state === "ON_CHAIN" && s.sessionToken) {
      stopPolling();
      els.result.hidden = false;
      els.sessionToken.textContent = s.sessionToken;
    } else if (
      // Round-9 audit R9-OP-01: real terminal states match
      // cmd/is-service/session.go IsTerminal() — the pre-R9
      // reference checked a non-existent "FAILED" state and
      // polled forever on FORWARD_FAILED / ATTESTATION_TIMEOUT /
      // SLOW_PATH_PENDING.
      s.state === "EXPIRED" ||
      s.state === "FORWARD_FAILED" ||
      s.state === "ATTESTATION_TIMEOUT" ||
      s.state === "SLOW_PATH_PENDING"
    ) {
      stopPolling();
    }
  } catch (err) {
    // Network blip — keep polling. The session has its own TTL on the
    // server and will time out without our help.
    console.warn("status poll failed:", err);
  }
}
