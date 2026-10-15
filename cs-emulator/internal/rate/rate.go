// Package rate controla o ritmo de disparo de eventos para a rampa do
// experimento (taxa base, 2x, 4x, 8x… até a falha).
//
// O controle é por intervalo fixo entre eventos (1/taxa). Esta é a peça que
// modula a "taxa de eventos" — uma das três variáveis que o emulador reproduz.
// O emulador NÃO pode ser o gargalo: se o disparo de um evento atrasar, o
// limiter não tenta "recuperar" rajadas — ele simplesmente segue, e o atraso
// fica visível nas métricas próprias (responsabilidade do chamador/main).
package rate

import "time"

// Limiter espaça os disparos para sustentar uma taxa-alvo em eventos/segundo.
//
// NÃO é seguro para uso concorrente.
type Limiter struct {
	interval time.Duration // tempo entre eventos; 0 = sem limite (o mais rápido possível)
	next     time.Time     // instante do próximo disparo permitido
}

// New cria um Limiter para eventsPerSecond eventos/s.
//
// eventsPerSecond <= 0 significa "sem limitação": Wait retorna imediatamente,
// útil para medir o teto de throughput do próprio emulador.
func New(eventsPerSecond float64) *Limiter {
	l := &Limiter{}
	if eventsPerSecond > 0 {
		l.interval = time.Duration(float64(time.Second) / eventsPerSecond)
	}
	return l
}

// NewRamp cria um Limiter aplicando a rampa do experimento: a taxa efetiva é
// base * ramp, onde base é o ponto de operação real (req/s) e ramp é o
// multiplicador (1, 2, 4, 8, 16, 32…). A separação base × multiplicador é a
// forma como o experimento percorre a rampa até o ponto de falha das
// ferramentas, que só aparece bem acima do real.
//
// base ou ramp <= 0 resultam em taxa não-positiva => sem limitação.
func NewRamp(base, ramp float64) *Limiter {
	return New(base * ramp)
}

// Interval devolve o intervalo configurado entre eventos (0 = sem limite).
func (l *Limiter) Interval() time.Duration { return l.interval }

// Start ancora o relógio do limiter no instante atual. Deve ser chamado uma
// vez, imediatamente antes do loop de disparo.
func (l *Limiter) Start(now time.Time) {
	l.next = now
}

// Wait bloqueia até que o próximo evento possa ser disparado, devolvendo o
// instante em que o disparo é liberado.
//
// Sem limite (interval == 0): retorna imediatamente o now informado.
// Com limite: dorme até l.next e avança l.next em +interval. Note que o
// agendamento é por horário-alvo (não "now + interval"), de modo que erros de
// arredondamento não acumulam ao longo da rampa.
func (l *Limiter) Wait(now time.Time) time.Time {
	if l.interval <= 0 {
		return now
	}
	if l.next.IsZero() {
		l.next = now
	}
	if d := l.next.Sub(now); d > 0 {
		time.Sleep(d)
	}
	l.next = l.next.Add(l.interval)
	return l.next
}
