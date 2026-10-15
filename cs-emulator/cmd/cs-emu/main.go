// Comando cs-emu: emulador do Sistema Constituinte (CS) do experimento de captura.
//
// Esta é a Máquina origem: gera eventos JSON (~6,3 KB) e os envia por TCP à
// Máquina destino, em uma única syscall write por evento, a uma taxa modulável
// (a rampa do experimento). Este binário SÓ gera e envia carga — não contém
// captura, medição de overhead nem tradução semântica.
//
// Uso:
//
//	cs-emu --dest host:porta [--size 6300] [--rate 1] [--ramp 1] \
//	       [--conn per-event|persistent] [--duration 30s]
//
// Cada evento é uma requisição HTTP/1.0 completa (POST /events/vehicles), fiel ao
// perfil calibrado do CS de referência. A taxa efetiva é --rate (base, req/s) × --ramp
// (multiplicador da rampa: 1, 2, 4, 8, 16, 32…).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"syscall"
	"time"

	"csbench/cs-emulator/internal/event"
	"csbench/cs-emulator/internal/rate"
	"csbench/cs-emulator/internal/sender"
)

// config agrega os parâmetros de execução. Defaults declarados aqui; NADA do
// perfil real é hardcoded além de placeholders parametrizáveis.
type config struct {
	dest     string
	size     int
	rate     float64
	ramp     float64
	conn     string
	duration time.Duration
	latOut   string
	seqBase  uint64
}

// latRec é um registro de latência por evento, acumulado em memória durante o
// loop e despejado SÓ no fim (ver run). seq casa com o received.jsonl do
// cs-receiver para derivar a latência fim-a-fim; sendNanos é a duração do write
// na origem (overhead da captura, relógio único).
type latRec struct {
	seq          uint64
	sendUnixNano int64
	sendNanos    int64
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.dest, "dest", "", "host:porta da Máquina destino (obrigatório)")
	flag.IntVar(&c.size, "size", 6300, "tamanho-alvo do corpo do evento em bytes (== Content-Length)")
	flag.Float64Var(&c.rate, "rate", 1, "taxa-base em req/s (ponto de operação real; <=0 = sem limite)")
	flag.Float64Var(&c.ramp, "ramp", 1, "multiplicador da rampa (1, 2, 4, 8, 16, 32…); taxa efetiva = rate*ramp")
	flag.StringVar(&c.conn, "conn", "per-event", "modo de conexão: per-event (fiel) | persistent")
	flag.DurationVar(&c.duration, "duration", 30*time.Second, "duração do disparo (0 = indefinido)")
	flag.StringVar(&c.latOut, "lat-out", "", "se dado, grava latência por evento (jsonl: seq, send_unixnano, send_dur_ns) no fim do run; vazio = sem medição")
	flag.Uint64Var(&c.seqBase, "seq-base", 0, "desloca o seq inicial (1º evento = base+1); torna os seqs únicos entre runs p/ não colidir no received.jsonl")
	flag.Parse()
	return c
}

func connMode(s string) (sender.Mode, error) {
	switch s {
	case "persistent":
		return sender.Persistent, nil
	case "per-event":
		return sender.PerEvent, nil
	default:
		return 0, fmt.Errorf("modo de conexão inválido: %q (use persistent|per-event)", s)
	}
}

func main() {
	cfg := parseFlags()

	if cfg.dest == "" {
		log.Fatal("--dest é obrigatório (host:porta da Máquina destino)")
	}
	mode, err := connMode(cfg.conn)
	if err != nil {
		log.Fatal(err)
	}

	builder := event.New(cfg.size)
	builder.SetSeqBase(cfg.seqBase)
	limiter := rate.NewRamp(cfg.rate, cfg.ramp)
	snd := sender.New(cfg.dest, mode)

	// Estabelece a conexão persistente antes do loop, para não contaminar o
	// primeiro evento com o custo do handshake. No-op em per-event.
	if err := snd.Connect(); err != nil {
		log.Fatalf("falha ao conectar em %s: %v", cfg.dest, err)
	}
	defer snd.Close()

	if err := run(cfg, builder, limiter, snd); err != nil {
		log.Fatalf("execução interrompida: %v", err)
	}
}

// run executa o loop de disparo até a duração esgotar ou um sinal de parada.
func run(cfg config, builder *event.Builder, limiter *rate.Limiter, snd *sender.Sender) error {
	// Parada por sinal (Ctrl-C / SIGTERM) para encerrar limpo.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	// Prazo de término por duração (0 = indefinido).
	var deadline time.Time
	if cfg.duration > 0 {
		deadline = time.Now().Add(cfg.duration)
	}

	log.Printf("cs-emu iniciando: dest=%s size=%dB rate=%.3g/s ramp=%.3gx efetiva=%.3g/s conn=%s duration=%s seq_base=%d",
		cfg.dest, cfg.size, cfg.rate, cfg.ramp, cfg.rate*cfg.ramp, cfg.conn, cfg.duration, cfg.seqBase)

	// Coleta de latência por evento, gated por --lat-out. Pré-alocada por uma
	// estimativa (taxa efetiva × duração) para evitar realloc no hot path; o
	// despejo em arquivo acontece só no fim, sem syscalls extras durante o loop
	// (que poluiriam/perturbariam a ferramenta de captura).
	var lat []latRec
	if cfg.latOut != "" {
		est := int(cfg.rate * cfg.ramp * cfg.duration.Seconds() * 1.2)
		if est < 64 {
			est = 64
		}
		lat = make([]latRec, 0, est)
	}

	var sent, errs uint64
	start := time.Now()
	limiter.Start(start)

	for {
		select {
		case <-stop:
			log.Print("sinal de parada recebido")
			reportMetrics(sent, errs, start)
			return dumpLatency(cfg.latOut, lat)
		default:
		}

		now := time.Now()
		if !deadline.IsZero() && now.After(deadline) {
			break
		}

		limiter.Wait(now)

		sendUnixNano := time.Now().UnixNano()
		buf := builder.Next(sendUnixNano)
		if err := snd.Send(buf); err != nil {
			errs++
			// Métricas próprias: erros vão para stderr, separados dos
			// medidores do experimento. Não abortamos a rampa por um erro.
			log.Printf("erro no envio (seq=%d): %v", builder.Seq(), err)
			continue
		}
		sent++
		if lat != nil {
			lat = append(lat, latRec{seq: builder.Seq(), sendUnixNano: sendUnixNano, sendNanos: snd.LastSendNanos()})
		}
	}

	reportMetrics(sent, errs, start)
	return dumpLatency(cfg.latOut, lat)
}

// dumpLatency despeja os registros de latência em jsonl e loga os percentis da
// duração do write (p50/p90/p99/max) para leitura rápida. No-op se --lat-out
// não foi dado. Roda FORA do loop temporizado, então não perturba a captura.
func dumpLatency(path string, lat []latRec) error {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("não consegui criar %s: %w", path, err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, r := range lat {
		buf := make([]byte, 0, 96)
		buf = append(buf, `{"seq":`...)
		buf = strconv.AppendUint(buf, r.seq, 10)
		buf = append(buf, `,"send_unixnano":`...)
		buf = strconv.AppendInt(buf, r.sendUnixNano, 10)
		buf = append(buf, `,"send_dur_ns":`...)
		buf = strconv.AppendInt(buf, r.sendNanos, 10)
		buf = append(buf, "}\n"...)
		if _, err := w.Write(buf); err != nil {
			return fmt.Errorf("erro gravando %s: %w", path, err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("erro finalizando %s: %w", path, err)
	}
	logSendPercentiles(lat)
	return nil
}

// logSendPercentiles ordena as durações de write e emite p50/p90/p99/max em µs.
func logSendPercentiles(lat []latRec) {
	if len(lat) == 0 {
		return
	}
	d := make([]int64, len(lat))
	for i, r := range lat {
		d[i] = r.sendNanos
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	pct := func(p float64) float64 {
		idx := int(p * float64(len(d)-1))
		return float64(d[idx]) / 1000.0 // ns -> µs
	}
	log.Printf("latência de envio (write): p50=%.1fµs p90=%.1fµs p95=%.1fµs p99=%.1fµs max=%.1fµs n=%d",
		pct(0.50), pct(0.90), pct(0.95), pct(0.99), float64(d[len(d)-1])/1000.0, len(d))
}

// reportMetrics emite as métricas PRÓPRIAS do emulador (enviados, erros, taxa
// efetiva) em stderr. São distintas dos medidores do experimento, que medem o
// CS — servem apenas para operar e validar o disparo.
func reportMetrics(sent, errs uint64, start time.Time) {
	elapsed := time.Since(start)
	var eff float64
	if elapsed > 0 {
		eff = float64(sent) / elapsed.Seconds()
	}
	log.Printf("métricas: enviados=%d erros=%d duração=%s taxa_efetiva=%.1f/s",
		sent, errs, elapsed.Round(time.Millisecond), eff)
}
