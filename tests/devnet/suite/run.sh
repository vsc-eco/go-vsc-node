#!/usr/bin/env bash
# Runs a list of devnet tests one at a time and records each result, so a long
# list survives interruptions: rerun with the same run directory to resume.
# See README.md in this directory.
set -euo pipefail

usage() {
	cat <<'EOF'
usage: tests/devnet/suite/run.sh [-l LIST] [-o DIR] [-f] [-r]
  -l LIST  test list (default: tests/devnet/suite/lists/full.txt)
  -o DIR   run directory (default: .devnet/suite/<UTC date-time>); run again
           with the same DIR to resume where it stopped
  -f       fast blocks (DEVNET_FASTBLOCKS=1)
  -r       rerun the tests that did not pass in DIR (FAIL, ERROR, NO-INPUT)
Stop after the current test: touch DIR/STOP. Ctrl-C stops now (the harness
tears the running devnet down first).
EOF
}

die() { echo "suite: $*" >&2; exit 1; }

suite_dir=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$suite_dir/../../.." && pwd)
list=$suite_dir/lists/full.txt
dir=""
fast=""
rerun=""
while getopts "l:o:frh" opt; do
	case $opt in
	l) list=$OPTARG ;;
	o) dir=$OPTARG ;;
	f) fast=1 ;;
	r) rerun=1 ;;
	*) usage; exit 2 ;;
	esac
done
[ -f "$list" ] || die "no test list at $list"
[ -n "$dir" ] || dir=$repo/.devnet/suite/$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$dir/logs" "$dir/nodes"
dir=$(cd "$dir" && pwd)

exec 9>"$dir/.lock"
flock -n 9 || die "another run is using $dir"

if [ -f "$HOME/.wasmedge/env" ]; then
	set +u
	# shellcheck disable=SC1091
	. "$HOME/.wasmedge/env"
	set -u
fi

results=$dir/results.tsv
[ -f "$results" ] || printf 'test\tverdict\trc\tstart\tseconds\tfast\tlog\n' >"$results"
min_free_gb=${SUITE_MIN_FREE_GB:-10}
test_timeout=${SUITE_TEST_TIMEOUT:-200m}

# done_verdict prints the last recorded verdict of a test in this run, if any.
done_verdict() { awk -F'\t' -v t="$1" '$1 == t { v = $2 } END { print v }' "$results"; }

free_gb() { df -BG --output=avail "$1" 2>/dev/null | tail -1 | tr -dc 0-9; }

docker_root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)

stop_now=""
trap 'stop_now=1' INT TERM

total=$(grep -cvE '^\s*(#|$)' "$list" || true)
n=0
while read -r test rest; do
	case $test in '' | \#*) continue ;; esac
	n=$((n + 1))
	prev=$(done_verdict "$test")
	if [ -n "$prev" ] && { [ -z "$rerun" ] || [ "$prev" = PASS ] || [ "$prev" = SKIP ]; }; then
		continue
	fi
	[ -f "$dir/STOP" ] && { echo "suite: stopped by $dir/STOP before $test"; break; }
	[ -n "$stop_now" ] && break

	for path in "$repo" "$docker_root"; do
		[ -n "$path" ] || continue
		free=$(free_gb "$path")
		if [ -n "$free" ] && [ "$free" -lt "$min_free_gb" ]; then
			die "only ${free}G free on $path (SUITE_MIN_FREE_GB=$min_free_gb); stopping before $test"
		fi
	done

	# The list's settings, with ${VAR} references filled in from the environment.
	envs=()
	missing=""
	read -r -a kvs <<<"$rest"
	for kv in ${kvs[@]+"${kvs[@]}"}; do
		while [[ $kv =~ \$\{([A-Za-z_][A-Za-z0-9_]*)\} ]]; do
			name=${BASH_REMATCH[1]}
			[ -n "${!name:-}" ] || missing="$missing $name"
			kv=${kv//\$\{$name\}/${!name:-}}
		done
		envs+=("$kv")
	done
	start=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	log=$dir/logs/$test.log
	if [ -n "$missing" ]; then
		printf '%s\tNO-INPUT\t-\t%s\t0\t%s\t%s\n' "$test" "$start" "${fast:-0}" "needs:$missing" >>"$results"
		echo "[$n/$total] $test NO-INPUT (set:$missing)"
		continue
	fi

	printf '%s\t%s\t%s\n' "$test" "$start" "$log" >"$dir/current"
	t0=$(date +%s)
	set +e
	(cd "$repo" && env ${envs[@]+"${envs[@]}"} DEVNET_LOG_DIR="$dir/nodes/$test" ${fast:+DEVNET_FASTBLOCKS=1} \
		go test -v -count=1 -run "^${test}\$" -timeout "$test_timeout" ./tests/devnet/ >"$log" 2>&1 </dev/null)
	rc=$?
	set -e
	secs=$(($(date +%s) - t0))
	rm -f "$dir/current"
	if [ -n "$stop_now" ]; then
		echo "suite: interrupted during $test; not recorded, so the next run starts with it"
		break
	fi

	if grep -q "^--- PASS: $test " "$log"; then
		verdict=PASS
	elif grep -q "^--- FAIL: $test " "$log"; then
		verdict=FAIL
	elif grep -q -- "--- SKIP: $test " "$log"; then
		verdict=SKIP
	elif grep -q "no tests to run" "$log"; then
		verdict=NO-TEST
	else
		verdict=ERROR
	fi
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$test" "$verdict" "$rc" "$start" "$secs" "${fast:-0}" "$log" >>"$results"
	printf '[%d/%d] %s %s %dm%02ds\n' "$n" "$total" "$test" "$verdict" $((secs / 60)) $((secs % 60))
done <"$list"

echo "suite: $dir"
awk -F'\t' 'NR > 1 { v[$1] = $2; s[$1] = $5 } END {
	for (t in v) { c[v[t]]++; total += s[t] }
	for (k in c) printf "  %s %d\n", k, c[k]
	printf "  time %.1fh\n", total / 3600 }' "$results"
