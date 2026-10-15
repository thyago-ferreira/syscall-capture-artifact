# Non-intrusive data capture via system-call interception — artifact

This repository contains the testbed, the capture implementations, the
measurement scripts and the aggregated results used in the paper *An Analysis of
Non-Intrusive Data Capture for Interoperability in Systems-of-Systems via System
Calls*.

## Layout

```
cs-emulator/          Load generator and ground-truth receiver (Go)
  cmd/cs-emu/         Emulator of the constituent system (CS): emits the calibrated load
  cmd/cs-receiver/    Destination-side receiver: records length, sha256 and timestamp per event
  internal/           Event builder, sender (one write(2) per event) and rate control
experiment/           Capture tools and measurement harness
  ebpf/               eBPF capture (libbpf/CO-RE, Go loader); --attach kprobe|fentry
  strace/             ptrace-based capture (strace wrapper + parser)
  run-matrix.sh       Faithful ramp: conditions x load levels x replicas
  run-stress.sh       Stress ramp: N parallel per-event connections
  run-size-sweep.sh   Payload-size sweep at the operating rate
  sampler.py          System-wide CPU and per-process memory sampling (/proc)
  latency.py          Send-latency and end-to-end latency percentiles
  compare.py          Event loss and payload integrity (capture vs. ground truth)
  finalize.py         Cross-sha256 integrity, delivery loss and end-to-end latency
  truth_from_emu.py   Ground truth derived from the emulator log
  loadpoints.py       Load-point selection
  HOOKPOINT.md        Empirical confirmation of the intercepted system call
  results/            Aggregated results (see below)
```

## Results used in the paper

| File | Content | Used in |
|---|---|---|
| `experiment/results/fiel-summary.csv` | Faithful ramp, 480 runs (4 conditions x 12 load levels x 10 replicas): CPU, memory, send latency, loss, failure mode | Table 3 (C1-C4), Figure 2 |
| `experiment/results/fiel-summary-final.csv` | Same 480 runs after finalization: cross-sha256 integrity, delivery loss, end-to-end latency | Payload integrity (E3), end-to-end latency |
| `experiment/results/summary-stress.csv` | Stress ramp, N = 1 to 64 parallel connections, 10 replicas | Stress results (Section 5.1) |
| `experiment/results/summary-size.csv` | Size sweep, 1 kB to 64 kB, 5 replicas | Figure 3 |

Raw per-event logs are not included: they are large and are fully summarized by
the files above.

## Requirements

- Two Linux hosts on the same network: the *origin* runs the emulator under the
  capture tool; the *destination* runs the receiver. End-to-end latency requires
  their clocks to be synchronized (e.g., chrony, one host serving the other).
- Go 1.22 or later.
- For the ptrace condition: `strace`.
- For the eBPF conditions: root, a kernel with BTF (`/sys/kernel/btf/vmlinux`),
  `clang`, and the libbpf headers. fentry additionally requires Linux 5.5 or later.

## Building

```
cd cs-emulator && go build ./cmd/cs-emu ./cmd/cs-receiver
cd experiment/ebpf && go generate && go build
```

`go generate` compiles `bpf/capture.bpf.c` with bpf2go. For the size sweep,
rebuild the eBPF program with `-DMAX_PAYLOAD=131072` (see `run-size-sweep.sh`).

## Running

1. Destination: `cs-receiver --listen :8080 --out received.jsonl --quiet`
2. Origin, faithful ramp:
   `DEST=<destination>:8080 RAMPS="1 2 4 8 16 32 64 128 256 512 1024 2048" DURATION=10s ./experiment/run-matrix.sh`
3. Copy `received.jsonl` from the destination to the origin and run
   `python3 experiment/finalize.py --out <results dir> --received <results dir>/received.jsonl`.

The stress ramp and the size sweep follow the same pattern with
`run-stress.sh` and `run-size-sweep.sh`.

A run fails when its effective rate falls below 90% of the target rate or its
event loss exceeds 1%, whichever happens first (`SAT_RATIO` and `LOSS_TOL` in
`run-matrix.sh`).

## Notes

- **Endpoint path.** In this release the emulated endpoint is
  `/events/vehicles`. The results were produced with an endpoint path two bytes
  shorter, so each write was 6,430 bytes instead of 6,432. The event body
  (6,300 bytes), on which integrity and all measurements are computed, is
  identical.
- **Validity of the hook point.** The emulator is written in Go and sends each
  event in a single `write(2)`. It reproduces the *load profile* of the reference
  constituent system (event size, rate, connection pattern), not necessarily the
  system-call pattern of its original runtime, which may use `writev`, `sendto`
  or `sendmsg`.
- Code comments are in Portuguese.
