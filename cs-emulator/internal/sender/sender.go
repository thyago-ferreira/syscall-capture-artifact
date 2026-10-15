// Package sender entrega cada evento à Máquina destino como uma requisição
// HTTP/1.0 completa, via TCP.
//
// Invariante central: a captura do experimento intercepta a
// SYSCALL, não o pacote. Portanto cada evento deve sair em UM ÚNICO write/sendto
// sobre o socket — nunca fragmentar na aplicação. Aqui montamos o envelope HTTP
// (request line + headers) e o corpo no MESMO buffer reusado e fazemos uma única
// chamada Write com envelope+corpo juntos.
//
// Em Go, um net.TCPConn.Write traduz-se em uma chamada write(2) sobre o socket;
// para um buffer do tamanho de um evento (~6,3 KB) o kernel aceita o buffer
// inteiro em uma syscall. Garantimos isso com uma única chamada Write por evento
// e verificando que n == len(buf). NÃO usamos writers com buffer intermediário
// (bufio), pois eles poderiam coalescer ou fatiar a escrita.
//
// Perfil calibrado (CS de referência, /events/vehicles): POST /events/vehicles HTTP/1.0,
// Content-Type application/json, Connection: close => modo per-event (abre,
// envia, fecha) por padrão.
package sender

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

// Mode define a estratégia de conexão.
type Mode int

const (
	// PerEvent abre uma nova conexão TCP por evento e a fecha em seguida.
	// É o padrão, fiel ao Connection: close do perfil calibrado.
	PerEvent Mode = iota
	// Persistent reutiliza uma única conexão TCP para todos os eventos.
	// NÃO é fiel ao perfil (que usa Connection: close); existe apenas para
	// medir o teto de throughput do próprio emulador.
	Persistent
)

// Cabeçalho HTTP do perfil. A rota e o método são do perfil calibrado; o Host
// é genérico/sintético — NUNCA o host real do CS.
const (
	requestLine = "POST /events/vehicles HTTP/1.0\r\n"
	genericHost = "cs-emulator.local"
)

// Sender encapsula a entrega de eventos a um destino TCP.
//
// NÃO é seguro para uso concorrente: o modo Persistent compartilha uma conexão e
// o reqBuf é reusado entre chamadas.
type Sender struct {
	dest string
	mode Mode

	dialTimeout  time.Duration
	writeTimeout time.Duration

	conn   net.Conn // conexão viva no modo Persistent; nil no modo PerEvent
	reqBuf []byte   // buffer reusado da requisição (envelope+corpo); zero-aloc no hot path

	// lastSendNanos guarda a duração (ns) da ÚLTIMA chamada conn.Write — a
	// "latência de envio" do payload na origem. É exatamente o syscall write que
	// a ferramenta de captura intercepta; sob ptrace cada write é parado 2x, de
	// modo que esta duração mede direto o overhead da ferramenta (relógio único,
	// sem skew entre máquinas). Não inclui dial/close.
	lastSendNanos int64
}

// New cria um Sender para o destino dado. Não conecta ainda — a conexão é
// estabelecida sob demanda (em Send, ou explicitamente via Connect).
func New(dest string, mode Mode) *Sender {
	return &Sender{
		dest:         dest,
		mode:         mode,
		dialTimeout:  5 * time.Second,
		writeTimeout: 5 * time.Second,
		reqBuf:       make([]byte, 0, 8192), // folga p/ envelope + corpo (~6,3 KB)
	}
}

// dial abre uma conexão TCP e desabilita o algoritmo de Nagle, para que cada
// Write vá ao fio imediatamente (sem coalescência entre eventos).
func (s *Sender) dial() (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", s.dest, s.dialTimeout)
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		// SetNoDelay(true) desliga o Nagle: relevante para o perfil de I/O.
		_ = tcp.SetNoDelay(true)
	}
	return conn, nil
}

// Connect estabelece a conexão persistente antecipadamente. No-op em PerEvent.
func (s *Sender) Connect() error {
	if s.mode != Persistent {
		return nil
	}
	if s.conn != nil {
		return nil
	}
	conn, err := s.dial()
	if err != nil {
		return err
	}
	s.conn = conn
	return nil
}

// buildRequest monta a requisição HTTP/1.0 completa (envelope + corpo) no
// buffer reusado e devolve o slice resultante. O Content-Length é exatamente
// len(body), mantendo coerência com o corpo enviado.
func (s *Sender) buildRequest(body []byte) []byte {
	buf := s.reqBuf[:0]
	buf = append(buf, requestLine...)
	buf = append(buf, "Host: "...)
	buf = append(buf, genericHost...)
	buf = append(buf, "\r\n"...)
	buf = append(buf, "Content-Type: application/json\r\n"...)
	buf = append(buf, "Content-Length: "...)
	buf = strconv.AppendInt(buf, int64(len(body)), 10)
	buf = append(buf, "\r\n"...)
	buf = append(buf, "Connection: close\r\n"...)
	buf = append(buf, "\r\n"...) // fim dos headers
	buf = append(buf, body...)
	s.reqBuf = buf
	return buf
}

// Send entrega um evento. Recebe o CORPO JSON; monta o envelope HTTP e escreve
// envelope+corpo em uma única syscall write.
func (s *Sender) Send(body []byte) error {
	req := s.buildRequest(body)
	switch s.mode {
	case Persistent:
		return s.sendPersistent(req)
	default:
		return s.sendPerEvent(req)
	}
}

func (s *Sender) sendPersistent(req []byte) error {
	if s.conn == nil {
		if err := s.Connect(); err != nil {
			return err
		}
	}
	if err := s.writeOnce(s.conn, req); err != nil {
		// Conexão pode ter caído; descarta para reconectar na próxima.
		_ = s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}

func (s *Sender) sendPerEvent(req []byte) error {
	conn, err := s.dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	return s.writeOnce(conn, req)
}

// writeOnce faz exatamente uma chamada Write e exige que todo o buffer tenha
// sido aceito. Um short write violaria o invariante de "um write por evento";
// por isso o tratamos como erro em vez de fazer loop de reescrita.
func (s *Sender) writeOnce(conn net.Conn, buf []byte) error {
	if s.writeTimeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(s.writeTimeout))
	}
	t0 := time.Now()
	n, err := conn.Write(buf)
	s.lastSendNanos = time.Since(t0).Nanoseconds()
	if err != nil {
		return err
	}
	if n != len(buf) {
		return fmt.Errorf("short write: %d de %d bytes", n, len(buf))
	}
	return nil
}

// LastSendNanos devolve a duração (ns) do write do evento mais recente. Válido
// após um Send bem-sucedido; serve à medição de latência de envio (ver campo
// lastSendNanos). Duas leituras de relógio por evento, fora do caminho de rede.
func (s *Sender) LastSendNanos() int64 { return s.lastSendNanos }

// Close encerra a conexão persistente, se houver.
func (s *Sender) Close() error {
	if s.conn != nil {
		err := s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}
