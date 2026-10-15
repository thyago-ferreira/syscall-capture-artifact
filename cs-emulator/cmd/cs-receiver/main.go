// Comando cs-receiver: instrumento do LADO DESTINO do experimento de captura.
//
// NÃO faz parte do emulador (que só gera e envia). Este binário roda na Máquina
// destino: recebe as requisições POST /events/vehicles HTTP/1.0 emitidas pelo
// cs-emu, responde 200 e grava um registro de VERDADE-BASE (ground truth)
// append-only — um objeto JSON por linha (jsonl) com seq, tamanho, hash e
// instante de recepção de cada evento.
//
// Esse jsonl é a referência contra a qual se mede, depois, a PERDA DE EVENTOS e
// a INTEGRIDADE DO PAYLOAD de cada ferramenta de captura (que roda na origem):
// basta comparar o conjunto de seq e os hashes capturados com os recebidos aqui.
//
// Uso:
//
//	cs-receiver [--listen :9000] [--out received.jsonl] [--quiet]
//
// O receiver é desenhado para NÃO ser gargalo da rampa: uma goroutine por
// conexão (via net/http) e um único escritor serializado por mutex. Se ele
// saturar antes da ferramenta de captura, a falha medida seria do receiver, não
// da ferramenta — por isso o mantemos enxuto.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// record é uma linha do ground truth (um evento recebido).
type record struct {
	Seq           uint64 `json:"seq"`            // = episode, o contador do emulador
	ContentLength int    `json:"content_length"` // bytes do corpo efetivamente lidos
	SHA256        string `json:"sha256"`         // hash do corpo, para checar integridade
	RecvUnixNano  int64  `json:"recv_unixnano"`  // instante de recepção na destino
	RemoteAddr    string `json:"remote_addr"`    // origem da conexão
}

// bodyHead decodifica apenas o campo episode do corpo (array de objetos de
// placa). É o seq que o emulador embute; serve de identificador do evento.
type bodyHead struct {
	Episode uint64 `json:"episode"`
}

// writer serializa a escrita do jsonl entre as goroutines de conexão.
type writer struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

func (w *writer) write(r record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(r) // Encode já anexa '\n' => jsonl
}

func main() {
	listen := flag.String("listen", ":9000", "endereço de escuta (host:porta; vazio = todas as interfaces)")
	out := flag.String("out", "received.jsonl", "arquivo append-only de ground truth (jsonl)")
	quiet := flag.Bool("quiet", false, "suprime o log por evento (mantém o resumo)")
	flag.Parse()

	f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatalf("não consegui abrir %s: %v", *out, err)
	}
	defer f.Close()
	w := &writer{f: f, enc: json.NewEncoder(f)}

	var received, errs uint64

	mux := http.NewServeMux()
	mux.HandleFunc("/events/vehicles", func(rw http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			atomic.AddUint64(&errs, 1)
			http.Error(rw, "erro lendo corpo", http.StatusBadRequest)
			return
		}

		// Extrai o seq (episode) do primeiro objeto do array. Se falhar, grava
		// seq=0 — o evento conta como recebido mas sem identificador.
		var head []bodyHead
		var seq uint64
		if json.Unmarshal(body, &head) == nil && len(head) > 0 {
			seq = head[0].Episode
		}

		sum := sha256.Sum256(body)
		rec := record{
			Seq:           seq,
			ContentLength: len(body),
			SHA256:        hex.EncodeToString(sum[:]),
			RecvUnixNano:  time.Now().UnixNano(),
			RemoteAddr:    req.RemoteAddr,
		}
		if err := w.write(rec); err != nil {
			atomic.AddUint64(&errs, 1)
			log.Printf("erro gravando ground truth (seq=%d): %v", seq, err)
		}

		n := atomic.AddUint64(&received, 1)
		if !*quiet {
			log.Printf("recv seq=%d len=%d sha256=%s.. (#%d)", seq, len(body), rec.SHA256[:12], n)
		}

		// Resposta mínima; HTTP/1.0 sem keep-alive fecha a conexão (fiel ao
		// Connection: close do perfil).
		rw.Header().Set("Content-Length", "0")
		rw.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: *listen, Handler: mux}

	// Encerramento limpo por sinal, com resumo das contagens próprias.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("sinal de parada: encerrando (recebidos=%d erros=%d)", atomic.LoadUint64(&received), atomic.LoadUint64(&errs))
		_ = srv.Close()
	}()

	log.Printf("cs-receiver escutando em %s, ground truth em %s", *listen, *out)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("servidor falhou: %v", err)
	}
	log.Printf("resumo: recebidos=%d erros=%d", atomic.LoadUint64(&received), atomic.LoadUint64(&errs))
}
