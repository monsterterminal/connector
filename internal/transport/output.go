package transport

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/monsterterminal/connector/internal/agents"
	"github.com/monsterterminal/connector/internal/files"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/terminal"
)

// Ritmo e contropressione dell'output del terminale (§1.7).
const (
	DefaultOutputHighWater = 4 << 20 // oltre: i canali smettono di leggere da tmux
	DefaultOutputLowWater  = 1 << 20 // sotto: riprendono
	// DefaultOutputFlushDelay è quanto si accorpa l'output prima di spedirlo: ben sotto i 16 ms della
	// spec, per non far sentire il ritardo dell'eco a chi scrive.
	DefaultOutputFlushDelay = 5 * time.Millisecond
)

// Terminal è il gestore dei canali visto dal trasporto (implementato da *terminal.Manager).
type Terminal interface {
	Attach(out terminal.Out)
	Handle(env protocol.Envelope) bool
	Input(channel uint32, data []byte)
	Detach(shutdown bool)
	SetMaxChannels(n int)
}

// Requests gestisce le richieste dell'agente (§2.9, §2.12): request.respond in ingresso,
// request.context e request.respond.result in uscita. nil = nessuna richiesta.
type Requests interface {
	Attach(out agents.Out)
	Handle(env protocol.Envelope) bool
	Detach()
}

// Files riceve i file di DragMeHere (schemi 0.7.0, ADR 0409 A1.5): file.put e file.cancel verso ~/dragmehere.
// nil = nessun ricevitore.
type Files interface {
	Attach(out files.Out)
	Handle(env protocol.Envelope) bool
	Detach()
}

var errOutputClosed = errors.New("connessione chiusa: output non più accettato")

// outQueue accoda l'output per canale. Il mittente lo spedisce in frame da al massimo 64 KiB, un canale
// alla volta a turno, dopo al più FlushDelay dal primo byte in attesa, attraverso il limitatore dei
// messaggi (§1.17): se i gettoni mancano, l'output si accumula e parte in frame più grandi.
type outQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	pending map[uint32][]byte
	since   map[uint32]time.Time
	order   []uint32
	total   int
	paused  bool
	closed  bool
	high    int
	low     int
	now     func() time.Time
}

func newOutQueue(high, low int, now func() time.Time) *outQueue {
	q := &outQueue{pending: map[uint32][]byte{}, since: map[uint32]time.Time{}, high: high, low: low, now: now}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Output accoda e, se la coda ha superato il tetto, attende che scenda sotto la soglia bassa: il
// canale intanto non legge da tmux, che conserva tutto nella sessione.
func (q *outQueue) Output(ch uint32, data []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errOutputClosed
	}
	if _, ok := q.pending[ch]; !ok {
		q.order = append(q.order, ch)
		q.since[ch] = q.now()
	}
	q.pending[ch] = append(q.pending[ch], data...)
	q.total += len(data)
	if q.total > q.high {
		q.paused = true
	}
	q.cond.Broadcast()
	for q.paused && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return errOutputClosed
	}
	return nil
}

// Discard toglie l'output in attesa di un canale chiuso.
func (q *outQueue) Discard(ch uint32) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if buf, ok := q.pending[ch]; ok {
		q.total -= len(buf)
		delete(q.pending, ch)
		delete(q.since, ch)
		for i, c := range q.order {
			if c == ch {
				q.order = append(q.order[:i], q.order[i+1:]...)
				break
			}
		}
		q.unpauseLocked()
	}
}

func (q *outQueue) unpauseLocked() {
	if q.paused && q.total < q.low {
		q.paused = false
		q.cond.Broadcast()
	}
}

func (q *outQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Buffered è il totale in attesa (per i test).
func (q *outQueue) Buffered() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.total
}

// next aspetta il prossimo pezzo da spedire. ok=false a coda chiusa.
func (q *outQueue) next(ctx context.Context, delay time.Duration, sleep func(context.Context, time.Duration) error) (ch uint32, chunk []byte, ok bool) {
	for {
		q.mu.Lock()
		for len(q.order) == 0 && !q.closed {
			q.cond.Wait()
		}
		if q.closed {
			q.mu.Unlock()
			return 0, nil, false
		}
		ch = q.order[0]
		buf := q.pending[ch]
		wait := q.since[ch].Add(delay).Sub(q.now())
		if len(buf) < protocol.MaxOutputChunk && wait > 0 {
			q.mu.Unlock()
			if err := sleep(ctx, wait); err != nil {
				return 0, nil, false
			}
			continue
		}
		n := len(buf)
		if n > protocol.MaxOutputChunk {
			n = protocol.MaxOutputChunk
		}
		chunk = buf[:n:n]
		q.order = q.order[1:]
		if n == len(buf) {
			delete(q.pending, ch)
			delete(q.since, ch)
		} else {
			// Il resto torna in fondo al turno: un canale che produce molto non affama gli altri.
			q.pending[ch] = append([]byte(nil), buf[n:]...)
			q.order = append(q.order, ch)
		}
		q.total -= n
		q.unpauseLocked()
		q.mu.Unlock()
		return ch, chunk, true
	}
}

// flushOutput spedisce l'output finché la coda non si chiude o la scrittura fallisce.
func flushOutput(ctx context.Context, cfg *Config, w *writer, q *outQueue) {
	for {
		ch, chunk, ok := q.next(ctx, cfg.OutputFlushDelay, cfg.Sleep)
		if !ok {
			return
		}
		if err := w.writeType(ctx, websocket.MessageBinary, protocol.EncodeFrame(protocol.FrameOutput, ch, chunk)); err != nil {
			q.close()
			return
		}
	}
}

// termOut è la connessione vista dai canali.
type termOut struct {
	ctx context.Context
	cfg *Config
	w   *writer
	q   *outQueue
}

func (o *termOut) Control(typ, re string, body any) error {
	frame, _, err := protocol.Encode(typ, re, body, o.cfg.Now())
	if err != nil {
		return err
	}
	// Anche a connessione in chiusura (session.closed «connector-shutdown»), ma mai per più di 5 s.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(o.ctx), 5*time.Second)
	defer cancel()
	return o.w.write(ctx, frame)
}

func (o *termOut) Output(ch uint32, data []byte) error { return o.q.Output(ch, data) }
func (o *termOut) Discard(ch uint32)                   { o.q.Discard(ch) }
