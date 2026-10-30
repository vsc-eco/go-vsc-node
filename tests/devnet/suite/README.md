# Devnet suite runner

Runs a list of devnet tests one at a time, records every result, and resumes
where it stopped. The full list (`lists/full.txt`, 97 tests: vault custody,
consensus, POA) takes about 47 hours at the real 3s block cadence and about 10
hours on fast blocks (see `../README.md`, "Fast blocks").

## Quick start

```bash
# Inputs for the tests that need them (see the header of lists/full.txt)
export BTC_MAPPING_WASM=/path/to/utxo-mapping/bin/dev.wasm
export BTC_MAPPING_V1_WASM=/path/to/btc-mapping-v1/bin/dev.wasm
export OLD_CODE_DIR=/path/to/go-vsc-node-at-the-released-version
export POA_OLD_SOURCE=/path/to/go-vsc-node-from-before-poa

# Whole list on fast blocks, into a new run directory under .devnet/suite/
tests/devnet/suite/run.sh -f

# Same, into a named directory; run the same command again to resume
tests/devnet/suite/run.sh -f -o .devnet/suite/fast-1

# In a second terminal: alerts for stalled, silent or held tests
tests/devnet/suite/watch.sh .devnet/suite/fast-1
```

A test whose input is not set is recorded as `NO-INPUT` and skipped, so the
consensus and POA tests run without any of the four.

## Options and files

| | |
|---|---|
| `-l LIST` | another list (same format: test name, then `KEY=value` settings) |
| `-o DIR` | run directory; reuse it to resume |
| `-f` | fast blocks (`DEVNET_FASTBLOCKS=1`) |
| `-r` | rerun the tests that did not pass (FAIL, ERROR, NO-INPUT) |
| `DIR/STOP` | create it to stop after the current test |
| `SUITE_MIN_FREE_GB` | stop before a test when the repo or Docker disk has less free space (default 10) |
| `SUITE_TEST_TIMEOUT` | `go test -timeout` per test (default 200m) |
| `DEVNET_PORT_OFFSET` | host port offset for the devnet, passed through (keep it at or below 12000) |
| `DEVNET_PREPARAMS_DIR` | pre-generated TSS pre-parameters, passed through (see `../README.md`) |

Each run directory holds:

- `results.tsv`: one row per finished test (verdict, exit code, start, seconds, fast or not, log). A rerun appends a row; the last row counts.
- `logs/<Test>.log`: the `go test -v` output.
- `nodes/<Test>/`: each node's container log and, on fast blocks, `pacemaker-blocks.tsv` (one row per block with what held it).

Verdicts: `PASS`, `FAIL`, `SKIP` (the test skipped itself), `ERROR` (no verdict line, e.g. a build failure or a timeout), `NO-INPUT`, `NO-TEST` (no such test: a typo in the list).

## Stopping and interrupting

- `touch DIR/STOP` ends the run after the current test.
- Ctrl-C stops at once: the harness tears the running devnet down before the test binary exits, and the interrupted test is not recorded, so the next run starts with it.
- If the machine dies mid-test, the next devnet start removes what the dead run left behind (containers, volumes, network, image, data dir).

## Comparing two runs

```bash
tests/devnet/suite/summary.py .devnet/suite/fast-1                       # one run
tests/devnet/suite/summary.py .devnet/suite/fast-1 .devnet/suite/real-1  # against a baseline
```

Against a baseline it compares each test's verdict, its per-case verdicts (vault
`CASE` lines and Go subtests) and its time, and gives the speedup over the tests
that passed in both. A fast-block change is acceptable only when the per-case
verdicts match a real-cadence run.

Measured on the full list (2026-10-20, 5-node devnet, one machine): per-case
verdicts identical to real cadence on every test; 9.0h against 42.0h over the 91
tests that passed both ways (4.7x). Vault tests gain about 5-7x. Short consensus
and POA tests gain 2.5-3x, because about 2 minutes of each run is devnet boot.
Tests with old-code nodes, stopped nodes or network faults run at real cadence by
design (`../README.md`, "Known limits").

## Notes

- One devnet at a time: a devnet is about 12 containers, and two at once roughly
  double the load and the run times. For a second lane on the same machine, give
  it its own `DEVNET_PORT_OFFSET` (a multiple of 1000, at most 12000).
- Do not put `DEVNET_KEEP=1` in a list: it holds every finished devnet open until
  someone deletes its HOLD file. `watch.sh` reports a held test.
- Races between nodes reading the same block (for example the election proposer
  against L2 block production) only show at real cadence. Run timing-sensitive
  tests at real cadence too.
