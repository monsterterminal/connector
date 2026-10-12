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
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
)

// Codici di chiusura del protocollo (§1.16).
const (
	CloseRevoked  websocket.StatusCode = 4401
	CloseDisabled websocket.StatusCode = 4403
	CloseNoHello  websocket.StatusCode = 4408
	CloseReplaced websocket.StatusCode = 4409
	CloseTooMany  websocket.StatusCode = 4429
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
			cfg.Logger.Error("collegamento terminato in modo definitivo", "motivo", err.Error())
			return err
		}
		wait := cfg.Backoff.Next()
		if errors.Is(err, errDisabled) && wait < DisabledMinWait {
			wait = DisabledMinWait
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

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	frame, helloID, err := protocol.Encode(protocol.TypeHello, "", cfg.Hello(), cfg.Now())
	if err != nil {
		return false, err
	}
	if err := conn.Write(sctx, websocket.MessageText, frame); err != nil {
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

	for {
		typ, data, err := conn.Read(sctx)
		if err != nil {
			return true, readErr(err, &heartbeatExpired)
		}
		lastSignal.Store(cfg.Now().UnixNano())
		if typ != websocket.MessageText {
			// Nessun canale del terminale in questa fetta: i frame binari si scartano (§1.7).
			continue
		}
		handleMessage(sctx, cfg, conn, data)
	}
}

func handleMessage(ctx context.Context, cfg *Config, conn *websocket.Conn, data []byte) {
	env, err := protocol.DecodeEnvelope(data)
	if err != nil {
		reply(ctx, cfg, conn, "", protocol.CodeMessageInvalid, err.Error())
		return
	}
	switch env.Type {
	case protocol.TypeConfigUpdate:
		var u protocol.ConfigUpdate
		if err := protocol.DecodeStrict(env.Body, &u); err != nil {
			reply(ctx, cfg, conn, env.ID, protocol.CodeMessageInvalid, err.Error())
			return
		}
		if err := u.Config.Validate(); err != nil {
			reply(ctx, cfg, conn, env.ID, protocol.CodeMessageInvalid, err.Error())
			return
		}
		if cfg.OnConfig != nil {
			cfg.OnConfig(u.Config)
		}
	case protocol.TypeError:
		e, _ := protocol.DecodeError(env.Body)
		cfg.Logger.Warn("errore dal backend", "codice", e.Code, "re", env.Re)
	case protocol.TypeWelcome:
		// Un welcome ripetuto non cambia nulla.
	default:
		// Tipi non ancora implementati in questa fetta: il connettore non li ha dichiarati nelle
		// capability di hello, quindi il backend non dovrebbe mandarli.
		reply(ctx, cfg, conn, env.ID, protocol.CodeTypeUnknown, "tipo non supportato da questa versione: "+env.Type)
	}
}

func reply(ctx context.Context, cfg *Config, conn *websocket.Conn, re, code, msg string) {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	frame, _, err := protocol.Encode(protocol.TypeError, re, protocol.ErrorBody{Code: code, Message: msg}, cfg.Now())
	if err != nil {
		return
	}
	_ = conn.Write(ctx, websocket.MessageText, frame)
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
	default:
		return fmt.Errorf("handshake rifiutato: HTTP %d %s: %w", resp.StatusCode, body.Code, err)
	}
}
