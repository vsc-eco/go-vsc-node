#!/usr/bin/env bash
# Rebuilds MockUSDC.bin (creation bytecode, hex) from MockUSDC.sol. The devnet
# USDC wave deploys the committed MockUSDC.bin; run this after editing the
# source and commit both. Needs solc 0.8.33 on PATH.
set -euo pipefail
cd "$(dirname "$0")"
solc --version | grep -q 'Version: 0.8.33+' || { echo "need solc 0.8.33" >&2; exit 1; }
solc --bin --optimize --optimize-runs 200 --evm-version cancun --metadata-hash none MockUSDC.sol 2>/dev/null |
	awk '/^======= MockUSDC.sol:MockUSDC =======/{f=1} f&&/^[0-9a-f]+$/{print; exit}' > MockUSDC.bin
test -s MockUSDC.bin
