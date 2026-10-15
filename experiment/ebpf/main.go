// Comando cs-ebpf-capture: ferramenta de captura via eBPF (libbpf-CO-RE) do
// experimento de captura. Mecanismo kernel-eBPF; a flag --attach escolhe a ANCORAGEM:
//
//	kprobe  — breakpoint na função do caminho de write (ksys_write)
//	fentry  — trampoline BPF na entrada da mesma função (Fase C)
//
// Mesmo programa BPF e mesmo empacotamento para os dois; difere só o tipo de
// attach — é essa a comparação intra-mecanismo.
//
// Para cada write cujo buffer começa com a request line do evento, o BPF copia o
// buffer inteiro para um ringbuf; aqui separamos envelope/corpo, calculamos o
// sha256 do CORPO e gravamos capture.jsonl no MESMO schema do parse_strace
// (seq, content_length, declared_len, sha256) — compare.py/finalize.py reusam.
//
// Load/attach exigem root (CAP_BPF/CAP_PERFMON). O BUILD não exige root.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

const (
	stMatched = uint32(0)
	stDropped = uint32(1)
)

var headerSep = []byte("\r\n\r\n")
var clenKey = []byte("Content-Length:")
var episodeKey = []byte(`"episode":`)

func main() {
	attach := flag.String("attach", "kprobe", "tipo de ancoragem: kprobe | fentry")
	out := flag.String("out", "capture.jsonl", "arquivo de captura (jsonl) no schema do parse_strace")
	duration := flag.Duration("duration", 0, "duração da captura (0 = até SIGINT/SIGTERM)")
	flag.Parse()

	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("removendo rlimit memlock: %v", err)
	}

	spec, err := loadCapture()
	if err != nil {
		log.Fatalf("carregando spec BPF: %v", err)
	}
	// Carrega SÓ o programa do attach escolhido: em modo kprobe NÃO carrega o
	// programa fentry, preservando a independência de BTF do kprobe (o programa
	// fentry exigiria BTF da função-alvo).
	var progName string
	switch *attach {
	case "kprobe":
		progName = "on_ksys_write"
		delete(spec.Programs, "on_ksys_write_fentry")
	case "fentry":
		progName = "on_ksys_write_fentry"
		delete(spec.Programs, "on_ksys_write")
	default:
		log.Fatalf("--attach inválido: %q (use kprobe|fentry)", *attach)
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		log.Fatalf("carregando objeto BPF (%s): %v", *attach, err)
	}
	defer coll.Close()
	prog := coll.Programs[progName]
	eventsMap := coll.Maps["events"]
	statsMap := coll.Maps["stats"]

	var lnk link.Link
	switch *attach {
	case "kprobe":
		lnk, err = link.Kprobe("ksys_write", prog, nil)
	case "fentry":
		lnk, err = link.AttachTracing(link.TracingOptions{Program: prog})
	}
	if err != nil {
		log.Fatalf("anexando %s: %v", *attach, err)
	}
	defer lnk.Close()

	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("criando %s: %v", *out, err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	defer w.Flush()

	rd, err := ringbuf.NewReader(eventsMap)
	if err != nil {
		log.Fatalf("abrindo ringbuf: %v", err)
	}
	defer rd.Close()

	// Encerra por duração ou sinal: fecha o reader, o que desbloqueia rd.Read.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		if *duration > 0 {
			select {
			case <-stop:
			case <-time.After(*duration):
			}
		} else {
			<-stop
		}
		rd.Close()
	}()

	log.Printf("cs-ebpf-capture: %s anexado em ksys_write; gravando %s", *attach, *out)

	var written uint64
	for {
		rec, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				break
			}
			continue
		}
		if processRecord(rec.RawSample, *attach, w) {
			written++
		}
	}
	w.Flush()

	matched := readStat(statsMap, stMatched)
	dropped := readStat(statsMap, stDropped)
	log.Printf("resumo: matched=%d gravados=%d dropped_ringbuf=%d (matched-gravados=%d)",
		matched, written, dropped, matched-written)
}

func readStat(m *ebpf.Map, idx uint32) uint64 {
	var v uint64
	_ = m.Lookup(idx, &v)
	return v
}

// processRecord decodifica um registro do ringbuf (len + bytes do write), separa
// envelope/corpo, calcula o sha256 do corpo e grava uma linha jsonl. Retorna true
// se gravou.
func processRecord(raw []byte, attach string, w *bufio.Writer) bool {
	if len(raw) < 4 {
		return false
	}
	n := binary.LittleEndian.Uint32(raw[:4])
	if int(n) > len(raw)-4 {
		n = uint32(len(raw) - 4) // defensivo
	}
	payload := raw[4 : 4+n]

	sep := bytes.Index(payload, headerSep)
	var header, body []byte
	if sep >= 0 {
		header = payload[:sep]
		body = payload[sep+len(headerSep):]
	} else {
		body = payload
	}

	seq := extractEpisode(body)
	declared := extractContentLength(header)
	sum := sha256.Sum256(body)

	// Linha jsonl no schema do parse_strace + attach (contexto).
	buf := make([]byte, 0, 160)
	buf = append(buf, `{"seq":`...)
	buf = strconv.AppendUint(buf, seq, 10)
	buf = append(buf, `,"content_length":`...)
	buf = strconv.AppendInt(buf, int64(len(body)), 10)
	buf = append(buf, `,"declared_len":`...)
	if declared >= 0 {
		buf = strconv.AppendInt(buf, declared, 10)
	} else {
		buf = append(buf, "null"...)
	}
	buf = append(buf, `,"sha256":"`...)
	buf = append(buf, hex.EncodeToString(sum[:])...)
	buf = append(buf, `","attach":"`...)
	buf = append(buf, attach...)
	buf = append(buf, "\"}\n"...)
	_, _ = w.Write(buf)
	return true
}

// extractEpisode varre o corpo (array JSON) e lê o 1º "episode":N. Scan manual
// (sem encoding/json) p/ minimizar custo no caminho da captura.
func extractEpisode(body []byte) uint64 {
	i := bytes.Index(body, episodeKey)
	if i < 0 {
		return 0
	}
	j := i + len(episodeKey)
	for j < len(body) && body[j] == ' ' {
		j++
	}
	var v uint64
	for j < len(body) && body[j] >= '0' && body[j] <= '9' {
		v = v*10 + uint64(body[j]-'0')
		j++
	}
	return v
}

// extractContentLength lê o Content-Length do envelope (-1 se ausente).
func extractContentLength(header []byte) int64 {
	i := bytes.Index(header, clenKey)
	if i < 0 {
		return -1
	}
	j := i + len(clenKey)
	for j < len(header) && (header[j] == ' ' || header[j] == '\t') {
		j++
	}
	var v int64
	got := false
	for j < len(header) && header[j] >= '0' && header[j] <= '9' {
		v = v*10 + int64(header[j]-'0')
		j++
		got = true
	}
	if !got {
		return -1
	}
	return v
}
