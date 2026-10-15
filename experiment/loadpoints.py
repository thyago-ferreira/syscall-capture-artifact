#!/usr/bin/env python3
"""loadpoints.py — C1/C2/C3 em DOIS pontos de carga, com média ± IC95 e Δ vs baseline.

Decisão de desenho: C1/C2/C3 empatam no ponto operacional (1×) — é o desenho;
o discriminador no ponto real é o C4 (headroom). Para C1/C2/C3 não serem inúteis,
reporta-se também um ponto de CARGA ALTA (perto da saturação), onde voltam a
discriminar. Este script extrai os dois pontos do summary.csv da rampa (que já mede
todos os indicadores em cada múltiplo) — NÃO roda nada novo.

Para cada condição != baseline:
  - ponto OPERACIONAL = rampa 1× (ou a menor presente).
  - ponto CARGA ALTA  = a MAIOR rampa ainda "ok" (headroom), por failure_mode.
Em cada ponto, agrega as réplicas (média ± IC95, t de Student) de:
  C1 = cpu_sys_avg (impacto no host), C2 = emu_rss_peak_kb, C3 = lat_p95_us,
e calcula Δ vs baseline (mesma rampa). Saída: loadpoints.csv + tabela legível.

Uso:  loadpoints.py --summary results/<ts>/summary.csv
"""
import argparse
import csv
import math
import statistics
import sys
from collections import defaultdict

# t crítico bicaudal 95% por graus de liberdade (df=n-1); >30 ~ 2.04, usa-se 1.96.
T95 = {1: 12.71, 2: 4.303, 3: 3.182, 4: 2.776, 5: 2.571, 6: 2.447, 7: 2.365,
       8: 2.306, 9: 2.262, 10: 2.228, 11: 2.201, 12: 2.179, 13: 2.160, 14: 2.145,
       15: 2.131, 16: 2.120, 18: 2.101, 20: 2.086, 25: 2.060, 30: 2.042}


def tcrit(n):
    df = n - 1
    if df < 1:
        return float("nan")
    if df in T95:
        return T95[df]
    keys = sorted(T95)
    for k in keys:           # menor df tabelado >= df (conservador)
        if k >= df:
            return T95[k]
    return 1.96


def agg(vals):
    """média, semi-largura do IC95 (t) e n, ignorando não-numéricos."""
    xs = []
    for v in vals:
        try:
            xs.append(float(v))
        except (TypeError, ValueError):
            pass
    if not xs:
        return None
    m = statistics.mean(xs)
    if len(xs) < 2:
        return (m, float("nan"), len(xs))
    sd = statistics.stdev(xs)
    half = tcrit(len(xs)) * sd / math.sqrt(len(xs))
    return (m, half, len(xs))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--summary", required=True, help="summary.csv da rampa (com réplicas)")
    ap.add_argument("--majority", type=float, default=0.5,
                    help="fração mín. de réplicas 'ok' p/ a rampa contar como ok (default 0.5)")
    args = ap.parse_args()

    rows = list(csv.DictReader(open(args.summary)))
    if not rows:
        sys.stderr.write("summary vazio\n")
        sys.exit(1)

    # cell[(cond,ramp)][campo] = [valores por réplica]
    cell = defaultdict(lambda: defaultdict(list))
    ramps_by_cond = defaultdict(set)
    for r in rows:
        cond, ramp = r["condition"], int(r["ramp"])
        ramps_by_cond[cond].add(ramp)
        for f in ("cpu_sys_avg", "emu_rss_peak_kb", "lat_p95_us", "eff_rate", "failure_mode"):
            cell[(cond, ramp)][f].append(r.get(f))

    def ok_ramp(cond, ramp):
        modes = cell[(cond, ramp)]["failure_mode"]
        if not modes:
            return False
        return sum(1 for m in modes if m == "ok") / len(modes) >= args.majority

    # baseline por rampa (p/ Δ).
    def base_mean(ramp, field):
        a = agg(cell[("baseline", ramp)][field]) if ("baseline", ramp) in cell else None
        return a[0] if a else None

    metrics = [("C1_cpu_sys_avg_pct", "cpu_sys_avg"),
               ("C2_rss_peak_kb", "emu_rss_peak_kb"),
               ("C3_lat_p95_us", "lat_p95_us")]

    out = []
    for cond in sorted(ramps_by_cond):
        if cond == "baseline":
            continue
        ramps = sorted(ramps_by_cond[cond])
        operacional = 1 if 1 in ramps else ramps[0]
        ok_ramps = [r for r in ramps if ok_ramp(cond, r)]
        carga_alta = max(ok_ramps) if ok_ramps else operacional
        for regime, ramp in (("operacional", operacional), ("carga_alta", carga_alta)):
            for label, field in metrics:
                a = agg(cell[(cond, ramp)][field])
                if a is None:
                    continue
                mean, half, n = a
                bm = base_mean(ramp, field)
                delta = (mean - bm) if bm is not None else float("nan")
                out.append({
                    "condition": cond, "regime": regime, "ramp": ramp,
                    "metric": label, "mean": mean, "ci95_half": half, "n": n,
                    "baseline_mean": bm if bm is not None else "n/a",
                    "delta_vs_baseline": delta,
                })

    # CSV
    import os
    out_csv = os.path.join(os.path.dirname(os.path.abspath(args.summary)), "loadpoints.csv")
    cols = ["condition", "regime", "ramp", "metric", "mean", "ci95_half", "n",
            "baseline_mean", "delta_vs_baseline"]
    with open(out_csv, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=cols)
        w.writeheader()
        for r in out:
            w.writerow(r)
    sys.stderr.write(f">> loadpoints.csv escrito em {out_csv}\n\n")

    # Tabela legível
    def fmt(x):
        return f"{x:.2f}" if isinstance(x, float) and not math.isnan(x) else str(x)
    hdr = ["condition", "regime", "ramp", "metric", "mean", "±ci95", "n", "Δ vs base"]
    line = lambda c: [c["condition"], c["regime"], str(c["ramp"]), c["metric"],
                      fmt(c["mean"]), fmt(c["ci95_half"]), str(c["n"]),
                      fmt(c["delta_vs_baseline"])]
    table = [hdr] + [line(c) for c in out]
    widths = [max(len(r[i]) for r in table) for i in range(len(hdr))]
    for r in table:
        sys.stderr.write("  " + "  ".join(r[i].ljust(widths[i]) for i in range(len(hdr))) + "\n")


if __name__ == "__main__":
    main()
