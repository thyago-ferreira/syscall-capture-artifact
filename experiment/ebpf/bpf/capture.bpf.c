// capture.bpf.c — programa eBPF de captura do experimento de captura (libbpf-CO-RE).
//
// Hooka o caminho de write(2) — ksys_write(fd, buf, count) — e, para cada write
// cujo buffer começa com a request line do evento (POST /events/vehicles), COPIA O
// BUFFER INTEIRO (envelope+corpo) para um ringbuf. O user-space separa
// envelope/corpo, calcula o sha256 do corpo e grava capture.jsonl. A cópia
// integral é DELIBERADA (decisão de desenho): mede o custo realista de
// captura íntegra como uma integração a executaria, não o custo de hook em abstrato.
//
// Lê só pt_regs + buffer de usuário (bpf_probe_read_user) — NÃO acessa structs
// versionadas do kernel, então não exige relocs CO-RE => BTF-leve (preserva a
// independência de BTF do mecanismo kprobe; C5a). O mesmo objeto serve fentry,
// mudando só o tipo de attach (Fase C).
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

// Critério de identificação do evento (uniforme entre ferramentas, ver
// HOOKPOINT.md): o buffer do write começa com a request line do perfil.
#define PREFIX_LEN 21
static const char PREFIX[PREFIX_LEN] = "POST /events/vehicles";

// Piso de captura: cobre envelope (~130 B) + corpo. Default 8192 cobre o perfil
// real (corpo ~6300 => write ~6430). O sweep de tamanho (Eixo 2: até 64 KB) deve
// REBUILDAR com -DMAX_PAYLOAD=131072 (roda a 1/s, então a folga do reserve é
// irrelevante). NÃO inflar aqui: reserve grande encheria o ringbuf cedo demais e
// adiantaria a falsa falha por perda no experimento de rampa.
#ifndef MAX_PAYLOAD
#define MAX_PAYLOAD 8192
#endif

// Registro exfiltrado: comprimento copiado + os bytes (envelope+corpo).
struct evt {
	__u32 len;
	__u8 data[MAX_PAYLOAD];
};

// Ringbuf de saída. Escrevemos direto via reserve/submit (sem scratch per-CPU:
// valor per-CPU de 128KB estoura o alocador). Cheio sob carga alta => reserve
// falha => dropped (modo de falha 'loss' do eBPF; ver "Definição de FALHA").
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 16 * 1024 * 1024); // 16 MB
} events SEC(".maps");

// Contadores: índice 0 = matched (eventos vistos), 1 = dropped (ringbuf cheio).
#define ST_MATCHED 0
#define ST_DROPPED 1
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 2);
	__type(key, __u32);
	__type(value, __u64);
} stats SEC(".maps");

static __always_inline void bump(__u32 idx)
{
	__u64 *v = bpf_map_lookup_elem(&stats, &idx);
	if (v)
		__sync_fetch_and_add(v, 1);
}

static __always_inline int match_prefix(const char *buf)
{
	char pre[PREFIX_LEN];
	if (bpf_probe_read_user(pre, sizeof(pre), buf) != 0)
		return 0;
#pragma unroll
	for (int i = 0; i < PREFIX_LEN; i++)
		if (pre[i] != PREFIX[i])
			return 0;
	return 1;
}

// capture_write é o corpo comum: filtra o evento, copia o buffer e exfiltra.
// Recebe os args já desempacotados (serve kprobe e, na Fase C, fentry).
static __always_inline int capture_write(const char *buf, __u64 count)
{
	if (count < PREFIX_LEN)
		return 0;
	if (!match_prefix(buf))
		return 0;
	bump(ST_MATCHED);

	struct evt *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		bump(ST_DROPPED); // ringbuf cheio: evento perdido (loss)
		return 0;
	}

	__u64 n = count;
	if (n > MAX_PAYLOAD) // delimita o read p/ o verificador
		n = MAX_PAYLOAD;
	e->len = n;
	if (bpf_probe_read_user(e->data, n, buf) != 0) {
		bpf_ringbuf_discard(e, 0);
		return 0;
	}
	bpf_ringbuf_submit(e, 0);
	return 0;
}

// ksys_write(unsigned int fd, const char __user *buf, size_t count) — função
// interna do caminho de write(2). Args diretos, extraídos por BPF_KPROBE.
SEC("kprobe/ksys_write")
int BPF_KPROBE(on_ksys_write, unsigned int fd, const char *buf, size_t count)
{
	return capture_write(buf, count);
}

// Mesma função-alvo, ancoragem por TRAMPOLINE BPF (fentry). Args tipados via BTF
// (por isso fentry exige BTF — C5a). É a 2ª implementação do mecanismo eBPF; a
// comparação kprobe↔fentry isola breakpoint vs trampoline. O loader carrega só
// UM dos dois programas (conforme --attach), preservando a independência de BTF
// do kprobe quando em modo kprobe.
SEC("fentry/ksys_write")
int BPF_PROG(on_ksys_write_fentry, unsigned int fd, const char *buf, size_t count)
{
	return capture_write(buf, count);
}
