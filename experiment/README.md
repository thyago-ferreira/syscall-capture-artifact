# Capture experiment — comparison of capture tools

Measures each non-intrusive capture tool against a capture-free baseline, using
`cs-emulator` (`cs-emu`) as the calibrated load and `cs-receiver` (destination
side) as ground truth.

| Indicator | Where | How |
|---|---|---|
| Event loss | capture | `compare.py` (truth − capture, by seq) |
| Payload integrity | capture | `compare.py` (self-check); `finalize.py` (cross-sha256 against `received.jsonl`) |
| CPU (%) | origin host | `sampler.py` (system-wide `/proc/stat` and per process) |
| Memory (peak RSS) | origin host | `sampler.py` (`/proc/<pid>/status`) |
| Send latency | origin host | `cs-emu --lat-out` (duration of each write) + `latency.py` |

`run-matrix.sh` runs the conditions × load levels × replicas matrix and
aggregates everything into a `summary.csv` (one column per indicator).
`sampler.py` and `--lat-out` are started automatically in each run.

## Two measurement phases

1. **Live** (`run-matrix.sh`, origin only): loss (against the emulator's own
   log), self-check integrity (Content-Length), host CPU/memory and send latency.
   Does not depend on the destination during the run.
2. **Finalize** (`finalize.py`, after copying `received.jsonl` from the
   destination): cross-sha256 integrity (captured body vs. received body),
   delivery loss and end-to-end latency. Produces `summary-final.csv`.

Finalization is correct because each run uses a disjoint `seq` range
(`cs-emu --seq-base`, assigned by the harness): sequence numbers are unique
across the whole matrix, so the append-only `received.jsonl` can be sliced per
run. End-to-end latency requires synchronized clocks between the two hosts
(`finalize.py` warns when it detects clock skew).

## Topology

Two hosts. The emulator runs on the origin under the capture tool; `cs-receiver`
runs on the destination and records the ground truth.

```
ORIGIN                                            DESTINATION
  cs-emu (under capture) ──── TCP write ────────>  cs-receiver
        │                                                │
   capture tool                                   received.jsonl (ground truth)
        │
   parse → capture.jsonl
        └──────────────── compare.py ────────────────────┘
                 loss = received − capture
                 integrity = sha256(body) per seq
```

`capture.jsonl` and `received.jsonl` are matched by `seq` (embedded by the
emulator) and compared by the `sha256` of the event body.

## Single run by hand (strace)

1. Destination:
   ```
   cd cs-emulator
   go run ./cmd/cs-receiver --listen :9000 --out received.jsonl
   ```
2. Origin:
   ```
   cd experiment/strace
   ./capture.sh --dest <destination>:9000 --size 6300 --rate 1 --ramp 1 \
                --conn per-event --duration 30s
   python3 parse_strace.py strace_raw.txt > capture.jsonl
   ```
3. Compare (with both files on the same host):
   ```
   python3 experiment/compare.py received.jsonl capture.jsonl
   ```
   Exits with 0 when there is no loss and no hash mismatch.

The eBPF capture (`ebpf/`, `--attach kprobe|fentry`) produces `capture.jsonl` in
the same format and reuses `compare.py`. For full ramps use `run-matrix.sh`
(see the top-level README).
