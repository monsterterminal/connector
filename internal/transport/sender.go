package transport

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/ciarasino/monsterterminal-connector/internal/journal"
	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
)

// Limiti del backend per host (§1.17): 50 messaggi al secondo in finestra scorrevole, binari
// compresi, e una coda di 64 messaggi in attesa di elaborazione. Il connettore resta sotto con
// margine: oltre, il backend chiude con 4429.
const (
	DefaultMaxRate     = 40 // messaggi al secondo, tutti i frame in uscita
	DefaultRateBurst   = 5  // nel secondo peggiore: 5 + 40 = 45, sotto i 50
	DefaultMaxInFlight = 32 // eventi inviati e non ancora confermati
)

// EventStore è il giornale visto dal trasporto (implementato da *journal.Journal).
type EventStore interface {
	LastAcked() int64
	Pending(after int64, max int) []protocol.Event
	Ack(upTo int64) error
	Resync(bookmark int64) error
	Notify() <-chan struct{}
}

// limiter è un secchio di gettoni: rate al secondo, burst gettoni al massimo. Conta OGNI frame
// in uscita, perché il backend li conta tutti.
type limiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newLimiter(rate, burst int, now func() time.Time) *limiter {
	return &limiter{rate: float64(rate), burst: float64(burst), tokens: float64(burst), last: now(), now: now}
}

// reserve prende un gettone e dice quanto attendere prima di usarlo.
func (l *limiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
	l.tokens--
	if l.tokens >= 0 {
		return 0
	}
	return time.Duration(-l.tokens / l.rate * float64(time.Second))
}

// writer serializza le scritture della sessione attraverso il limitatore.
type writer struct {
	conn  *websocket.Conn
	lim   *limiter
	sleep func(context.Context, time.Duration) error
	sent  atomic.Int64 // frame scritti, per i test
}

func (w *writer) write(ctx context.Context, frame []byte) error {
	if d := w.lim.reserve(); d > 0 {
		if err := w.sleep(ctx, d); err != nil {
			return err
		}
	}
	w.sent.Add(1)
	return w.conn.Write(ctx, websocket.MessageText, frame)
}

// syncState è lo stato del riallineamento col backend dopo welcome (§1.12: fa fede il backend).
type syncState struct {
	ahead   atomic.Bool // arrivato ACK_AHEAD
	acked   atomic.Bool // arrivato almeno un events.ack
	restart atomic.Bool // il segnalibro è stato riportato indietro: il mittente riparte da lì
	ackCh   chan struct{}
}

func newSyncState() *syncState { return &syncState{ackCh: make(chan struct{}, 1)} }

func (s *syncState) poke() {
	select {
	case s.ackCh <- struct{}{}:
	default:
	}
}

// onAck applica un events.ack: in avanti conferma, all'indietro riallinea al segnalibro del backend.
func onAck(cfg *Config, st *syncState, upTo int64) error {
	st.acked.Store(true)
	if upTo < cfg.Events.LastAcked() {
		cfg.Logger.Warn("segnalibro del backend indietro rispetto al giornale: riallineo", "backend", upTo, "giornale", cfg.Events.LastAcked())
		if err := cfg.Events.Resync(upTo); err != nil {
			return err
		}
		st.restart.Store(true)
	} else if err := cfg.Events.Ack(upTo); err != nil {
		return err
	}
	st.poke()
	return nil
}

// sendLoop invia gli eventi del giornale finché ctx non termina. Prima attende la finestra di
// riallineamento: il backend, dopo welcome, manda ACK_AHEAD e/o events.ack col suo segnalibro se
// differisce dal lastAckedSeq di hello. Senza events.ack dopo ACK_AHEAD il segnalibro è 0.
func sendLoop(ctx context.Context, cfg *Config, w *writer, st *syncState) error {
	if err := cfg.Sleep(ctx, cfg.SyncGrace); err != nil {
		return err
	}
	if st.ahead.Load() && !st.acked.Load() {
		wctx, cancel := context.WithTimeout(ctx, cfg.AckAheadWait)
		for !st.acked.Load() && wctx.Err() == nil {
			select {
			case <-wctx.Done():
			case <-st.ackCh:
			}
		}
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !st.acked.Load() {
			cfg.Logger.Warn("ACK_AHEAD senza segnalibro: il backend non ha eventi di questo host")
			if err := cfg.Events.Resync(0); err != nil {
				return err
			}
		}
	}
	st.restart.Store(false)
	prev := cfg.Events.LastAcked()
	inflight := 0
	for {
		if st.restart.Swap(false) {
			prev = cfg.Events.LastAcked()
		}
		acked := cfg.Events.LastAcked()
		if acked > prev {
			prev = acked
		}
		// Eventi in volo: i seq fra il segnalibro e l'ultimo inviato (un troncamento ne conta
		// più d'uno: stima per eccesso, quindi prudente).
		inflight = int(prev - acked)
		room := cfg.MaxInFlight - inflight
		var evs []protocol.Event
		if room > 0 {
			evs = cfg.Events.Pending(prev, room)
		}
		if len(evs) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-cfg.Events.Notify():
			case <-st.ackCh:
			}
			continue
		}
		for _, ev := range evs {
			if st.restart.Load() {
				break
			}
			ev = journal.Rebase(ev, prev)
			frame, _, err := protocol.Encode(protocol.TypeEvent, "", ev, cfg.Now())
			if err != nil {
				return err
			}
			if err := w.write(ctx, frame); err != nil {
				return err
			}
			prev = ev.Seq
		}
	}
}
