# vsc-ledger-signer

A small, **self-contained** TypeScript helper that signs a VSC contract-deployment
transaction with a Ledger hardware wallet. It exists only for operators who
deploy contracts from a Ledger-managed Hive account.

It is **not** part of the Go build. `make` never touches it, and nobody who
doesn't use the Ledger flow needs Node installed. Everything it needs lives in
this directory; `node_modules/` and `dist/` are git-ignored.

## What it does

It plugs into the `contract-deployer`'s `-ledger-sign-cmd` hook. The Go tool
builds the deployment transaction, computes the signing digest with **hivego**
(the same serializer the node uses to broadcast, NAI/HF26 asset format), and
hands this script the digest on stdin. This script asks the Ledger to sign that
exact 32-byte hash and prints the signature on stdout. The Go tool then attaches
it and broadcasts.

Under the hood it uses [`@aioha/ledger-app-hive`](https://www.npmjs.com/package/@aioha/ledger-app-hive)
over a USB-HID transport, calling the Hive app's **hash-signing** path
(`signHash`).

> **Why hash signing.** The device signs the digest Go already computed — nothing
> in this script serializes or re-hashes the transaction. That is deliberate:
> there is exactly one serializer (hivego) in the whole flow, so the bytes the
> node broadcasts and the bytes the device signs can never diverge. The earlier
> approach re-serialized the tx in JS and compared digests, which failed because
> the JS library serializes assets in the legacy format while hivego uses NAI —
> a mismatch that aborted before the device was ever prompted.
>
> The tradeoff is that hash signing is **blind**: the Ledger shows only the hash,
> not a decoded transaction. You must enable it on the device (see Setup).

## I/O contract

- **stdin**: the signing-bundle JSON. Only two fields are read: `signing_digest`
  (64 hex chars / 32 bytes) and `path` (BIP32). Everything else is ignored.
- **stdout**: only the signature hex (so the caller parses it cleanly).
- **stderr**: prompts and diagnostics.
- **exit code**: non-zero on any failure (missing deps, no device, hash signing
  disabled, rejection on device).

This script does **no** serialization and **no** digest verification — it signs
the digest it is given.

## Toolchain

This package uses **pnpm** (the `packageManager` field records the version it was
set up with; `engines` requires `pnpm >=9`). Any pnpm you already have installed
works — you don't need Corepack. If you don't have pnpm: `npm install -g pnpm`.

The source is TypeScript (`sign.ts`) compiled to `dist/sign.js` with `tsc`. The
`prepare` script builds automatically on install, so a single `pnpm install`
gives you a runnable `dist/sign.js`.

## Setup (once)

Prerequisites:

- Node matching [`.nvmrc`](./.nvmrc) (`nvm use` / `fnm use`). Building the
  `node-hid` native module needs a C toolchain + libusb/hidapi headers.
- Linux: install the [LedgerHQ udev rules](https://github.com/LedgerHQ/udev-rules)
  so a non-root user can reach the device.
- On the device: unlock it and open the **Hive** app, then **enable hash
  signing** in the Hive app settings (e.g. *Settings → Sign raw hashes /
  Hash signing → Enabled*). This signer always uses the hash-signing path, so it
  is required — not optional.

Install and build (commit the generated `pnpm-lock.yaml` the first time so
installs are reproducible):

```bash
cd cmd/contract-deployer/ledger-signer
pnpm install          # installs deps and runs `prepare` (tsc) -> dist/sign.js
```

> **pnpm blocks dependency build scripts by default.** `node-hid` needs its
> native build to run, so it's whitelisted via `pnpm.onlyBuiltDependencies` in
> `package.json`. If pnpm still prompts, run `pnpm approve-builds` and allow
> `node-hid`.

### Troubleshooting: Corepack signature errors

Corepack is **optional** here. If you've run `corepack enable` and `pnpm install`
then fails with `Error: Cannot find matching keyid`, that's a known Corepack bug:
the Corepack bundled with your Node ships outdated npm-registry signing keys, so
its signature check rejects current pnpm releases. Fixes:

- Skip Corepack and use your installed pnpm directly: `corepack disable`.
- Or update Corepack (newer versions have current keys): `npm install -g corepack@latest`.
- Or, on Corepack ≥ 0.31, bypass the check: `COREPACK_INTEGRITY_KEYS=0 pnpm install`.

## Test the connection first

Before a real deploy, confirm the cable, app, path, and hash-signing setting with
the bundled `pubkey` helper. It reads the Hive app name/version, whether hash
signing is enabled, and the public key at a path — exercising the same transport
the signer uses, without building or signing a transaction:

```bash
# default path m/48'/13'/0'/0'/0', no device confirmation:
pnpm -s -C cmd/contract-deployer/ledger-signer run pubkey

# a specific path, and confirm the address on the device screen:
pnpm -s -C cmd/contract-deployer/ledger-signer run pubkey -- "m/48'/13'/0'/0'/0'" --confirm
```

Expected stderr looks like `device app: Hive v…` and `hash signing: ENABLED`,
with the `STM…` key on stdout. If you see `Locked device (0x5515)`, unlock the
Ledger and open the Hive app. If `hash signing: DISABLED`, enable it (see Setup)
or the real signer will be rejected.

## Use

`-ledger-sign-cmd` is just a shell command, so the simplest, fully-scoped option
needs no install — point it straight at the built file. From the repo root,
deploy in one run (prepare → sign on device → broadcast):

```bash
contract-deployer -network mainnet -wasmPath ./contract.wasm -name demo \
  -ledger-sign-cmd "node $(pwd)/cmd/contract-deployer/ledger-signer/dist/sign.js" \
  -ledger-path "m/48'/13'/0'/0'/0'"
```

If you'd rather not hardcode the path, run the package's `sign` script scoped to
this directory (no global install, no PATH changes):

```bash
contract-deployer ... \
  -ledger-sign-cmd "pnpm -s -C $(pwd)/cmd/contract-deployer/ledger-signer run sign"
```

Or install it as a short command name — note this is **user-wide** (pnpm's
global bin dir goes on your PATH; there's no pnpm flag to scope a command to a
directory subtree — use `direnv` or a shell alias for that):

```bash
pnpm add -g ./cmd/contract-deployer/ledger-signer   # provides `vsc-ledger-sign`
contract-deployer ... -ledger-sign-cmd "vsc-ledger-sign"
```

If the command isn't installed/runnable, the deployer exits with an error and
points back here.

### No-build alternative (Node ≥ 22.6)

Recent Node can run the TypeScript directly via type stripping, skipping the
build step entirely:

```bash
contract-deployer ... \
  -ledger-sign-cmd "node --experimental-strip-types $(pwd)/cmd/contract-deployer/ledger-signer/sign.ts"
```

(You still need `pnpm install` for the runtime dependencies; you just don't need
`dist/`. The experimental notice Node prints goes to stderr and is harmless.)

## Notes

`-ledger-path` defaults to `m/48'/13'/0'/0'/0'`. Find the right path/account
with another Hive-Ledger tool (e.g. `hive-ledger-cli get-public-key` /
`discover-accounts`) if you're unsure which index holds your active authority.

Testnet/devnet use a non-default Hive chain id. Because this signer signs a
precomputed digest, the chain id is already folded into `signing_digest` by the
Go side (`sha256(chain_id || serialized_tx)`) — there is nothing chain-specific
to configure here.
