#!/usr/bin/env bash
# run-size-sweep.sh — EIXO 2: efeito do TAMANHO do evento no overhead (ramp fixo).
#
# Varre --size em 4 pontos (1024, 6300, 16384, 65536 B), TODOS em ramp-1 (carga
# fixa, sem rampa) → isola o tamanho da carga. NÃO cruza com o Eixo 1 (rampa só em
# 6300; sweep só em ramp-1). NÃO é critério do MCDA — é TESTE DE ESTABILIDADE do
# ranking: o ranking de overhead se mantém conforme o corpo cresce?
#
# size_regime: real (6300, observado) | synthetic (demais, envelope de
# generalização). CSV separado (summary-size.csv), não misturar com a rampa.
#
# Receiver LOCAL (o sweep mede o overhead na ORIGEM; o destino não importa p/ o
# custo de captura, e evita transferir 64KB/evento pela rede). eBPF: usar o binário
# REBUILDADO com -DMAX_PAYLOAD=131072 (cobre 64KB) — senão truncaria o corpo grande.
#
# Exige root (eBPF/strace). Roda com sudo.
set -uo pipefail
export LC_ALL=C  # ponto decimal no awk (sistema é pt_BR; ver run-stress)

DEST=${DEST:-127.0.0.1:9100}
SIZES=${SIZES:-"1024 6300 16384 65536"}
RATE=${RATE:-50}                    # carga FIXA (ramp=1); 50/s × DURATION = amostras
RAMP=1                              # FIXO — Eixo 2 não rampa
DURATION=${DURATION:-20s}
CONN=${CONN:-per-event}
CONDITIONS=${CONDITIONS:-"baseline strace ebpf-kprobe ebpf-fentry"}
REPLICAS=${REPLICAS:-5}
INTERVAL=${INTERVAL:-0.2}

EXP_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$EXP_DIR/.." && pwd)
TS=${TS:-$(date +%Y%m%d-%H%M%S)}
OUT=${OUT:-$EXP_DIR/results/size-$TS}
BIN=${BIN:-/tmp/cs-emu.matrix.bin}
EBPF_BIN=${EBPF_BIN:-/tmp/cs-ebpf-capture-big}   # build com MAX_PAYLOAD=131072
RECV_BIN=${RECV_BIN:-/tmp/cs-recv.bin}
SEQ_STRIDE=${SEQ_STRIDE:-100000000}
SEQ_ORIGIN=${SEQ_ORIGIN:-40000000000}
RUN_IDX=0

case " $CONDITIONS " in
  *ebpf-*|*strace*) [ "$(id -u)" -eq 0 ] || { echo "ERRO: eBPF/strace exigem root. Rode com sudo." >&2; exit 1; } ;;
esac
for b in "$BIN" "$RECV_BIN"; do
  [ -x "$b" ] || { echo "ERRO: $b ausente (pré-compile como usuário)." >&2; exit 1; }
done
case " $CONDITIONS " in
  *ebpf-*) [ -x "$EBPF_BIN" ] || { echo "ERRO: $EBPF_BIN ausente. Rebuild: (cd experiment/ebpf && GOPACKAGE=main go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -cflags '-O2 -g -Wall -Werror -D__TARGET_ARCH_x86 -DMAX_PAYLOAD=131072' capture ./bpf/capture.bpf.c -- -I./bpf -I/usr/include && go build -o $EBPF_BIN . && go generate ./...)" >&2; exit 1; } ;;
esac

mkdir -p "$OUT"
"$RECV_BIN" --listen "$DEST" --out /dev/null --quiet 2> "$OUT/recv.log" &
RECV=$!
trap 'kill "$RECV" 2>/dev/null' EXIT
sleep 0.5

SUMMARY="$OUT/summary-size.csv"
echo "condition,size,size_regime,rep,rate,duration,sent,errors,eff_rate,captured,loss,loss_pct,integrity_mode,integrity_bad,cpu_sys_avg,cpu_sys_peak,tool_cpu_peak,emu_rss_peak_kb,lat_p50_us,lat_p95_us,lat_p99_us,lat_max_us,result" > "$SUMMARY"

emu_field() { sed -n "s/.*$1=\([0-9.]*\).*/\1/p" "$2" | tail -1; }
samp_avg()  { sed -n "s/^$1,\([-0-9.]*\),[-0-9.]*/\1/p" "$2" | tail -1; }
samp_peak() { sed -n "s/^$1,[-0-9.]*,\([-0-9.]*\)/\1/p" "$2" | tail -1; }
emu_run() { "$BIN" --dest "$DEST" --size "$1" --rate "$RATE" --ramp "$RAMP" --conn "$CONN" --duration "$DURATION" --lat-out "$3" --seq-base "$4" 2> "$2"; }

run_cell() {
  local cond=$1 size=$2 rep=$3
  local rd="$OUT/$cond/size-$size/rep-$rep"; mkdir -p "$rd"
  local emulog="$rd/emu.log" latout="$rd/lat.jsonl" metsum="$rd/metrics.summary"
  local base=$(( SEQ_ORIGIN + RUN_IDX * SEQ_STRIDE )); RUN_IDX=$(( RUN_IDX + 1 ))
  local regime="synthetic"; [ "$size" = "6300" ] && regime="real"
  echo ">> [$cond] size=${size}B rep=${rep}/${REPLICAS} (regime=$regime, rate=${RATE}/s ramp=1)" >&2

  python3 "$EXP_DIR/sampler.py" --out "$rd/metrics.csv" --interval "$INTERVAL" \
    --match cs-emu --match strace --match cs-ebpf --summary "$metsum" 2> "$rd/sampler.err" &
  local samp=$!

  case "$cond" in
    baseline) emu_run "$size" "$emulog" "$latout" "$base" ;;
    strace)
      strace -f -xx -s 262144 -e trace=write,sendto -o "$rd/strace_raw.txt" \
        "$BIN" --dest "$DEST" --size "$size" --rate "$RATE" --ramp "$RAMP" --conn "$CONN" \
        --duration "$DURATION" --lat-out "$latout" --seq-base "$base" 2> "$emulog"
      python3 "$EXP_DIR/strace/parse_strace.py" "$rd/strace_raw.txt" > "$rd/capture.jsonl" 2> "$rd/parse.err"
      rm -f "$rd/strace_raw.txt"  # descartável pós-parse (grande a 64KB)
      ;;
    ebpf-kprobe|ebpf-fentry)
      local mode=${cond#ebpf-}
      "$EBPF_BIN" --attach "$mode" --out "$rd/capture.jsonl" 2> "$rd/ebpf.log" &
      local cap=$!
      sleep 1
      emu_run "$size" "$emulog" "$latout" "$base"
      sleep 0.5
      kill -INT "$cap" 2>/dev/null; wait "$cap" 2>/dev/null
      ;;
  esac
  kill -INT "$samp" 2>/dev/null; wait "$samp" 2>/dev/null

  local sent errs eff
  sent=$(emu_field enviados "$emulog"); errs=$(emu_field erros "$emulog"); eff=$(emu_field taxa_efetiva "$emulog")
  sent=${sent:-0}; errs=${errs:-0}; eff=${eff:-0}
  local cpu_avg cpu_peak tool_peak emu_rss tool_comm=""
  case "$cond" in strace) tool_comm=strace ;; ebpf-*) tool_comm=cs-ebpf ;; esac
  cpu_avg=$(samp_avg cpu_sys_pct "$metsum"); cpu_peak=$(samp_peak cpu_sys_pct "$metsum")
  emu_rss=$(samp_peak cs-emu_rss_kb "$metsum"); tool_peak=n/a
  [ -n "$tool_comm" ] && tool_peak=$(samp_peak "${tool_comm}_cpu_pct" "$metsum")
  cpu_avg=${cpu_avg:-n/a}; cpu_peak=${cpu_peak:-n/a}; tool_peak=${tool_peak:-n/a}; emu_rss=${emu_rss:-n/a}
  local lp50 lp95 lp99 lmax lrow
  lrow=$(python3 "$EXP_DIR/latency.py" --lat "$latout" --csv-row "$cond" 2>> "$rd/parse.err")
  IFS=',' read -r _l lp50 lp95 lp99 lmax _e1 _e2 <<< "$lrow"
  lp50=${lp50:-n/a}; lp95=${lp95:-n/a}; lp99=${lp99:-n/a}; lmax=${lmax:-n/a}
  local over="$cpu_avg,$cpu_peak,$tool_peak,$emu_rss,$lp50,$lp95,$lp99,$lmax"

  if [ "$cond" = "baseline" ]; then
    echo "$cond,$size,$regime,$rep,$RATE,$DURATION,$sent,$errs,$eff,n/a,n/a,n/a,n/a,n/a,$over,OK" >> "$SUMMARY"
  else
    python3 "$EXP_DIR/truth_from_emu.py" "$emulog" > "$rd/truth.jsonl" 2>> "$rd/parse.err"
    local row
    row=$(python3 "$EXP_DIR/compare.py" --truth "$rd/truth.jsonl" --capture "$rd/capture.jsonl" --csv-row "$cond" 2>> "$rd/parse.err")
    IFS=',' read -r _lab _nt captured missing losspct _extra mode bad result <<< "$row"
    echo "$cond,$size,$regime,$rep,$RATE,$DURATION,$sent,$errs,$eff,$captured,$missing,$losspct,$mode,$bad,$over,$result" >> "$SUMMARY"
  fi
}

echo ">> SWEEP: condições=[$CONDITIONS] sizes=[$SIZES] réplicas=$REPLICAS rate=$RATE ramp=1 dur=$DURATION dest=$DEST(local)" >&2
for cond in $CONDITIONS; do
  for size in $SIZES; do
    for rep in $(seq 1 "$REPLICAS"); do
      run_cell "$cond" "$size" "$rep"
    done
  done
done

echo ">> CONCLUÍDO. Resultados em $OUT" >&2
echo "" >&2
column -t -s, "$SUMMARY" >&2
