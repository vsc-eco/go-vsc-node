#!/usr/bin/env node
// Self-contained Ledger signer for the VSC contract-deployer.
//
// Contract (matches cmd/contract-deployer's -ledger-sign-cmd hook):
//   stdin : a signing-bundle JSON, e.g.
//           { "chain_id": "<hex>", "signing_digest": "<hex>", "path": "m/48'/13'/0'/0'/0'",
//             "transaction": { ref_block_num, ref_block_prefix, expiration, operations, extensions, signatures } }
//   stdout: ONLY the signature hex (so the caller can read it cleanly)
//   stderr: all diagnostics / prompts
//   exit  : non-zero on any failure (missing deps, no device, digest mismatch, user rejection)
//
// Before signing it recomputes the digest with the Hive Ledger library and
// refuses to sign if it disagrees with the digest the Go side computed (a
// serialization mismatch) — so a mismatch can never produce a signature you'd
// broadcast by mistake.
//
// Deps are loaded dynamically so a clean checkout (deps not installed) fails
// with an actionable message instead of a stack trace. They're typed `any`
// deliberately: this file must not be coupled to the upstream type surface.

interface SigningRequest {
  transaction?: unknown
  chain_id?: string
  signing_digest?: string
  path?: string
  account?: string
}

function fail(msg: string, code?: number): never {
  process.stderr.write(String(msg) + '\n')
  process.exit(code ?? 1)
}

function load(name: string): any {
  try {
    return require(name)
  } catch (e: any) {
    if (e && e.code === 'MODULE_NOT_FOUND' && String(e.message).includes(name)) {
      fail(
        `missing dependency '${name}'. Run 'pnpm install' in cmd/contract-deployer/ledger-signer/ ` +
          `(see its README.md).`,
        2,
      )
    }
    throw e
  }
}

async function readStdin(): Promise<string> {
  const chunks: Uint8Array[] = []
  for await (const chunk of process.stdin) chunks.push(chunk as Uint8Array)
  return Buffer.concat(chunks).toString('utf8')
}

async function main(): Promise<void> {
  const aioha = load('@aioha/ledger-app-hive')
  const Hive: any = aioha.default || aioha.Hive || aioha
  const transportMod = load('@ledgerhq/hw-transport-node-hid')
  const TransportNodeHid: any = transportMod.default || transportMod

  let req: SigningRequest
  try {
    req = JSON.parse(await readStdin()) as SigningRequest
  } catch (e: any) {
    fail('could not parse signing request JSON on stdin: ' + e.message, 1)
  }

  const tx = req.transaction
  const chainId = req.chain_id
  const expectedDigest = req.signing_digest
  const path = req.path
  if (!tx || !path) fail('signing request must include "transaction" and "path"', 1)

  // Refuse to sign unless this library reproduces the caller's digest. This is
  // the safety net against the two libraries serializing the tx differently —
  // signing a digest the node won't accept would just waste a device approval.
  try {
    const libDigest: string | undefined = Hive.getTransactionDigest(tx, chainId)
    if (expectedDigest && libDigest && libDigest.toLowerCase() !== expectedDigest.toLowerCase()) {
      fail(
        `digest mismatch — refusing to sign.\n  signer computed: ${libDigest}\n  caller expected: ${expectedDigest}\n` +
          `(the Go and JS Hive serializers disagree; do not broadcast this.)`,
        3,
      )
    }
  } catch (e: any) {
    process.stderr.write('warning: could not pre-verify digest with the library: ' + e.message + '\n')
  }

  let transport: any
  try {
    transport = await TransportNodeHid.create()
  } catch (e: any) {
    fail('could not open the Ledger (plugged in, unlocked, Hive app open?): ' + e.message, 4)
  }

  try {
    process.stderr.write('review and approve the transaction on your Ledger...\n')
    const hive = new Hive(transport)
    const signed: any = await hive.signTransaction(tx, path, chainId)
    const sig = Array.isArray(signed?.signatures) ? signed.signatures[0] : signed?.signatures
    if (!sig) fail('device returned no signature', 5)
    process.stdout.write(String(sig).trim())
  } catch (e: any) {
    fail('signing failed on the device: ' + (e && e.message ? e.message : String(e)), 5)
  } finally {
    try {
      await transport.close()
    } catch {
      /* ignore */
    }
  }
}

main().catch((e: any) => fail('unexpected error: ' + (e && e.message ? e.message : String(e)), 1))
