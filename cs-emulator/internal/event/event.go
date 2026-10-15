// Package event gera o CORPO JSON de um evento do CS emulado.
//
// Invariantes:
//   - O payload deve sair ÍNTEGRO em um único write/sendto. Aqui produzimos
//     apenas o corpo; o envelope HTTP e a integridade do envio são do sender.
//   - Sem alocação por evento no hot path: o Builder reusa um único buffer e
//     pré-constrói o filler uma vez, em New.
//   - O tamanho-alvo do corpo vem por parâmetro (--size), coerente com o
//     Content-Length calculado pelo sender.
//
// Forma do corpo (perfil calibrado do CS de referência, /events/vehicles): array JSON de
// objetos de reconhecimento de placa. Campos do perfil: license_plate_thumbnail
// (URL), episode, matched_object, matched_card e demais matched_*. Todos os
// VALORES são SINTÉTICOS — nunca IP/host/rota reais (usamos 127.0.0.1 e host
// genérico). O padding mora num campo string (_pad) para manter o JSON válido.
package event

import "strconv"

// Builder constrói o corpo de eventos reusando o mesmo buffer subjacente.
//
// NÃO é seguro para uso concorrente: cada goroutine de disparo deve ter o seu
// próprio Builder. O slice devolvido por Next é válido apenas até a próxima
// chamada de Next no mesmo Builder.
type Builder struct {
	target int    // tamanho-alvo do corpo em bytes (== Content-Length)
	buf    []byte // buffer reusado entre chamadas
	filler []byte // bytes de preenchimento pré-construídos
	seq    uint64 // contador de sequência, incrementado a cada evento
}

// fillerByte é o caractere usado para preencher o corpo até o tamanho-alvo.
const fillerByte = 'x'

// New cria um Builder que produz corpos de aproximadamente targetSize bytes.
//
// O filler é dimensionado uma única vez aqui. Se a parte estrutural do objeto
// já exceder targetSize, o corpo sairá maior que o alvo (sem filler) — o
// tamanho mínimo é o tamanho da estrutura.
func New(targetSize int) *Builder {
	b := &Builder{
		target: targetSize,
		buf:    make([]byte, 0, targetSize+512), // folga p/ evitar realocação
	}
	// Pré-aloca um filler do tamanho-alvo cheio; em Next usamos só o trecho
	// necessário (slice, sem alocar).
	b.filler = make([]byte, targetSize)
	for i := range b.filler {
		b.filler[i] = fillerByte
	}
	return b
}

// Next monta o próximo corpo e devolve o buffer interno.
//
// nowUnixNano é injetado pelo chamador (ex.: time.Now().UnixNano()) para manter
// este pacote livre de I/O de relógio no hot path e facilitar testes.
//
// O slice retornado aponta para o buffer interno e é sobrescrito na próxima
// chamada — o sender deve consumi-lo (write) antes de chamar Next de novo.
func (b *Builder) Next(nowUnixNano int64) []byte {
	b.seq++

	buf := b.buf[:0] // reusa a capacidade, zera o tamanho

	// Array com um objeto de reconhecimento de placa. Montado manualmente (sem
	// encoding/json) para evitar reflexão/alocação no hot path. VALORES
	// SINTÉTICOS apenas: URL em 127.0.0.1, placa/cartão/objeto fictícios.
	buf = append(buf, `[{"license_plate_thumbnail":"http://127.0.0.1/events/thumbnails/`...)
	buf = strconv.AppendUint(buf, b.seq, 10)
	buf = append(buf, `.jpg","episode":`...)
	buf = strconv.AppendUint(buf, b.seq, 10)
	buf = append(buf, `,"matched_object":{"id":`...)
	buf = strconv.AppendUint(buf, b.seq, 10)
	buf = append(buf, `,"name":"vehicle"},"matched_card":{"id":`...)
	buf = strconv.AppendUint(buf, b.seq, 10)
	buf = append(buf, `,"status":"active"},"matched_plate":"ABC1D23",`...)
	buf = append(buf, `"matched_confidence":0.987,"matched_at":`...)
	buf = strconv.AppendInt(buf, nowUnixNano, 10)
	buf = append(buf, `,"_pad":"`...)

	// Preenchimento até o tamanho-alvo. Reservamos 3 bytes para o fechamento
	// (`"}]`). Se não couber filler, segue sem (corpo mínimo).
	const tailLen = 3 // `"}]`
	pad := b.target - len(buf) - tailLen
	if pad > 0 {
		if pad > len(b.filler) {
			pad = len(b.filler)
		}
		buf = append(buf, b.filler[:pad]...)
	}

	buf = append(buf, '"', '}', ']')

	b.buf = buf
	return buf
}

// Seq devolve o número de sequência do último evento gerado.
func (b *Builder) Seq() uint64 { return b.seq }

// SetSeqBase desloca o contador de sequência: o primeiro evento gerado por Next
// passa a ter seq = base+1. Serve para tornar os seqs GLOBALMENTE únicos entre
// runs distintos (cada run recebe uma faixa disjunta), de modo que um
// received.jsonl append-only do cs-receiver não colida entre rampas — condição
// para o cross-sha256 e a latência fim-a-fim por run. Chamar ANTES do primeiro
// Next.
func (b *Builder) SetSeqBase(base uint64) { b.seq = base }
