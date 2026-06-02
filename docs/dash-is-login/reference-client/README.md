# Sign in with DashPay — reference client

This is a self-contained reference integration for the
`is-service` HTTP API. It's a single static HTML/JS/CSS bundle with
no build step — just open `index.html` against a running IS service.

Use it to:

* Smoke-test an IS-service deployment end-to-end with a real DashPay wallet
* Crib protocol details when integrating into a real frontend
  (e.g. Altera's `LoginModal`)

## Running

1. Start the IS service:

   ```bash
   go run ./cmd/is-service \
     -primaryPubkey <hex> \
     -backupPubkey <hex> \
     -addressSignerSecret devsecret-not-for-prod \
     -dashdRPC http://localhost:9998 \
     -dashdRPCUser <user> -dashdRPCPassword <pass> \
     -network testnet -port 3030
   ```

2. Open `index.html` directly in a browser. Configure the base URL
   (default `http://localhost:3030`) and click "Start session".

3. Scan the QR with DashPay. Once the InstantSend lock fires the
   client polls `/session/{sid}/status` until it sees `ON_CHAIN`,
   then displays the session token.

## What this is NOT

* Not production-ready. The QR rendering, polling cadence, and CSS
  are placeholders.
* Doesn't verify the address signature against a pinned key. The
  IS-service uses an HMAC stub by default; a real frontend would
  pin the IS service's public key (per spec §5.7) and verify
  `addressSignature` against `depositAddress || instruction` before
  showing the QR.
* No deep-link fallback for desktop browsers. The `dash:` URI works
  if a DashPay wallet is installed on the same device or registered
  as a URI handler; otherwise the QR is the only path.

## Files

* `index.html` — UI shell, form fields, QR slot
* `client.js`  — protocol logic; copy-paste this into your frontend
* `styles.css` — minimal dark-theme styling
* `vendor/qrcode.min.js` — bundled QR renderer (placeholder; ship
  your own)

## Integration checklist for Altera

When porting `client.js` into the Altera `LoginModal`:

- [ ] Wrap the polling in a Svelte/Preact store so the rest of the UI
      can subscribe to session state
- [ ] Wire the address-signature verifier against the pinned IS-service
      public key (currently the reference client just shows the first 6
      chars as a fingerprint — that closes the visual-substitution vector,
      not the network-level one)
- [ ] On `ON_CHAIN`, exchange the session token for an Altera session
      (whatever shape Altera's own auth expects)
- [ ] Pipe the deep-link `dash:` URI to the relevant DashPay deep-link
      handler on iOS/Android, with desktop fallback to the QR
- [ ] Move `baseUrl` out of an input box and into Altera's env config
- [ ] Add error states for `FORWARD_FAILED` / `ATTESTATION_TIMEOUT` /
      `SLOW_PATH_PENDING` / `EXPIRED` that explain what to do next
      (matches the Go `IsTerminal()` set in `cmd/is-service/session.go`;
      round-10 audit R10-DRIFT-REF-CLIENT-README-FAILED fixed the
      stale `FAILED` reference here)
