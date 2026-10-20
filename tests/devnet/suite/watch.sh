#!/usr/bin/env bash
# Watches a running suite and prints one line per event, so a hung test is seen
# in minutes instead of hours. Run it next to run.sh: watch.sh DIR [-b BASELINE]
#   RESULT  a test finished
#   STALL   the running test has taken longer than its baseline duration (half
#           again for a test held at real cadence), or 15 min without a
#           baseline; repeats every 10 min
#   QUIET   the running test's log has not grown for 5 min (15 at real cadence)
#   HOLD    the running test is waiting for its HOLD file (DEVNET_KEEP)
#   DONE    the runner has exited
set -euo pipefail

[ $# -ge 1 ] || { echo "usage: watch.sh DIR [-b BASELINE_DIR]" >&2; exit 2; }
dir=$1
shift
baseline=""
[ "${1:-}" = -b ] && baseline=${2:-}
results=$dir/results.tsv

# expected prints a test's duration in whole minutes in the baseline run (0 if none).
expected() {
	[ -n "$baseline" ] && [ -f "$baseline/results.tsv" ] || { echo 0; return; }
	awk -F'\t' -v t="$1" '$1 == t && $2 == "PASS" { s = $5 } END { print int(s / 60) }' "$baseline/results.tsv"
}

runner_alive() { ! flock -n "$dir/.lock" true 2>/dev/null; }

seen=$(($(wc -l <"$results" 2>/dev/null || echo 1)))
cur="" next=0 quiet=0 real="" hold=""
while :; do
	lines=$(wc -l <"$results" 2>/dev/null || echo 0)
	if [ "$lines" -gt "$seen" ]; then
		tail -n +"$((seen + 1))" "$results" | awk -F'\t' '{ printf "RESULT %s %s %dm%02ds\n", $1, $2, $5 / 60, $5 % 60 }'
		seen=$lines
	fi
	if [ -f "$dir/current" ]; then
		read -r test start log <"$dir/current"
		now=$(date +%s)
		el=$(((now - $(date -d "$start" +%s)) / 60))
		if [ "$test" != "$cur" ]; then
			cur=$test exp=$(expected "$test") quiet=0 real="" hold=""
			[ "$exp" -gt 0 ] || exp=15
			next=$exp
		fi
		if [ -f "$log" ]; then
			if [ -z "$real" ] && {
				grep -aq 'DEVNET_FASTBLOCKS=1, but' "$log" ||
					[ "$(grep -aoE 'early [0-9]+ / capped [0-9]+' "$log" | tail -1 | awk '{ print ($5 + 0 > $2 + 0) }')" = 1 ]
			}; then
				real=1 exp=$((exp * 3 / 2)) next=$exp
			fi
			idle=$(((now - $(stat -c %Y "$log")) / 60))
			q=5
			[ -n "$real" ] && q=15
			if [ "$idle" -ge "$q" ] && [ "$quiet" = 0 ]; then
				echo "QUIET $test log silent ${idle}m (running ${el}m)"
				quiet=1
			fi
			[ "$idle" -lt "$q" ] && quiet=0
			if [ -z "$hold" ] && grep -aq 'to tear down, run:' "$log"; then
				echo "HOLD $test is waiting for its HOLD file to be deleted (DEVNET_KEEP)"
				hold=1
			fi
		fi
		if [ "$el" -ge "$next" ]; then
			echo "STALL $test running ${el}m (expected about ${exp}m)"
			next=$((next + 10))
		fi
	fi
	if ! runner_alive; then
		echo "DONE runner exited"
		exit 0
	fi
	sleep 60
done
