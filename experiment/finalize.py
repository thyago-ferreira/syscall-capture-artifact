#!/usr/bin/env python3
"""finalize.py — pós-processa a matriz com o received.jsonl do destino.

O run-matrix.sh roda na ORIGEM e mede, ao vivo, perda (auto vs truth local),
CPU/mem e latência de syscall. O que NÃO dá para medir na origem durante o run é
a INTEGRIDADE REAL (cross-sha256: corpo capturado vs corpo recebido) e a latência
FIM-A-FIM — ambas precisam do received.jsonl, que vive na máquina DESTINO.

Fluxo:
  1. Ao fim da matriz, traga o ground truth do destino para a origem, p.ex.:
       scp destino:/caminho/received.jsonl  results/<ts>/received.jsonl
  2. Rode:  finalize.py --out results/<ts> --received results/<ts>/received.jsonl

Para cada run (OUT/<cond>/ramp-<n>/), usando o seq_base + contagem do emu.log,
fatia o received.jsonl na FAIXA daquele run (os seqs são globalmente únicos por
--seq-base, então a fatia é exata e sem colisão entre rampas) e calcula:

  - integridade CROSS-SHA256: corpo capturado (capture.jsonl) vs recebido, por seq.
  - perda REAL na entrega: seqs escritos pelo emu que NÃO chegaram ao destino.
  - latência FIM-A-FIM: recv_unixnano (destino) − send_unixnano (origem), por seq.
    EXIGE relógios sincronizados (NTP); avisa se der negativa.

Emite summary-final.csv em OUT, juntando essas colunas às do run.
"""
import argparse
import json
import os
import re
import sys

METRICS_RE = re.compile(r'enviados=(\d+)\s+erros=(\d+)')
BASE_RE = re.compile(r'seq_base=(\d+)')
ERRSEQ_RE = re.compile(r'erro no envio \(seq=(\d+)\)')


def parse_emulog(path):
    sent = errs = None
    base = 0
    err_seqs = set()
    for line in open(path, errors="replace"):
        m = METRICS_RE.search(line)
        if m:
            sent, errs = int(m.group(1)), int(m.group(2))
        m = BASE_RE.search(line)
        if m:
            base = int(m.group(1))
        m = ERRSEQ_RE.search(line)
        if m:
            err_seqs.add(int(m.group(1)))
    if sent is None:
        return None
    return {"sent": sent, "errs": errs, "base": base, "err_seqs": err_seqs,
            "count": sent + errs}


def load_jsonl(path):
    rows = []
    if not os.path.exists(path):
        return rows
    for line in open(path):
        line = line.strip()
        if line:
            rows.append(json.loads(line))
    return rows


def pct(vals, p):
    if not vals:
        return 0.0
    s = sorted(vals)
    return s[int(p * (len(s) - 1))]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True, help="dir da matriz (results/<ts>)")
    ap.add_argument("--received", required=True, help="received.jsonl trazido do destino")
    ap.add_argument("--out-csv", help="caminho do summary-final.csv (default: <out>/summary-final.csv; útil se <out> for read-only)")
    args = ap.parse_args()

    # Indexa o received.jsonl por seq (seqs globalmente únicos => sem colisão).
    recv_by_seq = {}
    for r in load_jsonl(args.received):
        seq = r.get("seq")
        if seq is not None:
            recv_by_seq[seq] = r
    if not recv_by_seq:
        sys.stderr.write(f"ERRO: {args.received} vazio ou sem seqs\n")
        sys.exit(2)

    # Cada RUN é um dir folha com emu.log. Estrutura: cond/ramp-N/[rep-K/]. Anda
    # recursivamente e processa todo dir que contenha emu.log (cobre com e sem
    # réplicas). Membership por seq (recv_by_seq é dict) evita varrer o received
    # inteiro por run.
    rows = []
    for dirpath, _dirs, files in os.walk(args.out):
        if "emu.log" not in files:
            continue
        meta = parse_emulog(os.path.join(dirpath, "emu.log"))
        if meta is None:
            continue
        rel = os.path.relpath(dirpath, args.out).split(os.sep)
        cond = rel[0]
        ramp = next((p.split("-")[-1] for p in rel if p.startswith("ramp-")), "?")
        rep = next((p.split("-")[-1] for p in rel if p.startswith("rep-")), "1")
        lo, hi = meta["base"] + 1, meta["base"] + meta["count"]

        written = {k for k in range(lo, hi + 1) if k not in meta["err_seqs"]}
        arrived_n = sum(1 for s in written if s in recv_by_seq)  # chegaram ao destino
        delivered_loss = len(written) - arrived_n

        cap = {r["seq"]: r for r in load_jsonl(os.path.join(dirpath, "capture.jsonl"))
               if r.get("seq") is not None}

        integ_mode, integ_bad, cap_loss = "n/a", "n/a", "n/a"
        if cap:
            cap_loss = len(written - set(cap))
            # cross-sha256: corpo capturado vs recebido, nos seqs em comum.
            bad = sum(1 for s in cap if s in recv_by_seq
                      and cap[s].get("sha256") != recv_by_seq[s].get("sha256"))
            integ_mode, integ_bad = "cross-sha256", str(bad)

        # latência fim-a-fim: recv − send, casado por seq via lat.jsonl.
        e2e = []
        for r in load_jsonl(os.path.join(dirpath, "lat.jsonl")):
            s = r.get("seq")
            if s in recv_by_seq:
                e2e.append((recv_by_seq[s]["recv_unixnano"] - r["send_unixnano"]) / 1e6)
        if e2e:
            e2e_p50 = f"{pct(e2e, 0.50):.2f}"
            e2e_p99 = f"{pct(e2e, 0.99):.2f}"
            e2e_neg = "sim" if min(e2e) < 0 else "nao"
        else:
            e2e_p50 = e2e_p99 = e2e_neg = "n/a"

        rows.append([cond, ramp, rep, len(written), arrived_n, delivered_loss,
                     cap_loss, integ_mode, integ_bad, e2e_p50, e2e_p99, e2e_neg])

    rows.sort(key=lambda r: (r[0], int(r[1]) if r[1].isdigit() else 0,
                             int(r[2]) if r[2].isdigit() else 0))
    header = ["condition", "ramp", "rep", "written", "arrived", "delivered_loss",
              "capture_loss", "integrity_mode", "integrity_bad",
              "e2e_p50_ms", "e2e_p99_ms", "e2e_clock_skew"]
    out_csv = args.out_csv or os.path.join(args.out, "summary-final.csv")
    with open(out_csv, "w") as f:
        f.write(",".join(header) + "\n")
        for r in rows:
            f.write(",".join(str(x) for x in r) + "\n")
    sys.stderr.write(f">> summary-final.csv escrito em {out_csv} ({len(rows)} runs)\n")
    # Eco legível.
    widths = [max(len(str(r[i])) for r in ([header] + rows)) for i in range(len(header))]
    for r in [header] + rows:
        sys.stderr.write("  " + "  ".join(str(c).ljust(widths[i]) for i, c in enumerate(r)) + "\n")


if __name__ == "__main__":
    main()
