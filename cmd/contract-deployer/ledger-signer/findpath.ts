#!/usr/bin/env node
// Find which Ledger derivation path holds a known Hive public key.
//
// The Hive app derives a different key per BIP32 path, and owner/active/posting
// often live at sibling paths (one of the trailing hardened components encodes
// the role/account/key index). Reading a public key needs no on-device approval,
// so this brute-forces a small grid of paths and reports which one matches.
//
// Usage:
//   pnpm -s -C cmd/contract-deployer/ledger-signer run findpath -- STM<yourActiveKey>
//   pnpm -s -C cmd/contract-deployer/ledger-signer run findpath -- STM<key> --max 8
//   pnpm -s -C cmd/contract-deployer/ledger-signer run findpath -- STM<key> --base "m/48'/13'"
//
//   arg 1   : the target public key (STM…), required
//   --max N : scan each of the 3 trailing hardened indices over 0..N-1 (default 5)
//   --base  : path prefix the 3 indices are appended to (default "m/48'/13'")
//
// Matching paths are printed to stdout; progress/diagnostics go to stderr.

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

function argValue(args: string[], flag: string): string | undefined {
  const i = args.indexOf(flag)
  return i >= 0 && i + 1 < args.length ? args[i + 1] : undefined
}

async function main(): Promise<void> {
  const args = process.argv.slice(2)
  const target = args.find((a) => a.startsWith('STM') || a.startsWith('TST'))
  if (!target) fail('provide the target public key as the first argument, e.g. STM6n4Wc...', 1)
  const max = parseInt(argValue(args, '--max') || '5', 10)
  if (!Number.isInteger(max) || max < 1 || max > 50) fail('--max must be an integer in 1..50', 1)
  const base = (argValue(args, '--base') || "m/48'/13'").replace(/\/+$/, '')
  const prefix = target.slice(0, 3) // STM (mainnet) or TST (testnet)

  const aioha = load('@aioha/ledger-app-hive')
  const Hive: any = aioha.default || aioha.Hive || aioha
  const transportMod = load('@ledgerhq/hw-transport-node-hid')
  const TransportNodeHid: any = transportMod.default || transportMod

  let transport: any
  try {
    transport = await TransportNodeHid.create()
  } catch (e: any) {
    fail('could not open the Ledger (plugged in, unlocked, Hive app open?): ' + e.message, 4)
  }

  const matches: string[] = []
  try {
    const hive = new Hive(transport)
    const total = max * max * max
    process.stderr.write(`scanning ${total} paths under ${base}/a'/b'/c' for ${target} ...\n`)

    let n = 0
    for (let a = 0; a < max; a++) {
      for (let b = 0; b < max; b++) {
        for (let c = 0; c < max; c++) {
          const path = `${base}/${a}'/${b}'/${c}'`
          let key: string
          try {
            key = (await hive.getPublicKey(path, false, prefix)).trim()
          } catch (e: any) {
            fail(`\nerror reading ${path}: ${(e && e.message) || e}\n(unlock the device and open the Hive app, then retry)`, 5)
          }
          n++
          if (key === target) {
            process.stderr.write(`\nMATCH: ${path}\n`)
            process.stdout.write(path + '\n')
            matches.push(path)
          }
          if (n % 10 === 0 || n === total) process.stderr.write(`  ...${n}/${total}\r`)
        }
      }
    }
    process.stderr.write('\n')
  } finally {
    try {
      await transport.close()
    } catch {
      /* ignore */
    }
  }

  if (matches.length === 0) {
    fail(
      `no match in ${max}^3 paths under ${base}. Try a larger grid (--max), a different ` +
        `--base, or confirm the key is from this device/seed.`,
      3,
    )
  }
}

main().catch((e: any) => fail('unexpected error: ' + (e && e.message ? e.message : String(e)), 1))
