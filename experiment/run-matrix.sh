#!/usr/bin/env bash
# run-matrix.sh — harness do experimento de captura (roda na MÁQUINA ORIGEM).
#
# Roda a matriz CONDIÇÕES × RAMPS, salvando todos os resultados sob
# results/<timestamp>/ e agregando um summary.csv. A condição "baseline" é o
# emulador LISO E SEM NADA (benchmark do teto nativo); as demais envolvem o
# cs-emu na ferramenta de captura sob teste.
#
# Verdade-base (sem instrumentar o emulador): truth_from_emu.py deriva os seqs
# escritos do stderr do cs-emu; compare.py mede perda + integridade da captura.
#
# Pré-requisitos: cs-receiver no ar na destino (aceita as conexões); por condição,
# a ferramenta correspondente instalada (strace já; eBPF/auditd quando instalados).
#
# Uso (variáveis de ambiente sobrescrevem os defaults):
#   DEST=192.0.2.10:8080 ./run-matrix.sh
#   DEST=... RAMPS="1 2 4 8 16 32 64" DURATION=10s CONDITIONS="baseline strace" ./run-matrix.sh
set -uo pipefail  # NÃO usar -e: runs podem "falhar" (erros/saturação) e seguimos

DEST=${DEST:?defina DEST=host:porta da Máquina destino (ex.: DEST=192.0.2.10:8080)}
SIZE=${SIZE:-6300}
RATE=${RATE:-1}
RAMPS=${RAMPS:-"1 2 4 8 16 32"}
DURATION=${DURATION:-10s}
CONN=${CONN:-per-event}
CONDITIONS=${CONDITIONS:-"baseline strace"}

EXP_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$EXP_DIR/.." && pwd)
TS=${TS:-$(date +%Y%m%d-%H%M%S)}
OUT=${OUT:-$EXP_DIR/results/$TS}
BIN=${BIN:-/tmp/cs-emu.matrix.bin}
# Binário eBPF: PRÉ-COMPILADO como usuário (tem dep cilium/ebpf; build como root
# re-baixaria o módulo). A matriz só o EXECUTA (load = root). Compilar antes:
#   (cd experiment/ebpf && go build -o /tmp/cs-ebpf-capture .)
EBPF_BIN=${EBPF_BIN:-/tmp/cs-ebpf-capture}

# Condições eBPF exigem root (load do BPF). Opção (a): a matriz INTEIRA roda com
# sudo. Falha cedo se pedirem eBPF sem privilégio.
case " $CONDITIONS " in
  *" ebpf-"*|*ebpf-*)
    if [ "$(id -u)" -ne 0 ]; then
      echo "ERRO: condições eBPF exigem root. Rode a matriz com sudo (opção a):" >&2
      echo "  sudo -E env DEST=$DEST ... $0   (preserve o ambiente com -E)" >&2
      exit 1
    fi ;;
esac

mkdir -p "$OUT"
# Compila o cs-emu se houver toolchain Go; sob sudo o PATH seguro do root pode não
# ter o go — nesse caso usa o BIN pré-compilado pelo usuário (sem dep externa, é só
# recompilar antes: go build -o /tmp/cs-emu.matrix.bin ./cmd/cs-emu).
if command -v go >/dev/null 2>&1; then
  echo ">> compilando cs-emu em $BIN" >&2
  ( cd "$REPO_ROOT/cs-emulator" && go build -o "$BIN" ./cmd/cs-emu ) || { echo "build falhou"; exit 1; }
elif [ -x "$BIN" ]; then
  echo ">> go ausente no PATH; usando $BIN pré-compilado" >&2
else
  echo "ERRO: go ausente e $BIN inexistente. Pré-compile como usuário: (cd cs-emulator && go build -o $BIN ./cmd/cs-emu)" >&2
  exit 1
fi

# Overhead do host: amostragem de CPU/memória durante cada run. Padrões de comm
# casados: o binário do emu (comm truncado em 15 chars contém "cs-emu") e a
# ferramenta tracer separada (strace). INTERVAL configurável.
INTERVAL=${INTERVAL:-0.2}

# Faixa de seq por run: cada run recebe [base+1, base+count], base = idx*STRIDE,
# com STRIDE >> qualquer count possível (rampa alta × duração). Assim o
# received.jsonl append-only do cs-receiver não colide entre runs => habilita
# cross-sha256 e latência fim-a-fim por run (ver finalize.py). idx é um contador
# global sobre condições×rampas.
# SEQ_ORIGIN desloca o início de TODAS as faixas desta invocação. Use-o quando
# reusar um received.jsonl já populado por um run anterior (sem reiniciar o
# cs-receiver no destino), p/ as faixas não colidirem entre invocações. Default
# 0 (cenário normal: received.jsonl novo a cada matriz).
SEQ_STRIDE=${SEQ_STRIDE:-100000000}
SEQ_ORIGIN=${SEQ_ORIGIN:-0}
RUN_IDX=0

# Réplicas por célula (condição×rampa). MCDA exige IC => n>=10 (sem réplica não há
# IC). Cada réplica é um run independente, com sua própria faixa de seq (RUN_IDX
# incrementa por réplica) e seu próprio subdir rep-<k>. summary.csv traz UMA linha
# por réplica (coluna `rep`); a agregação média+IC é da análise/MCDA, não do harness.
REPLICAS=${REPLICAS:-10}

# Limiares da definição de FALHA (ver artigo, Seção 3.3): falha = taxa
# efetiva < SAT_RATIO×alvo (saturação) OU perda > LOSS_TOL% (perda), o que vier
# primeiro. Coluna failure_mode = ok | saturation | loss.
SAT_RATIO=${SAT_RATIO:-0.90}
LOSS_TOL=${LOSS_TOL:-1.0}

SUMMARY="$OUT/summary.csv"
echo "condition,rep,rate,ramp,target_rate,duration,sent,errors,eff_rate,captured,loss,loss_pct,extra,integrity_mode,integrity_bad,cpu_sys_avg,cpu_sys_peak,tool_cpu_peak,emu_rss_peak_kb,lat_p50_us,lat_p95_us,lat_p99_us,lat_max_us,failure_mode,result" > "$SUMMARY"

# Classifica o modo de falha de um run a partir de eff, alvo e loss_pct.
# loss_pct "n/a" (baseline) conta como 0 => baseline só satura, nunca "perde".
classify_failure() {
  awk -v eff="$1" -v tgt="$2" -v lp="$3" -v sr="$SAT_RATIO" -v lt="$LOSS_TOL" 'BEGIN{
    loss = (lp=="n/a" ? 0 : lp+0);
    if (loss > lt) print "loss";
    else if (eff+0 < tgt*sr) print "saturation";
    else print "ok";
  }'
}

# Extrai um campo numérico da linha de métricas do emulador.
emu_field() { sed -n "s/.*$1=\([0-9.]*\).*/\1/p" "$2" | tail -1; }

# Lê a coluna avg/peak de uma métrica do metrics.summary do sampler.
# [-0-9.] tolera valores negativos (artefato de CPU per-processo já clampado no
# sampler, mas mantemos o regex robusto).
samp_avg()  { sed -n "s/^$1,\([-0-9.]*\),[-0-9.]*/\1/p"  "$2" | tail -1; }
samp_peak() { sed -n "s/^$1,[-0-9.]*,\([-0-9.]*\)/\1/p"  "$2" | tail -1; }

# Executa o emulador (liso) para um ramp, gravando o stderr em $2 e a latência
# por evento em $3 (--lat-out), com seq deslocado em $4 (--seq-base).
emu_run() { "$BIN" --dest "$DEST" --size "$SIZE" --rate "$RATE" --ramp "$1" --conn "$CONN" --duration "$DURATION" --lat-out "$3" --seq-base "$4" 2> "$2"; }

run_one() {
  local cond=$1 ramp=$2 rep=$3
  local rd="$OUT/$cond/ramp-$ramp/rep-$rep"
  mkdir -p "$rd"
  local emulog="$rd/emu.log" target
  local latout="$rd/lat.jsonl" metrics="$rd/metrics.csv" metsum="$rd/metrics.summary"
  local base=$(( SEQ_ORIGIN + RUN_IDX * SEQ_STRIDE )); RUN_IDX=$(( RUN_IDX + 1 ))
  target=$(awk -v r="$RATE" -v m="$ramp" 'BEGIN{printf "%g", r*m}')
  echo ">> [$cond] ramp=${ramp}x rep=${rep}/${REPLICAS} (alvo ${target}/s, ${DURATION}, seq_base=${base})" >&2

  # Sobe o sampler de overhead em background; encerra (SIGINT) ao fim do run.
  python3 "$EXP_DIR/sampler.py" --out "$metrics" --interval "$INTERVAL" \
    --match cs-emu --match strace --match cs-ebpf --summary "$metsum" 2> "$rd/sampler.err" &
  local samp=$!

  case "$cond" in
    baseline)
      emu_run "$ramp" "$emulog" "$latout" "$base"
      ;;
    strace)
      # strace É a ferramenta sob teste; -xx/-s grande p/ não truncar; -f p/ threads Go.
      strace -f -xx -s 262144 -e trace=write,sendto -o "$rd/strace_raw.txt" \
        "$BIN" --dest "$DEST" --size "$SIZE" --rate "$RATE" --ramp "$ramp" \
        --conn "$CONN" --duration "$DURATION" --lat-out "$latout" --seq-base "$base" 2> "$emulog"
      python3 "$EXP_DIR/strace/parse_strace.py" "$rd/strace_raw.txt" > "$rd/capture.jsonl" 2> "$rd/parse.err"
      # strace_raw.txt é DESCARTÁVEL pós-parse e ENORME em rampa alta (~130MB/run a
      # 2048×) — apagar já, senão enche o disco numa matriz n=10 (ver incidente
      # 2026-06-25). KEEP_RAW=1 preserva p/ depuração.
      [ -n "${KEEP_RAW:-}" ] || rm -f "$rd/strace_raw.txt"
      ;;
    ebpf-kprobe|ebpf-fentry)
      # eBPF (libbpf-CO-RE): captura anexada ao caminho de write, em PARALELO ao
      # emu. Grava capture.jsonl direto (mesmo schema do parse_strace). A falha do
      # eBPF é por PERDA (ringbuf cheio) — registrada em ebpf.log (dropped) e
      # detectada pelo compare (seqs ausentes => loss).
      if [ ! -x "$EBPF_BIN" ]; then
        echo "   ERRO: $EBPF_BIN ausente. Compile como usuário: (cd $REPO_ROOT/experiment/ebpf && go build -o $EBPF_BIN .)" >&2
        kill -INT "$samp" 2>/dev/null; wait "$samp" 2>/dev/null
        return
      fi
      local mode=${cond#ebpf-}
      "$EBPF_BIN" --attach "$mode" --out "$rd/capture.jsonl" 2> "$rd/ebpf.log" &
      local cap=$!
      sleep 1 # deixa o BPF anexar antes do emu disparar
      emu_run "$ramp" "$emulog" "$latout" "$base"
      sleep 0.3 # drena o ringbuf pendente
      kill -INT "$cap" 2>/dev/null; wait "$cap" 2>/dev/null
      ;;
    *)
      kill -INT "$samp" 2>/dev/null; wait "$samp" 2>/dev/null
      echo "   condição '$cond' ainda não implementada (precisa instalar/scaffold) — pulando" >&2
      return
      ;;
  esac

  kill -INT "$samp" 2>/dev/null; wait "$samp" 2>/dev/null

  local sent errs eff
  sent=$(emu_field enviados "$emulog"); errs=$(emu_field erros "$emulog"); eff=$(emu_field taxa_efetiva "$emulog")
  sent=${sent:-0}; errs=${errs:-0}; eff=${eff:-0}

  # Overhead do host (do sampler) e latência de syscall (do lat.jsonl).
  local cpu_avg cpu_peak tool_peak emu_rss tool_comm=""
  case "$cond" in strace) tool_comm=strace ;; ebpf-*) tool_comm=cs-ebpf ;; esac
  cpu_avg=$(samp_avg cpu_sys_pct "$metsum"); cpu_peak=$(samp_peak cpu_sys_pct "$metsum")
  emu_rss=$(samp_peak cs-emu_rss_kb "$metsum")
  tool_peak=n/a
  [ -n "$tool_comm" ] && tool_peak=$(samp_peak "${tool_comm}_cpu_pct" "$metsum")
  cpu_avg=${cpu_avg:-n/a}; cpu_peak=${cpu_peak:-n/a}; tool_peak=${tool_peak:-n/a}; emu_rss=${emu_rss:-n/a}
  local lat_p50 lat_p95 lat_p99 lat_max latrow
  latrow=$(python3 "$EXP_DIR/latency.py" --lat "$latout" --csv-row "$cond" 2>> "$rd/parse.err")
  IFS=',' read -r _l lat_p50 lat_p95 lat_p99 lat_max _e1 _e2 <<< "$latrow"
  lat_p50=${lat_p50:-n/a}; lat_p95=${lat_p95:-n/a}; lat_p99=${lat_p99:-n/a}; lat_max=${lat_max:-n/a}
  local over="$cpu_avg,$cpu_peak,$tool_peak,$emu_rss,$lat_p50,$lat_p95,$lat_p99,$lat_max"

  local fmode
  if [ "$cond" = "baseline" ]; then
    # Sem captura: só pode saturar (loss_pct=n/a => tratado como 0).
    fmode=$(classify_failure "$eff" "$target" "n/a")
    local res="OK"; [ "$errs" -gt 0 ] && res="SATUROU"
    echo "$cond,$rep,$RATE,$ramp,$target,$DURATION,$sent,$errs,$eff,n/a,n/a,n/a,n/a,n/a,n/a,$over,$fmode,$res" >> "$SUMMARY"
  else
    python3 "$EXP_DIR/truth_from_emu.py" "$emulog" > "$rd/truth.jsonl" 2>> "$rd/parse.err"
    # compare --csv-row imprime: label,n_truth,captured,missing,loss_pct,extra,mode,bad,result
    local row
    row=$(python3 "$EXP_DIR/compare.py" --truth "$rd/truth.jsonl" --capture "$rd/capture.jsonl" --csv-row "$cond" 2>> "$rd/parse.err")
    IFS=',' read -r _lab ntruth captured missing losspct extra mode bad result <<< "$row"
    fmode=$(classify_failure "$eff" "$target" "$losspct")
    echo "$cond,$rep,$RATE,$ramp,$target,$DURATION,$sent,$errs,$eff,$captured,$missing,$losspct,$extra,$mode,$bad,$over,$fmode,$result" >> "$SUMMARY"
  fi
}

echo ">> matriz: condições=[$CONDITIONS] ramps=[$RAMPS] réplicas=$REPLICAS rate=$RATE conn=$CONN dur=$DURATION dest=$DEST" >&2
for cond in $CONDITIONS; do
  for ramp in $RAMPS; do
    for rep in $(seq 1 "$REPLICAS"); do
      run_one "$cond" "$ramp" "$rep"
    done
  done
done

echo ">> CONCLUÍDO. Resultados em $OUT" >&2
echo "" >&2
column -t -s, "$SUMMARY" >&2
