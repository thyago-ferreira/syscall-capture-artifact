#!/usr/bin/env python3
"""compare.py — mede PERDA e INTEGRIDADE de uma ferramenta de captura.

Cruza por seq um arquivo de VERDADE-BASE (truth) com a captura da ferramenta:

  truth.jsonl    : seqs que o emulador ESCREVEU com sucesso (derivado do stderr
                   do cs-emu pelo harness) ou received.jsonl do cs-receiver.
                   Se trouxer "sha256" por seq, a integridade é checada contra ele.
  capture.jsonl  : o que a ferramenta de captura registrou (parse_strace.py etc.).

Métricas:
  - PERDA de captura = seqs no truth que a captura NÃO registrou (truth − capture).
  - capturados a mais = capture − truth (espera-se 0).
  - INTEGRIDADE:
      * se o truth tem sha256 -> compara sha256 do corpo por seq (cross-check real);
      * senão -> AUTO-CHECK local: um registro capturado é íntegro se
        declared_len (Content-Length do envelope) == content_length (corpo
        efetivamente capturado) e o seq foi extraído. Pega truncamento/partição.

Uso:  compare.py --truth truth.jsonl --capture capture.jsonl [--csv-row LABEL]
Sai !=0 se houver perda, sobra ou divergência de integridade (scriptável).
"""
import argparse
import json
import sys


def load(path):
    by_seq, dup = {}, 0
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        r = json.loads(line)
        seq = r.get("seq")
        if seq in by_seq:
            dup += 1
        by_seq[seq] = r
    return by_seq, dup


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--truth", required=True, help="jsonl de verdade-base (seqs escritos; opc. sha256)")
    ap.add_argument("--capture", required=True, help="jsonl da captura da ferramenta")
    ap.add_argument("--csv-row", help="se dado, imprime UMA linha CSV com esse rótulo em vez do relatório")
    args = ap.parse_args()

    truth, dup_truth = load(args.truth)
    captured, dup_cap = load(args.capture)

    truth_seqs = {s for s in truth if s is not None}
    cap_seqs = {s for s in captured if s is not None}

    missing = sorted(truth_seqs - cap_seqs)
    extra = sorted(cap_seqs - truth_seqs)
    common = sorted(truth_seqs & cap_seqs)

    has_truth_hash = any(truth[s].get("sha256") for s in common)
    bad = []
    if has_truth_hash:
        mode = "cross-sha256"
        for s in common:
            if truth[s].get("sha256") != captured[s].get("sha256"):
                bad.append(s)
    else:
        mode = "auto-check"
        for s in common:
            r = captured[s]
            dl = r.get("declared_len")
            if dl is not None and dl != r.get("content_length"):
                bad.append(s)

    n_truth = len(truth_seqs)
    loss_pct = (len(missing) / n_truth * 100) if n_truth else 0.0
    ok = not missing and not extra and not bad

    if args.csv_row is not None:
        # label,truth,captured,perda,perda_pct,a_mais,integridade_modo,integridade_falhas,resultado
        print("%s,%d,%d,%d,%.2f,%d,%s,%d,%s" % (
            args.csv_row, n_truth, len(cap_seqs), len(missing), loss_pct,
            len(extra), mode, len(bad), "OK" if ok else "FALHA"))
        sys.exit(0 if ok else 1)

    print(f"verdade-base (escritos): {n_truth}  (dup no arquivo: {dup_truth})")
    print(f"capturados:              {len(cap_seqs)}  (dup no arquivo: {dup_cap})")
    print(f"em comum:                {len(common)}")
    print(f"PERDA de captura:        {len(missing)}  ({loss_pct:.2f}%)")
    print(f"capturados a mais:       {len(extra)}")
    print(f"integridade ({mode}):    {len(bad)} falha(s) de {len(common)} comparados")
    if missing:
        print(f"  seqs perdidos: {missing[:50]}{' ...' if len(missing) > 50 else ''}")
    if extra:
        print(f"  seqs a mais:   {extra[:50]}{' ...' if len(extra) > 50 else ''}")
    if bad:
        print(f"  seqs c/ integridade ruim: {bad[:50]}{' ...' if len(bad) > 50 else ''}")
    print("RESULTADO:", "OK (sem perda, íntegro)" if ok else "FALHAS detectadas")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
