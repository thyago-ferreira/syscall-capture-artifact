#!/usr/bin/env python3
"""latency.py — percentis de latência de envio (origem) e fim-a-fim (2 máquinas).

Duas fontes:
  lat.jsonl      : do cs-emu (--lat-out). Por evento: seq, send_unixnano,
                   send_dur_ns (duração do write na ORIGEM — overhead da captura,
                   relógio único, sem skew).
  received.jsonl : do cs-receiver (opcional). Por evento: seq, recv_unixnano.

Métricas:
  - SYSCALL (write): percentis de send_dur_ns — sempre disponível com lat.jsonl.
  - FIM-A-FIM: recv_unixnano − send_unixnano, casado por seq. Mede latência
    percebida MAS mistura rede e EXIGE relógios sincronizados (NTP) entre origem
    e destino; sem isso o número é inválido (pode até dar negativo). Só é
    calculada se --received for dado.

Uso:
  latency.py --lat lat.jsonl [--received received.jsonl] [--csv-row LABEL]
"""
import argparse
import json
import sys


def load_jsonl(path):
    rows = []
    for line in open(path):
        line = line.strip()
        if line:
            rows.append(json.loads(line))
    return rows


def pct(sorted_vals, p):
    if not sorted_vals:
        return 0.0
    idx = int(p * (len(sorted_vals) - 1))
    return sorted_vals[idx]


def stats(vals):
    s = sorted(vals)
    return {
        "n": len(s),
        "p50": pct(s, 0.50),
        "p90": pct(s, 0.90),
        "p95": pct(s, 0.95),
        "p99": pct(s, 0.99),
        "max": s[-1] if s else 0,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--lat", required=True, help="lat.jsonl do cs-emu (--lat-out)")
    ap.add_argument("--received", help="received.jsonl do cs-receiver (p/ fim-a-fim)")
    ap.add_argument("--csv-row", help="imprime UMA linha CSV com esse rótulo")
    args = ap.parse_args()

    lat = load_jsonl(args.lat)
    syscall_us = [r["send_dur_ns"] / 1000.0 for r in lat]  # ns -> µs
    sc = stats(syscall_us)

    e2e_ms = []
    e2e = None
    skew_warn = ""
    if args.received:
        send_by_seq = {r["seq"]: r["send_unixnano"] for r in lat}
        for r in load_jsonl(args.received):
            seq = r.get("seq")
            if seq in send_by_seq:
                e2e_ms.append((r["recv_unixnano"] - send_by_seq[seq]) / 1e6)  # ns -> ms
        if e2e_ms:
            e2e = stats(e2e_ms)
            if min(e2e_ms) < 0:
                skew_warn = "  [ATENÇÃO: latência negativa => relógios NÃO sincronizados (NTP)]"

    if args.csv_row is not None:
        # label,sc_p50_us,sc_p95_us,sc_p99_us,sc_max_us,e2e_p50_ms,e2e_p99_ms
        # C3 do MCDA usa p95; mantemos p50/p99/max p/ contexto.
        e = e2e or {"p50": "n/a", "p99": "n/a"}
        print("%s,%.1f,%.1f,%.1f,%.1f,%s,%s" % (
            args.csv_row, sc["p50"], sc["p95"], sc["p99"], sc["max"],
            f"{e['p50']:.2f}" if e2e else "n/a",
            f"{e['p99']:.2f}" if e2e else "n/a"))
        return

    print(f"latência de envio — SYSCALL write (origem, n={sc['n']}):")
    print(f"  p50={sc['p50']:.1f}µs p90={sc['p90']:.1f}µs p95={sc['p95']:.1f}µs p99={sc['p99']:.1f}µs max={sc['max']:.1f}µs")
    if e2e:
        print(f"latência FIM-A-FIM origem→destino (n={e2e['n']}):{skew_warn}")
        print(f"  p50={e2e['p50']:.2f}ms p90={e2e['p90']:.2f}ms p99={e2e['p99']:.2f}ms max={e2e['max']:.2f}ms")
    elif args.received:
        print("fim-a-fim: nenhum seq casou entre lat.jsonl e received.jsonl")


if __name__ == "__main__":
    main()
