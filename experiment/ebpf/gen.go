// gen.go — diretiva de geração do binding Go a partir do programa BPF.
//
// bpf2go compila bpf/capture.bpf.c com clang (alvo bpfel) e gera o objeto BPF +
// o binding Go (capture_bpfel.go) com o objeto embutido. Headers: ./bpf p/ o
// vmlinux.h (gerado do BTF por bpftool) e /usr/include p/ os de libbpf
// (bpf_helpers.h etc., de libbpf-dev). Rodar: `go generate ./...` (precisa clang).
package main

// -D__TARGET_ARCH_x86: bpf_tracing.h (PT_REGS_PARM*) precisa da arquitetura-alvo
// p/ a ABI de registradores do kprobe. Host x86_64 => x86. Recalibrar se mudar.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -cflags "-O2 -g -Wall -Werror -D__TARGET_ARCH_x86" capture ./bpf/capture.bpf.c -- -I./bpf -I/usr/include
