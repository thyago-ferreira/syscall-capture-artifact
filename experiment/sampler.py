#!/usr/bin/env python3
"""sampler.py — amostra CPU e memória do host durante um run (lado ORIGEM).

Mede o OVERHEAD que a ferramenta de captura impõe ao host do CS. Amostra, a cada
--interval segundos, lendo só /proc (sem dependências):

  - CPU do SISTEMA inteiro (/proc/stat): pega TUDO — o cs-emu, o tracer separado
    (strace), e o trabalho de kernel/softirq que a ferramenta induz. É a métrica
    de overhead que importa: ptrace, p.ex., gasta CPU no processo strace, não no
    emu.
  - Por GRUPO de processo (--match NOME): CPU% (utime+stime de /proc/<pid>/stat,
    somando todos os pids/threads cujo comm casa) e RSS (VmRSS de
    /proc/<pid>/status). Permite separar "quanto foi do emu" de "quanto foi da
    ferramenta".

Escreve uma série temporal CSV em --out e, ao receber SIGINT/SIGTERM (o harness
manda ao fim do run), imprime no stderr um resumo (média e pico) — também grava
o resumo como CSV em <out>.summary se --summary for dado.

Uso:
  sampler.py --out metrics.csv --interval 0.2 --match cs-emu --match strace
  (rode em background; mande SIGINT/SIGTERM para encerrar e resumir)
"""
import argparse
import os
import signal
import sys
import time

CLK_TCK = os.sysconf("SC_CLK_TCK")  # jiffies por segundo (tipicamente 100)


def read_system_jiffies():
    """Total e ocioso de jiffies do sistema (primeira linha de /proc/stat)."""
    with open("/proc/stat") as f:
        parts = f.readline().split()
    vals = [int(x) for x in parts[1:]]
    idle = vals[3] + (vals[4] if len(vals) > 4 else 0)  # idle + iowait
    return sum(vals), idle


def iter_pids():
    for name in os.listdir("/proc"):
        if name.isdigit():
            yield name


def proc_comm(pid):
    try:
        with open(f"/proc/{pid}/comm") as f:
            return f.read().strip()
    except OSError:
        return None


def proc_cpu_jiffies(pid):
    """utime+stime (campos 14,15 de /proc/<pid>/stat), robusto a espaços no comm."""
    try:
        with open(f"/proc/{pid}/stat") as f:
            data = f.read()
    except OSError:
        return 0
    rp = data.rfind(")")  # comm vem entre parênteses; pula até depois dele
    fields = data[rp + 2:].split()
    # após o ')', o 1º campo é 'state' (índice 0) => utime=idx 11, stime=idx 12
    return int(fields[11]) + int(fields[12])


def proc_rss_kb(pid):
    try:
        with open(f"/proc/{pid}/status") as f:
            for line in f:
                if line.startswith("VmRSS:"):
                    return int(line.split()[1])
    except OSError:
        pass
    return 0


def match_pids(patterns):
    """Mapeia cada padrão -> lista de pids cujo comm o contém."""
    groups = {p: [] for p in patterns}
    for pid in iter_pids():
        comm = proc_comm(pid)
        if not comm:
            continue
        for p in patterns:
            if p in comm:
                groups[p].append(pid)
    return groups


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True, help="CSV da série temporal")
    ap.add_argument("--interval", type=float, default=0.2, help="intervalo de amostragem (s)")
    ap.add_argument("--match", action="append", default=[], help="padrão de comm a agrupar (repetível)")
    ap.add_argument("--summary", help="se dado, grava o resumo (média/pico) neste CSV")
    args = ap.parse_args()

    patterns = args.match
    samples = []  # (t, cpu_sys_pct, {pat: (cpu_pct, rss_kb)})

    running = {"v": True}

    def stop(_sig, _frm):
        running["v"] = False

    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)

    out = open(args.out, "w")
    header = ["epoch", "cpu_sys_pct"]
    for p in patterns:
        header += [f"{p}_cpu_pct", f"{p}_rss_kb"]
    out.write(",".join(header) + "\n")

    prev_total, prev_idle = read_system_jiffies()
    prev_proc = {p: 0 for p in patterns}
    # primeira leitura de jiffies por grupo (baseline para o delta)
    groups = match_pids(patterns)
    for p in patterns:
        prev_proc[p] = sum(proc_cpu_jiffies(pid) for pid in groups[p])
    time.sleep(args.interval)

    while running["v"]:
        t = time.time()
        total, idle = read_system_jiffies()
        dt_total = total - prev_total
        cpu_sys = 100.0 * (1 - (idle - prev_idle) / dt_total) if dt_total > 0 else 0.0
        prev_total, prev_idle = total, idle

        groups = match_pids(patterns)
        row = [f"{t:.3f}", f"{cpu_sys:.1f}"]
        per = {}
        for p in patterns:
            jif = sum(proc_cpu_jiffies(pid) for pid in groups[p])
            rss = sum(proc_rss_kb(pid) for pid in groups[p])
            # CPU% do grupo = delta_jiffies / delta_total_jiffies * ncpu * 100.
            # Clampa em >=0: sob strace -f os pids/threads entram e saem entre
            # amostras, e a soma de jiffies pode CAIR (thread saiu) => delta
            # negativo espúrio. Processo não usa CPU negativa.
            cpu_p = max(0.0, 100.0 * (jif - prev_proc[p]) / dt_total) if dt_total > 0 else 0.0
            prev_proc[p] = jif
            per[p] = (cpu_p, rss)
            row += [f"{cpu_p:.1f}", str(rss)]
        out.write(",".join(row) + "\n")
        out.flush()
        samples.append((t, cpu_sys, per))
        time.sleep(args.interval)

    out.close()
    summarize(samples, patterns, args.summary)


def summarize(samples, patterns, summary_path):
    if not samples:
        sys.stderr.write("sampler: nenhuma amostra coletada\n")
        return
    cpu_sys = [s[1] for s in samples]
    avg = lambda xs: sum(xs) / len(xs)
    lines = [f"cpu_sys: média={avg(cpu_sys):.1f}% pico={max(cpu_sys):.1f}% (n={len(samples)})"]
    cols = {"metric": [], "avg": [], "peak": []}
    cols["metric"].append("cpu_sys_pct"); cols["avg"].append(avg(cpu_sys)); cols["peak"].append(max(cpu_sys))
    for p in patterns:
        cpu = [s[2][p][0] for s in samples]
        rss = [s[2][p][1] for s in samples]
        lines.append(f"{p}: cpu média={avg(cpu):.1f}% pico={max(cpu):.1f}% | rss pico={max(rss)/1024:.1f}MB")
        cols["metric"] += [f"{p}_cpu_pct", f"{p}_rss_kb"]
        cols["avg"] += [avg(cpu), avg(rss)]
        cols["peak"] += [max(cpu), max(rss)]
    sys.stderr.write("sampler resumo:\n  " + "\n  ".join(lines) + "\n")
    if summary_path:
        with open(summary_path, "w") as f:
            f.write("metric,avg,peak\n")
            for m, a, pk in zip(cols["metric"], cols["avg"], cols["peak"]):
                f.write(f"{m},{a:.2f},{pk:.2f}\n")


if __name__ == "__main__":
    main()
