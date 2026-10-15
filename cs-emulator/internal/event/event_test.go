package event

import "testing"

// TestSize garante que o corpo gerado bate o tamanho-alvo (--size), que é o
// Content-Length anunciado pelo sender. O padding em _pad é dimensionado para
// fechar no alvo.
//
// Há um piso: a estrutura JSON mínima de um objeto de reconhecimento de placa
// (~263 B) não pode ser truncada sem virar JSON inválido. Para alvos acima do
// piso, len(body) == target exato; para alvos abaixo (ex.: 200), o corpo sai no
// mínimo estrutural — nunca menor que o alvo, mas sem encolher além do válido.
func TestSize(t *testing.T) {
	const nowUnixNano = 1700000000000000000

	// Piso estrutural: corpo gerado com alvo 0 (estrutura sem nenhum filler).
	structuralMin := len(New(0).Next(nowUnixNano))

	for _, target := range []int{6300, 6280, 6000, 200} {
		body := New(target).Next(nowUnixNano)
		switch {
		case target >= structuralMin:
			if len(body) != target {
				t.Errorf("target=%d (>= piso %d): len(body)=%d, esperado %d",
					target, structuralMin, len(body), target)
			}
		default:
			if len(body) != structuralMin {
				t.Errorf("target=%d (< piso %d): len(body)=%d, esperado o mínimo estrutural %d",
					target, structuralMin, len(body), structuralMin)
			}
			if len(body) < target {
				t.Errorf("target=%d: len(body)=%d menor que o alvo", target, len(body))
			}
		}
	}
}
