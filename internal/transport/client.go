// Package transport tiene il collegamento WebSocket in uscita verso il backend.
//
// Riferimento: spec monsterterminal-saas.md §1.2 (trasporto, heartbeat), §1.4 (autenticazione,
// hello/welcome), §1.12 (ricollegamento), §1.13 (revoca e kill-switch), §1.16 (codici).
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/monsterterminal/connector/internal/protocol"
)

// Codici di chiusura del protocollo (§1.16).
const (
	CloseRevoked  websocket.StatusCode = 4401
	CloseDisabled websocket.StatusCode = 4403
	CloseNoHello  websocket.StatusCode = 4408
	CloseReplaced websocket.StatusCode = 4409
	CloseTooMany  websocket.StatusCode = 4429
	// CloseInternal: errore del backend mentre elaborava un messaggio (§1.4); si ritenta col backoff.
	CloseInternal websocket.StatusCode = 1011
)

// DisabledMinWait è l'attesa minima dopo un kill-switch (§1.12, §1.13).
const DisabledMinWait = 15 * time.Minute

var (
	// ErrRevoked: l'host è stato revocato. Il connettore smette di ricollegarsi (§1.13).
	ErrRevoked = errors.New("host revocato: il connettore non si ricollega, serve una nuova registrazione")
	// ErrTokenInvalid: il token dell'host è sconosciuto al backend. Riprovare non serve.
	ErrTokenInvalid = errors.New("token dell'host non valido: serve una nuova registrazione")

	errDisabled  = errors.New("host fermato dal kill-switch")
	errHeartbeat = errors.New("nessun segnale dal backend entro il tempo dell'heartbeat")
)

// retryAfterError: il backend ha risposto 429 RATE_LIMITED con Retry-After (§1.17). L'attesa
// prima del prossimo tentativo non è mai più corta di quella indicata.
type retryAfterError struct {
	after time.Duration
	err   error
}

func (e *retryAfterError) Error() string {
	return fmt.Sprintf("troppi tentativi, riprovare fra %s: %v", e.after, e.err)
}
func (e *retryAfterError) Unwrap() error { return e.err }

// sessionError: il backend ha rifiutato un evento (SEQ_GAP, TRUNCATION_INCOHERENT). La
// connessione si chiude e il ricollegamento riparte da hello, che riallinea i segnalibri.
type sessionError struct{ code string }

func (e *sessionError) Error() string { return "evento rifiutato dal backend: " + e.code }

// Config configura il client.
type Config struct {
	// URL del WebSocket, sempre wss:// (§1.2).
	URL string
	// Token dell'host, mai scritto nei log.
	Token string
	// HTTPClient per l'handshake; la verifica TLS resta sempre attiva.
	HTTPClient *http.Client
	// Hello costruisce il messaggio iniziale a ogni collegamento.
	Hello func() protocol.Hello
	// WelcomeTimeout è l'attesa massima di welcome dopo hello.
	WelcomeTimeout time.Duration
	// HeartbeatTimeout: senza ping né messaggi del backend per questo tempo, la connessione è caduta (§1.2).
	HeartbeatTimeout time.Duration
	Backoff          *Backoff
	// Sleep attende d o finché ctx non termina; sostituibile nei test.
	Sleep func(ctx context.Context, d time.Duration) error
	// OnWelcome è chiamata a ogni welcome ricevuto.
	OnWelcome func(protocol.Welcome)
	// OnConfig è chiamata a ogni config.update valido.
	OnConfig func(protocol.HostConfig)
	Logger   *slog.Logger
	Now      func() time.Time

	// Events è il giornale degli eventi da consegnare (§1.12); nil = nessun evento.
	Events EventStore
	// SyncGrace: attesa dopo welcome prima di inviare, per ricevere ACK_AHEAD o il segnalibro.
	SyncGrace time.Duration
	// AckAheadWait: dopo ACK_AHEAD, attesa massima dell'events.ack col segnalibro del backend.
	AckAheadWait time.Duration
	// Limiti dell'invio (§1.17): messaggi al secondo, raffica, eventi in volo.
	MaxRate, RateBurst, MaxInFlight int

	// Terminal gestisce i canali del terminale (§1.7); nil = nessun terminale.
	Terminal Terminal
	// Requests gestisce le richieste dell'agente (§2.9, §2.12); nil = nessuna richiesta.
	Requests Requests
	// Coda dell'output del terminale: soglie della contropressione e accorpamento (§1.7).
	OutputHighWater, OutputLowWater int
	OutputFlushDelay                time.Duration
}

func (c *Config) defaults() {
	if c.WelcomeTimeout == 0 {
		c.WelcomeTimeout = 30 * time.Second
	}
	if c.HeartbeatTimeout == 0 {
		c.HeartbeatTimeout = 90 * time.Second
	}
	if c.Backoff == nil {
		c.Backoff = DefaultBackoff()
	}
	if c.Sleep == nil {
		c.Sleep = sleepCtx
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	if c.SyncGrace == 0 {
		c.SyncGrace = time.Second
	}
	if c.AckAheadWait == 0 {
		c.AckAheadWait = 3 * time.Second
	}
	if c.MaxRate == 0 {
		c.MaxRate = DefaultMaxRate
	}
	if c.RateBurst == 0 {
		c.RateBurst = DefaultRateBurst
	}
	if c.MaxInFlight == 0 {
		c.MaxInFlight = DefaultMaxInFlight
	}
	if c.OutputHighWater == 0 {
		c.OutputHighWater = DefaultOutputHighWater
	}
	if c.OutputLowWater == 0 {
		c.OutputLowWater = DefaultOutputLowWater
	}
	if c.OutputFlushDelay == 0 {
		c.OutputFlushDelay = DefaultOutputFlushDelay
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run mantiene il collegamento finché ctx non termina o l'host non viene revocato. Ritorna
// ErrRevoked, ErrTokenInvalid o l'errore di ctx.
func Run(ctx context.Context, cfg Config) error {
	cfg.defaults()
	if !strings.HasPrefix(cfg.URL, "wss://") {
		return errors.New("il collegamento deve essere wss:// (§1.2)")
	}
	if cfg.Hello == nil {
		return errors.New("Config.Hello mancante")
	}
	for {
		welcomed, err := session(ctx, &cfg)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if welcomed {
			cfg.Backoff.Reset()
		}
		switch {
		case errors.Is(err, ErrRevoked), errors.Is(err, ErrTokenInvalid):
			// Nessun log qui: l'errore torna al chiamante, che lo registra una volta sola.
			return err
		}
		wait := cfg.Backoff.Next()
		if errors.Is(err, errDisabled) && wait < DisabledMinWait {
			wait = DisabledMinWait
		}
		var ra *retryAfterError
		if errors.As(err, &ra) && wait < ra.after {
			wait = ra.after
		}
		cfg.Logger.Warn("collegamento caduto, nuovo tentativo", "motivo", errString(err), "attesa", wait.String())
		if err := cfg.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "chiusura normale"
	}
	return err.Error()
}

// session esegue un collegamento completo. welcomed dice se si è arrivati a welcome.
func session(ctx context.Context, cfg *Config) (welcomed bool, err error) {
	var lastSignal atomic.Int64
	lastSignal.Store(cfg.Now().UnixNano())
	header := http.Header{}
	header.Set("Authorization", "Bearer "+cfg.Token)
	conn, resp, err := websocket.Dial(ctx, cfg.URL, &websocket.DialOptions{
		HTTPClient: cfg.HTTPClient,
		HTTPHeader: header,
		OnPingReceived: func(context.Context, []byte) bool {
			lastSignal.Store(cfg.Now().UnixNano())
			return true // risponde col pong
		},
	})
	if err != nil {
		return false, classifyHandshake(resp, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxFrameBytes)

	// La sessione NON eredita l'annullamento di ctx: con coder/websocket un Read il cui contesto viene
	// annullato chiude subito la connessione, e allo spegnimento non si potrebbe più avvisare il backend.
	// Lo spegnimento passa dall'osservatore qui sotto: prima i canali (session.closed
	// «connector-shutdown»), poi la chiusura regolare 1000, poi la fine della sessione.
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	w := &writer{conn: conn, lim: newLimiter(cfg.MaxRate, cfg.RateBurst, cfg.Now), sleep: cfg.Sleep}
	go func() {
		select {
		case <-sctx.Done():
		case <-ctx.Done():
			if cfg.Terminal != nil {
				cfg.Terminal.Detach(true)
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
			cancel()
		}
	}()

	// Guardia dell'heartbeat: senza segnali oltre il tempo, chiude la connessione.
	var heartbeatExpired atomic.Bool
	go func() {
		tick := time.NewTicker(cfg.HeartbeatTimeout / 10)
		defer tick.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-tick.C:
				if time.Duration(cfg.Now().UnixNano()-lastSignal.Load()) > cfg.HeartbeatTimeout {
					heartbeatExpired.Store(true)
					conn.CloseNow()
					return
				}
			}
		}
	}()

	hello := cfg.Hello()
	if cfg.Events != nil {
		hello.LastAckedSeq = cfg.Events.LastAcked()
	}
	frame, helloID, err := protocol.Encode(protocol.TypeHello, "", hello, cfg.Now())
	if err != nil {
		return false, err
	}
	if err := w.write(sctx, frame); err != nil {
		return false, readErr(err, &heartbeatExpired)
	}

	wctx, wcancel := context.WithTimeout(sctx, cfg.WelcomeTimeout)
	defer wcancel()
	for !welcomed {
		typ, data, err := conn.Read(wctx)
		if err != nil {
			if wctx.Err() != nil && sctx.Err() == nil {
				return false, errors.New("welcome non arrivato in tempo")
			}
			return false, readErr(err, &heartbeatExpired)
		}
		lastSignal.Store(cfg.Now().UnixNano())
		if typ != websocket.MessageText {
			continue
		}
		env, err := protocol.DecodeEnvelope(data)
		if err != nil {
			return false, fmt.Errorf("primo messaggio del backend: %w", err)
		}
		switch env.Type {
		case protocol.TypeWelcome:
			if env.Re != helloID {
				return false, fmt.Errorf("welcome non riferito a hello: %w", protocol.ErrInvalid)
			}
			w, err := protocol.DecodeWelcome(env.Body)
			if err != nil {
				return false, fmt.Errorf("welcome: %w", err)
			}
			welcomed = true
			cfg.Logger.Info("collegato", "hostId", w.HostID, "protocollo", w.Protocol)
			if cfg.Terminal != nil {
				cfg.Terminal.SetMaxChannels(w.Config.MaxChannels)
			}
			if cfg.OnWelcome != nil {
				cfg.OnWelcome(w)
			}
		case protocol.TypeError:
			e, _ := protocol.DecodeError(env.Body)
			return false, fmt.Errorf("il backend ha rifiutato hello: %s", e.Code)
		default:
			return false, fmt.Errorf("messaggio %q prima di welcome: %w", env.Type, protocol.ErrInvalid)
		}
	}

	if cfg.Terminal != nil {
		q := newOutQueue(cfg.OutputHighWater, cfg.OutputLowWater, cfg.Now)
		go flushOutput(sctx, cfg, w, q)
		cfg.Terminal.Attach(&termOut{ctx: sctx, cfg: cfg, w: w, q: q})
		// Prima della chiusura della connessione (i defer girano in ordine inverso): i canali si
		// chiudono, le sessioni tmux restano. Se si ferma il connettore, il backend viene avvisato.
		defer func() {
			q.close()
			cfg.Terminal.Detach(false) // caduta: nessuno da avvisare (allo spegnimento è già fatto)
		}()
	}

	if cfg.Requests != nil {
		// Dopo welcome: le richieste ancora aperte rimandano il loro contesto (§2.12).
		cfg.Requests.Attach(&termOut{ctx: sctx, cfg: cfg, w: w})
		defer cfg.Requests.Detach()
	}

	st := newSyncState()
	sendErr := make(chan error, 1)
	if cfg.Events != nil {
		go func() {
			err := sendLoop(sctx, cfg, w, st)
			if sctx.Err() == nil {
				sendErr <- err
				conn.CloseNow()
			}
		}()
	}

	for {
		typ, data, err := conn.Read(sctx)
		if err != nil {
			select {
			case serr := <-sendErr:
				return true, fmt.Errorf("invio degli eventi: %w", serr)
			default:
			}
			return true, readErr(err, &heartbeatExpired)
		}
		lastSignal.Store(cfg.Now().UnixNano())
		if typ != websocket.MessageText {
			// Frame binari (§1.7): solo l'input dell'utente va al terminale; il resto si scarta.
			if cfg.Terminal != nil {
				if ft, ch, payload, ferr := protocol.DecodeFrame(data); ferr == nil && ft == protocol.FrameInput {
					cfg.Terminal.Input(ch, payload)
				}
			}
			continue
		}
		if err := handleMessage(sctx, cfg, w, st, data); err != nil {
			conn.Close(websocket.StatusNormalClosure, "")
			return true, err
		}
	}
}

// handleMessage elabora un messaggio dopo welcome. Un errore chiude la sessione.
func handleMessage(ctx context.Context, cfg *Config, w *writer, st *syncState, data []byte) error {
	env, err := protocol.DecodeEnvelope(data)
	if err != nil {
		reply(ctx, cfg, w, "", protocol.CodeMessageInvalid, err.Error())
		return nil
	}
	if cfg.Terminal != nil && cfg.Terminal.Handle(env) {
		return nil
	}
	if cfg.Requests != nil && cfg.Requests.Handle(env) {
		return nil
	}
	switch env.Type {
	case protocol.TypeConfigUpdate:
		var u protocol.ConfigUpdate
		if err := protocol.DecodeStrict(env.Body, &u); err != nil {
			reply(ctx, cfg, w, env.ID, protocol.CodeMessageInvalid, err.Error())
			return nil
		}
		if err := u.Config.Validate(); err != nil {
			reply(ctx, cfg, w, env.ID, protocol.CodeMessageInvalid, err.Error())
			return nil
		}
		if cfg.Terminal != nil {
			cfg.Terminal.SetMaxChannels(u.Config.MaxChannels)
		}
		if cfg.OnConfig != nil {
			cfg.OnConfig(u.Config)
		}
	case protocol.TypeEventsAck:
		a, err := protocol.DecodeEventsAck(env.Body)
		if err != nil {
			reply(ctx, cfg, w, env.ID, protocol.CodeMessageInvalid, err.Error())
			return nil
		}
		if cfg.Events != nil {
			if err := onAck(cfg, st, a.UpToSeq); err != nil {
				return fmt.Errorf("giornale: %w", err)
			}
		}
	case protocol.TypeError:
		e, _ := protocol.DecodeError(env.Body)
		cfg.Logger.Warn("errore dal backend", "codice", e.Code, "re", env.Re)
		switch e.Code {
		case protocol.CodeAckAhead:
			st.ahead.Store(true)
			st.poke()
		case protocol.CodeSeqGap, protocol.CodeTruncationIncoherent:
			return &sessionError{code: e.Code}
		}
	case protocol.TypeWelcome:
		// Un welcome ripetuto non cambia nulla.
	default:
		// Tipi non ancora implementati in questa fetta: il connettore non li ha dichiarati nelle
		// capability di hello, quindi il backend non dovrebbe mandarli.
		reply(ctx, cfg, w, env.ID, protocol.CodeTypeUnknown, "tipo non supportato da questa versione: "+env.Type)
	}
	return nil
}

func reply(ctx context.Context, cfg *Config, w *writer, re, code, msg string) {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	frame, _, err := protocol.Encode(protocol.TypeError, re, protocol.ErrorBody{Code: code, Message: msg}, cfg.Now())
	if err != nil {
		return
	}
	_ = w.write(ctx, frame)
}

func readErr(err error, heartbeat *atomic.Bool) error {
	if heartbeat.Load() {
		return errHeartbeat
	}
	switch websocket.CloseStatus(err) {
	case CloseRevoked:
		return ErrRevoked
	case CloseDisabled:
		return errDisabled
	case CloseInternal:
		return fmt.Errorf("errore interno del backend (1011), nuovo tentativo: %w", err)
	case -1:
		return err
	default:
		return fmt.Errorf("chiusura dal backend: %w", err)
	}
}

// classifyHandshake traduce il rifiuto dell'upgrade (§1.4).
func classifyHandshake(resp *http.Response, err error) error {
	if resp == nil {
		return err
	}
	var body struct {
		Code string `json:"code"`
	}
	if resp.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = json.Unmarshal(raw, &body)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized && body.Code == "HOST_REVOKED":
		return ErrRevoked
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrTokenInvalid
	case resp.StatusCode == http.StatusForbidden && body.Code == "HOST_DISABLED":
		return errDisabled
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusServiceUnavailable:
		// 429 RATE_LIMITED e 503 UNAVAILABLE sono transitori (§1.16, §1.17): si ritenta, mai prima
		// del Retry-After se il backend lo indica.
		return &retryAfterError{
			after: parseRetryAfter(resp.Header.Get("Retry-After")),
			err:   fmt.Errorf("HTTP %d %s: %w", resp.StatusCode, body.Code, err),
		}
	default:
		return fmt.Errorf("handshake rifiutato: HTTP %d %s: %w", resp.StatusCode, body.Code, err)
	}
}

// parseRetryAfter legge Retry-After in secondi (il backend non usa la forma con la data). Valori
// assenti o illeggibili valgono zero: resta il backoff.
func parseRetryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	if n > 3600 {
		n = 3600
	}
	return time.Duration(n) * time.Second
}
