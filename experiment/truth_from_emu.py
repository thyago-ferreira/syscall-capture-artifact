#!/usr/bin/env python3
"""truth_from_emu.py — deriva a verdade-base (seqs escritos) do stderr do cs-emu.

Sem instrumentar o emulador: o cs-emu já loga, no stderr, a linha de métricas
(enviados=N erros=E) e uma linha por erro com o seq que falhou. Cada iteração do
loop consome exatamente um seq (Next faz seq++ e então envia), logo:

    maxseq = enviados + erros
    seqs ESCRITOS com sucesso = {1..maxseq} \ {seqs com erro}

Emite uma linha jsonl {"seq": k} por seq escrito — entra como truth no compare.py.

Uso:  truth_from_emu.py <emu.log>  > truth.jsonl
"""
import json
import re
import sys

METRICS_RE = re.compile(r'enviados=(\d+)\s+erros=(\d+)')
ERRSEQ_RE = re.compile(r'erro no envio \(seq=(\d+)\)')
BASE_RE = re.compile(r'seq_base=(\d+)')


def main():
    if len(sys.argv) != 2:
        sys.stderr.write("uso: truth_from_emu.py <emu.log> > truth.jsonl\n")
        sys.exit(1)
    sent = errs = None
    base = 0  # deslocamento do seq (--seq-base); 0 se o log não trouxer
    err_seqs = set()
    for line in open(sys.argv[1], errors="replace"):
        m = METRICS_RE.search(line)
        if m:
            sent, errs = int(m.group(1)), int(m.group(2))
        m = BASE_RE.search(line)
        if m:
            base = int(m.group(1))
        m = ERRSEQ_RE.search(line)
        if m:
            err_seqs.add(int(m.group(1)))  # seq absoluto (já inclui o base)
    if sent is None:
        sys.stderr.write("ERRO: linha de métricas (enviados=.. erros=..) não encontrada no log\n")
        sys.exit(2)
    count = sent + errs  # nº de eventos tentados => seqs base+1 .. base+count
    out = sys.stdout.write
    for k in range(base + 1, base + count + 1):
        if k in err_seqs:
            continue
        out(json.dumps({"seq": k}) + "\n")


if __name__ == "__main__":
    main()
