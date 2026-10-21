#!/usr/bin/env bash
# Builds every artifact the EVM bridge devnet waves (-tags evm_devnet) load,
# each from its own source repo with that repo's pinned toolchain, and prints
# the exports the waves read:
#
#   eval "$(tests/devnet/suite/evm-artifacts.sh)"
#
#   ACCOUNT_MAPPING_DIR  account-mapping checkout   (default: ../account-mapping next to this repo)
#   ZK_VERIFIER_DIR      zk-header-verifier checkout (default: ../zk-header-verifier)
#   OUT                  output directory            (default: .devnet/evm-artifacts in this repo)
#
# Artifacts:
#   EVM_BRIDGE_WASM         the bridge contract as it is deployed
#   EVM_BRIDGE_DEVNET_WASM  the same source with -tags devnet_fasttimelock (admin
#                           timelocks of a few blocks, so a devnet can wire it up)
#   ZK_VERIFIER_WASM        the ZK header verifier
#   EVM_MOCK_VERIFIER_WASM  zk-header-verifier/mock-verifier: plants headers with
#                           real roots and no proof (devnet test double, never deployed)
#   EVM_BOT_BIN             cmd/evm-mapping-bot from this repo
set -euo pipefail

repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
am=${ACCOUNT_MAPPING_DIR:-$repo/../account-mapping}/evm-mapping-contract
zk=${ZK_VERIFIER_DIR:-$repo/../zk-header-verifier}
out=${OUT:-$repo/.devnet/evm-artifacts}
for d in "$am" "$zk"; do
	[ -d "$d" ] || { echo "missing source repo: $d" >&2; exit 1; }
done
mkdir -p "$out"

# The bridge: the image its Makefile pins (by digest) and its build flags.
am_image=$(sed -n 's/^TINYGO_IMAGE := //p' "$am/Makefile")
am_flags=$(sed -n 's/^BUILD_FLAGS = //p' "$am/Makefile")
[ -n "$am_image" ] && [ -n "$am_flags" ] || { echo "cannot read TINYGO_IMAGE/BUILD_FLAGS from $am/Makefile" >&2; exit 1; }
bridge() {
	# shellcheck disable=SC2086
	docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOPATH=/tmp/go -e GOCACHE=/tmp/gocache \
		-v "$am":/work -w /work "$am_image" tinygo build $am_flags "$@" ./contract >&2
}
bridge -o /work/bin/main.wasm
bridge -tags devnet_fasttimelock -o /work/bin/main-devnet.wasm
cp "$am/bin/main.wasm" "$out/evm-bridge.wasm"
cp "$am/bin/main-devnet.wasm" "$out/evm-bridge-devnet.wasm"

# The verifier and its mock: the verifier repo's own build (pinned there).
make -C "$zk" build >&2
"$zk/mock-verifier/build.sh" >&2
cp "$zk/bin/main.wasm" "$out/zk-verifier.wasm"
cp "$zk/bin/mock-verifier.wasm" "$out/mock-verifier.wasm"

(cd "$repo" && go build -o "$out/evm-mapping-bot" ./cmd/evm-mapping-bot)

for f in evm-bridge.wasm evm-bridge-devnet.wasm zk-verifier.wasm mock-verifier.wasm evm-mapping-bot; do
	echo "# $(sha256sum "$out/$f" | cut -c1-16)  $f" >&2
done
cat <<EOF
export EVM_BRIDGE_WASM=$out/evm-bridge.wasm
export EVM_BRIDGE_DEVNET_WASM=$out/evm-bridge-devnet.wasm
export ZK_VERIFIER_WASM=$out/zk-verifier.wasm
export EVM_MOCK_VERIFIER_WASM=$out/mock-verifier.wasm
export EVM_BOT_BIN=$out/evm-mapping-bot
EOF
