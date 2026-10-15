#!/usr/bin/env python3
"""parse_strace.py — converte o log bruto do strace em capture.jsonl.

Lê a saída de `strace -f -xx -s ... -e trace=write,sendto` e, para cada syscall
cujo buffer começa com a request line do evento (POST /events/vehicles), reconstrói
os bytes EXATOS escritos, separa o envelope HTTP do corpo e emite uma linha jsonl:

    {"seq", "content_length", "sha256", "syscall", "ret"}

onde sha256/content_length são do CORPO (mesma base que o cs-receiver grava), de
modo que a comparação por seq seja 1:1 entre captura (origem) e recepção (destino).

Cada evento sai do emulador em UM único write/sendto (envelope+corpo juntos), logo
um syscall casado == um evento. Linhas que não casam o prefixo (ex.: writes de log
em stderr) são ignoradas.

Limitações registradas:
  - Truncamento: se o strace truncar o buffer (marca '"'+'...'), o byte exato se
    perde; abortamos com erro pedindo -s maior (capture.sh já usa -s 262144).
  - unfinished/resumed: sob -f, um syscall pode aparecer partido. O buffer vem na
    linha '<unfinished ...>' (capturada normalmente); a linha 'resumed' só traz o
    retorno e é ignorada. Contamos um WARN para visibilidade.
"""
import hashlib
import json
import re
import sys

# Prefixo que identifica um write de evento (a request line do perfil calibrado).
EVENT_PREFIX = b"POST /events/vehicles"
HEADER_SEP = b"\r\n\r\n"

# Primeiro literal de string do syscall, todo em \xNN (write/sendto: 2º argumento).
BLOB_RE = re.compile(r'"((?:\\x[0-9a-fA-F]{2})+)"')
# Marca de truncamento do strace: a string fecha e vem '...'.
TRUNC_RE = re.compile(r'"\.\.\.')
SYSCALL_RE = re.compile(r'\b(write|sendto)\(')
RET_RE = re.compile(r'=\s*(-?\d+)')
CLEN_RE = re.compile(rb'Content-Length:\s*(\d+)', re.IGNORECASE)


def decode_blob(hexblob: str) -> bytes:
    # hexblob é "\\x50\\x4f..." -> bytes exatos.
    return bytes(int(h, 16) for h in re.findall(r'\\x([0-9a-fA-F]{2})', hexblob))


def parse(path):
    records = []
    warn_truncated = 0
    warn_resumed = 0
    for line in open(path, "r", errors="replace"):
        if "resumed>" in line:
            warn_resumed += 1
            continue
        m_sys = SYSCALL_RE.search(line)
        if not m_sys:
            continue
        m_blob = BLOB_RE.search(line)
        if not m_blob:
            continue
        buf = decode_blob(m_blob.group(1))
        if not buf.startswith(EVENT_PREFIX):
            continue  # write de log/stderr ou outro syscall não-evento
        if TRUNC_RE.search(line):
            warn_truncated += 1
            sys.stderr.write(
                "ERRO: buffer truncado pelo strace — aumente -s (capture.sh usa -s 262144)\n"
            )
            sys.exit(2)

        sep = buf.find(HEADER_SEP)
        header = buf[:sep] if sep >= 0 else b""
        body = buf[sep + len(HEADER_SEP):] if sep >= 0 else buf
        seq = extract_seq(body)
        m_ret = RET_RE.search(line)
        m_clen = CLEN_RE.search(header)
        # declared_len é o Content-Length anunciado no envelope; a checagem de
        # integridade LOCAL exige declared_len == len(body) capturado (um write
        # truncado/partido pela ferramenta quebraria essa igualdade).
        records.append({
            "seq": seq,
            "content_length": len(body),
            "declared_len": int(m_clen.group(1)) if m_clen else None,
            "sha256": hashlib.sha256(body).hexdigest(),
            "syscall": m_sys.group(1),
            "ret": int(m_ret.group(1)) if m_ret else None,
        })
    if warn_resumed:
        sys.stderr.write(f"WARN: {warn_resumed} linha(s) resumed> ignoradas (syscalls partidos sob -f)\n")
    return records


def extract_seq(body: bytes):
    # episode é o seq do emulador. Decodifica o corpo como array JSON.
    try:
        arr = json.loads(body)
        if isinstance(arr, list) and arr:
            return arr[0].get("episode")
    except Exception:
        pass
    return None


def main():
    if len(sys.argv) != 2:
        sys.stderr.write("uso: parse_strace.py <strace_raw.txt>  > capture.jsonl\n")
        sys.exit(1)
    for r in parse(sys.argv[1]):
        print(json.dumps(r))


if __name__ == "__main__":
    main()
