#!/usr/bin/env node
// Self-contained Ledger signer for the VSC contract-deployer.
//
// Contract (matches cmd/contract-deployer's -ledger-sign-cmd hook):
//   stdin : a signing-bundle JSON. Only two fields are used:
//             { "signing_digest": "<64 hex chars>", "path": "m/48'/13'/0'/0'/0'" }
//   stdout: ONLY the signature hex (so the caller can read it cleanly)
//   stderr: all diagnostics / prompts
//   exit  : non-zero on any failure (missing deps, no device, user rejection)
//
// This signer does NOT serialize, re-serialize, or re-hash anything. It takes the
// exact 32-byte digest the Go side already computed (hivego, NAI/HF26 asset
// serialization) and asks the Ledger to sign that hash directly via the Hive
// app's INS_SIGN_HASH ("hash signing") path. The bytes the node broadcasts and the
// bytes the device signs therefore come from a single source — Go — so there is
// no second serializer to disagree, and no digest verification to perform here.
//
// Tradeoff: hash signing is "blind" — the device displays only the hash, not a
// decoded transaction. The Hive app's hash-signing policy must be enabled on the
// device ("Sign raw hashes" in the app settings), or the app rejects the request.
//
// Deps are loaded dynamically so a clean checkout (deps not installed) fails with
// an actionable message instead of a stack trace. They're typed `any` deliberately:
// this file must not be coupled to the upstream type surface.

interface SigningRequest {
  signing_digest?: string
  path?: string
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

  const digest = req.signing_digest
  const path = req.path
  if (!path) fail('signing request must include "path"', 1)
  // Sanity-check only the *shape* of the digest (32 bytes of hex) — this does not
  // recompute or verify it, it just avoids sending obvious garbage to the device.
  if (!digest || !/^[0-9a-fA-F]{64}$/.test(digest)) {
    fail('signing request must include "signing_digest" as 64 hex chars (32 bytes)', 1)
  }

  let transport: any
  try {
    transport = await TransportNodeHid.create()
  } catch (e: any) {
    fail('could not open the Ledger (plugged in, unlocked, Hive app open?): ' + e.message, 4)
  }

  try {
    const hive = new Hive(transport)

    // Hash signing must be enabled in the Hive app, otherwise the device rejects
    // the request. Surface a clear, actionable message instead of a raw status.
    try {
      const settings: any = await hive.getSettings()
      if (settings && settings.hashSignPolicy === false) {
        fail(
          'the Ledger Hive app has hash signing disabled. Enable it on the device:\n' +
            '  Hive app > Settings > "Sign raw hashes" (or "Hash signing") > Enabled\n' +
            'then run this again. (This signer signs the digest directly, which requires that policy.)',
          6,
        )
      }
    } catch {
      /* older apps may not implement getSettings; let the sign attempt surface it */
    }

    process.stderr.write('review and approve the hash on your Ledger...\n')
    const sig: any = await hive.signHash(digest, path)
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
