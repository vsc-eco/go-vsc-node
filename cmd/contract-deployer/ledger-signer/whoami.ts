#!/usr/bin/env node
// Tiny Ledger connection test for the VSC contract-deployer signer.
//
// It opens the device, reads the Hive app name/version, the public key at a
// derivation path, and whether hash signing is enabled — exercising the exact
// transport + APDU path the real signer uses, without building or sending a
// transaction. Use it to confirm the cable/app/path before a deploy.
//
// Usage:
//   pnpm -s -C cmd/contract-deployer/ledger-signer run pubkey            # default path
//   pnpm -s -C cmd/contract-deployer/ledger-signer run pubkey -- "m/48'/13'/0'/0'/0'" --confirm
//
//   arg 1     : BIP32 path (default m/48'/13'/0'/0'/0', the deployer's default)
//   --confirm : also display the address on the device for visual verification
//
// All diagnostics go to stderr; the public key is printed to stdout.

function fail(msg: string, code?: number): never {
  process.stderr.write(String(msg) + "\n");
  process.exit(code ?? 1);
}

function load(name: string): any {
  try {
    return require(name);
  } catch (e: any) {
    if (
      e &&
      e.code === "MODULE_NOT_FOUND" &&
      String(e.message).includes(name)
    ) {
      fail(
        `missing dependency '${name}'. Run 'pnpm install' in cmd/contract-deployer/ledger-signer/ ` +
          `(see its README.md).`,
        2,
      );
    }
    throw e;
  }
}

const DEFAULT_PATH = "m/48'/13'/1'/0'/0'";

async function main(): Promise<void> {
  const args = process.argv.slice(2);
  const confirm = args.includes("--confirm");
  const path = args.find((a) => a.startsWith("m/")) || DEFAULT_PATH;

  const aioha = load("@aioha/ledger-app-hive");
  const Hive: any = aioha.default || aioha.Hive || aioha;
  const transportMod = load("@ledgerhq/hw-transport-node-hid");
  const TransportNodeHid: any = transportMod.default || transportMod;

  let transport: any;
  try {
    transport = await TransportNodeHid.create();
  } catch (e: any) {
    fail(
      "could not open the Ledger (plugged in, unlocked, Hive app open?): " +
        e.message,
      4,
    );
  }

  try {
    const hive = new Hive(transport);

    // App identity — confirms the Hive app (not the dashboard/another app) is open.
    try {
      const name = await hive.getAppName();
      const version = await hive.getAppVersion();
      process.stderr.write(`device app: ${name} v${version}\n`);
    } catch (e: any) {
      process.stderr.write(
        "warning: could not read app name/version: " + (e && e.message) + "\n",
      );
    }

    // Hash-signing policy — the real signer needs this enabled.
    try {
      const settings = await hive.getSettings();
      process.stderr.write(
        `hash signing: ${settings && settings.hashSignPolicy ? "ENABLED" : "DISABLED (enable it to sign)"}\n`,
      );
    } catch {
      /* older apps may not implement getSettings */
    }

    process.stderr.write(
      `reading public key at ${path}${confirm ? " (confirm on device)" : ""}...\n`,
    );
    const pubkey: string = await hive.getPublicKey(path, confirm);
    process.stdout.write(pubkey.trim() + "\n");
  } catch (e: any) {
    fail(
      "failed to read public key: " + (e && e.message ? e.message : String(e)),
      5,
    );
  } finally {
    try {
      await transport.close();
    } catch {
      /* ignore */
    }
  }
}

main().catch((e: any) =>
  fail("unexpected error: " + (e && e.message ? e.message : String(e)), 1),
);
