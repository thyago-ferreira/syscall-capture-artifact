#!/usr/bin/env bash
# run-stress.sh — RAMPA DE ESTRESSE (NÃO é a rampa fiel; NÃO misturar no gráfico).
#
# Propósito ÚNICO: localizar o ponto de saturação da FERRAMENTA DE CAPTURA — onde
# o eBPF começa a perder (overflow de ringbuf). NÃO reproduz o CS.
#
# Mecanismo: sobe N conexões per-event PARALELAS (cada emu no seu teto, ~10k/s em
# localhost), dobrando N até os dois eBPF atingirem failure_mode=loss, com teto de
# segurança (maior N de NLIST). Cada emu é uma conexão Connection:close própria.
#
# DECLARAÇÃO P/ O PAPER: N conexões efêmeras paralelas NÃO é o perfil calibrado
# (1 CS), mas é estruturalmente próximo do CS real (várias câmeras abrindo
# conexões concorrentes) — mais próximo do real do que uma conexão persistente
# seria. Serve só para achar o limite da captura, não para medir o cenário.
#
# Exige root (eBPF/strace). Roda com sudo. Usa receiver LOCAL (alta taxa).
set -uo pipefail
# Locale C: o sistema é pt_BR e awk printf "%.1f" usaria VÍRGULA decimal,
# quebrando o CSV (vira campo extra). C força ponto. (run-matrix não sofria: os
# números vêm do emu/Go e do sampler/Python, ambos com ponto.)
export LC_ALL=C

DEST=${DEST:-127.0.0.1:9100}        # receiver LOCAL (não o destino real)
SIZE=${SIZE:-6300}
EMU_RAMP=${EMU_RAMP:-16384}         # cada emu no teto per-event localhost (~10k/s)
DURATION=${DURATION:-10s}
NLIST=${NLIST:-"1 2 4 8 16 32 64"}  # paralelismo: doubling + teto de segurança
CONDITIONS=${CONDITIONS:-"baseline strace ebpf-kprobe ebpf-fentry"}
REPLICAS=${REPLICAS:-10}
LOSS_TOL=${LOSS_TOL:-1.0}           # % de perda p/ marcar failure_mode=loss
INTERVAL=${INTERVAL:-0.2}           # amostragem CPU/mem (diagnóstico do gargalo)

EXP_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$EXP_DIR/.." && pwd)
TS=${TS:-$(date +%Y%m%d-%H%M%S)}
OUT=${OUT:-$EXP_DIR/results/stress-$TS}
BIN=${BIN:-/tmp/cs-emu.matrix.bin}
EBPF_BIN=${EBPF_BIN:-/tmp/cs-ebpf-capture}
RECV_BIN=${RECV_BIN:-/tmp/cs-recv.bin}
# Faixa de seq disjunta das outras matrizes (fiel usa < 3e9; aqui começa em 3e10).
SEQ_STRIDE=${SEQ_STRIDE:-100000000}
SEQ_ORIGIN=${SEQ_ORIGIN:-30000000000}
GIDX=0

case " $CONDITIONS " in
  *ebpf-*|*strace*)
    [ "$(id -u)" -eq 0 ] || { echo "ERRO: exige root (eBPF/strace). Rode com sudo." >&2; exit 1; } ;;
esac
for b in "$BIN" "$RECV_BIN"; do
  [ -x "$b" ] || { echo "ERRO: $b ausente. Pré-compile como usuário." >&2; exit 1; }
done
case " $CONDITIONS " in *ebpf-*) [ -x "$EBPF_BIN" ] || { echo "ERRO: $EBPF_BIN ausente (compile em experiment/ebpf)." >&2; exit 1; } ;; esac

mkdir -p "$OUT"
# Receiver local SÓ absorve a carga — NÃO grava (--out /dev/null): a estresse mede
# perda pelos contadores do eBPF (dropped), não por received/capture. Gravar a N
# alto (~6M eventos/célula) encheria o disco (ver incidente 2026-06-25).
"$RECV_BIN" --listen "$DEST" --out /dev/null --quiet 2> "$OUT/recv.log" &
RECV=$!
trap 'kill "$RECV" 2>/dev/null' EXIT
sleep 0.5

SUMMARY="$OUT/summary-stress.csv"
# Colunas de diagnóstico (total_errors, cpu_sys_*) são p/ ACHAR o gargalo quando o
# eBPF NÃO perde: erros nos emus => receiver/rede saturou; cpu_sys no teto => CPU
# da origem; agg_rate em platô entre N => gerador/receiver, não o eBPF.
echo "condition,N,rep,emu_ramp,total_sent,total_errors,agg_rate,cpu_sys_avg,cpu_sys_peak,ebpf_matched,ebpf_gravados,ebpf_dropped,loss_pct,failure_mode" > "$SUMMARY"

dur_s() { awk -v d="$DURATION" 'BEGIN{ gsub(/[^0-9.]/,"",d); print d+0 }'; }
samp_avg()  { sed -n "s/^$1,\([-0-9.]*\),[-0-9.]*/\1/p"  "$2" | tail -1; }
samp_peak() { sed -n "s/^$1,[-0-9.]*,\([-0-9.]*\)/\1/p"  "$2" | tail -1; }

# Dispara N emus em paralelo (stderr por emu em $1/emu-i.log), seq-base disjunto.
launch_emus() {
  local rd=$1 N=$2 i base
  EMU_PIDS=()
  for ((i = 0; i < N; i++)); do
    base=$(( SEQ_ORIGIN + GIDX * SEQ_STRIDE )); GIDX=$(( GIDX + 1 ))
    "$BIN" --dest "$DEST" --size "$SIZE" --rate 1 --ramp "$EMU_RAMP" --conn per-event \
      --duration "$DURATION" --seq-base "$base" 2> "$rd/emu-$i.log" &
    EMU_PIDS+=($!)
  done
}

# Soma um campo numérico ($2: enviados|erros) dos logs dos emus em $1.
sum_field() {
  local rd=$1 fld=$2 t=0 v f
  for f in "$rd"/emu-*.log; do
    [ -f "$f" ] || continue
    v=$(sed -n "s/.*$fld=\([0-9]*\).*/\1/p" "$f" | tail -1)
    t=$(( t + ${v:-0} ))
  done
  echo "$t"
}

emu_field_log() { sed -n "s/.*$1=\([0-9]*\).*/\1/p" "$2" | tail -1; }

run_cell() {
  local cond=$1 N=$2 rep=$3
  local rd="$OUT/$cond/N-$N/rep-$rep"; mkdir -p "$rd"
  local metsum="$rd/metrics.summary"
  echo ">> [$cond] N=$N rep=$rep/$REPLICAS (cada emu ramp=$EMU_RAMP, ${DURATION})" >&2

  local matched=n/a grav=n/a drop=n/a losspct=n/a fmode=ok

  # Sampler p/ diagnóstico do gargalo (CPU sistema + por processo).
  python3 "$EXP_DIR/sampler.py" --out "$rd/metrics.csv" --interval "$INTERVAL" \
    --match cs-emu --match cs-ebpf --match strace --match cs-recv \
    --summary "$metsum" 2> "$rd/sampler.err" &
  local samp=$!

  case "$cond" in
    baseline)
      launch_emus "$rd" "$N"; wait "${EMU_PIDS[@]}" 2>/dev/null
      ;;
    strace)
      # strace -f traça o bash e TODOS os emus filhos (mesmo regime sob teste);
      # -o /dev/null: não precisamos do raw, só do estrangulamento (throttle).
      local base0=$(( SEQ_ORIGIN + GIDX * SEQ_STRIDE )); GIDX=$(( GIDX + N ))
      strace -f -o /dev/null -e trace=write,sendto bash -c '
        for ((i=0; i<'"$N"'; i++)); do
          "'"$BIN"'" --dest "'"$DEST"'" --size "'"$SIZE"'" --rate 1 --ramp "'"$EMU_RAMP"'" \
            --conn per-event --duration "'"$DURATION"'" \
            --seq-base $(( '"$base0"' + i*'"$SEQ_STRIDE"' )) 2>"'"$rd"'/emu-$i.log" &
        done; wait' 2> "$rd/strace.log"
      ;;
    ebpf-kprobe|ebpf-fentry)
      local mode=${cond#ebpf-}
      # --out /dev/null: a perda vem dos contadores (matched/gravados/dropped) no
      # ebpf.log, não do capture.jsonl. Evita encher o disco a N alto.
      "$EBPF_BIN" --attach "$mode" --out /dev/null 2> "$rd/ebpf.log" &
      local cap=$!
      sleep 1 # deixa o BPF anexar
      launch_emus "$rd" "$N"; wait "${EMU_PIDS[@]}" 2>/dev/null
      sleep 0.5 # drena o ringbuf
      kill -INT "$cap" 2>/dev/null; wait "$cap" 2>/dev/null
      matched=$(emu_field_log matched "$rd/ebpf.log")
      grav=$(emu_field_log gravados "$rd/ebpf.log")
      drop=$(emu_field_log dropped_ringbuf "$rd/ebpf.log")
      matched=${matched:-0}; grav=${grav:-0}; drop=${drop:-0}
      ;;
  esac

  kill -INT "$samp" 2>/dev/null; wait "$samp" 2>/dev/null

  local total errors agg cpu_avg cpu_peak
  total=$(sum_field "$rd" enviados)
  errors=$(sum_field "$rd" erros)
  agg=$(awk -v t="$total" -v d="$(dur_s)" 'BEGIN{ printf "%.1f", (d>0)? t/d : 0 }')
  cpu_avg=$(samp_avg cpu_sys_pct "$metsum"); cpu_peak=$(samp_peak cpu_sys_pct "$metsum")
  cpu_avg=${cpu_avg:-n/a}; cpu_peak=${cpu_peak:-n/a}

  # failure_mode: eBPF -> loss (overflow); strace -> saturation se a taxa por emu
  # despencou (estrangulado) vs o teto ~10k/s; baseline -> ok.
  if [ "$cond" = "ebpf-kprobe" ] || [ "$cond" = "ebpf-fentry" ]; then
    losspct=$(awk -v dr="$drop" -v m="$matched" 'BEGIN{ printf "%.2f", (m>0)? dr/m*100 : 0 }')
    fmode=$(awk -v lp="$losspct" -v lt="$LOSS_TOL" 'BEGIN{ print (lp+0 > lt)? "loss":"ok" }')
  elif [ "$cond" = "strace" ]; then
    # estrangulado se a taxa/emu < ~3000/s (bem abaixo do teto ~10k/s do per-event).
    fmode=$(awk -v a="$agg" -v n="$N" 'BEGIN{ print (n>0 && a/n < 3000)? "saturation":"ok" }')
  fi

  echo "$cond,$N,$rep,$EMU_RAMP,$total,$errors,$agg,$cpu_avg,$cpu_peak,$matched,$grav,$drop,$losspct,$fmode" >> "$SUMMARY"
}

echo ">> ESTRESSE: condições=[$CONDITIONS] N=[$NLIST] réplicas=$REPLICAS emu_ramp=$EMU_RAMP dur=$DURATION dest=$DEST(local)" >&2
for cond in $CONDITIONS; do
  for N in $NLIST; do
    for rep in $(seq 1 "$REPLICAS"); do
      run_cell "$cond" "$N" "$rep"
    done
  done
done

echo ">> CONCLUÍDO. Resultados em $OUT" >&2
echo "" >&2
column -t -s, "$SUMMARY" >&2
