#!/usr/bin/env bash
# capture.sh — captura via strace (ptrace) na MÁQUINA ORIGEM.
#
# Roda o cs-emu sob strace, registrando as syscalls write/sendto em um log bruto.
# O strace AQUI É A FERRAMENTA SOB TESTE: o que ele deixar de registrar conta
# como perda de captura; o que registrar errado conta como falha de integridade.
#
# Compila o binário antes e estraça o binário direto (não `go run`), para o log
# não vir poluído pela toolchain do Go.
#
# Uso:
#   ./capture.sh --dest <host:porta> [demais flags do cs-emu...]
# Saída: $RAW (default strace_raw.txt) — depois processado por parse_strace.py.
#
# Notas de fidelidade do strace:
#   -xx       : todo byte em \xNN (reversível; sem ambiguidade de escape).
#   -s 262144 : não truncar o corpo (~6,3 KB + envelope); folga grande p/ --size.
#   -f        : seguir as threads do runtime Go (os writes saem em qualquer uma).
#   -e trace=write,sendto : só os syscalls de saída do payload.
set -euo pipefail

RAW=${RAW:-strace_raw.txt}
BIN=${BIN:-/tmp/cs-emu.strace.bin}
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

if ! command -v strace >/dev/null 2>&1; then
  echo "ERRO: strace ausente. Instale na ORIGEM: sudo apt install strace" >&2
  exit 1
fi

echo ">> compilando cs-emu em $BIN" >&2
( cd "$REPO_ROOT/cs-emulator" && go build -o "$BIN" ./cmd/cs-emu )

echo ">> strace -> $RAW (ferramenta sob teste)" >&2
strace -f -xx -s 262144 -e trace=write,sendto -o "$RAW" "$BIN" "$@"

echo ">> captura bruta em $RAW; processe com: python3 parse_strace.py $RAW > capture.jsonl" >&2
