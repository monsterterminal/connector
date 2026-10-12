// Package fakeserver è un backend finto che parla il protocollo 0.1.0, SOLO per i test.
//
// Implementa la registrazione (§1.3), l'autenticazione dell'upgrade (§1.4), hello/welcome, i ping
// dell'heartbeat (§1.2), una connessione per host (4409), la revoca (4401) e il kill-switch (4403).
// Non va mai importato dal codice di produzione.
package fakeserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
	"github.com/ciarasino/monsterterminal-connector/internal/register"
)

type host struct {
	id       string
	revoked  bool
	disabled bool
	conn     *websocket.Conn
}

// Server è il backend finto.
type Server struct {
	TLS *httptest.Server

	// PingInterval è l'intervallo dei ping dell'heartbeat; zero = nessun ping.
	PingInterval time.Duration
	// HelloTimeout: senza hello entro questo tempo il server chiude con 4408.
	HelloTimeout time.Duration
	// ExtraRegisterField aggiunge un campo sconosciuto alla risposta di registrazione.
	ExtraRegisterField bool

	mu            sync.Mutex
	installTokens map[string]time.Time // token -> scadenza
	hosts         map[string]*host     // sha256(token dell'host) -> host
	registrations []register.Info
	hellos        chan protocol.Hello
	connects      int
	silent        bool
}

// New avvia il server su TLS con certificato autofirmato.
func New() *Server {
	s := &Server{
		installTokens: map[string]time.Time{},
		hosts:         map[string]*host{},
		hellos:        make(chan protocol.Hello, 64),
		HelloTimeout:  10 * time.Second,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/register", s.handleRegister)
	mux.HandleFunc("GET /v1", s.handleWS)
	s.TLS = httptest.NewTLSServer(mux)
	return s
}

// Close spegne il server e chiude di colpo le connessioni WebSocket aperte, come una caduta.
func (s *Server) Close() {
	s.mu.Lock()
	var open []*websocket.Conn
	for _, h := range s.hosts {
		if h.conn != nil {
			open = append(open, h.conn)
		}
	}
	s.mu.Unlock()
	for _, c := range open {
		c.CloseNow()
	}
	s.TLS.Close()
}

// BaseURL è l'origine https del server.
func (s *Server) BaseURL() string { return s.TLS.URL }

// WSURL è l'indirizzo wss del collegamento.
func (s *Server) WSURL() string { return "wss://" + strings.TrimPrefix(s.TLS.URL, "https://") + "/v1" }

// Client è un client HTTP che si fida SOLO del certificato di questo server.
func (s *Server) Client() *http.Client { return s.TLS.Client() }

// AddInstallToken registra un token di installazione valido per ttl.
func (s *Server) AddInstallToken(tok string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installTokens[tok] = time.Now().Add(ttl)
}

// Registrations restituisce i corpi di registrazione ricevuti.
func (s *Server) Registrations() []register.Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]register.Info(nil), s.registrations...)
}

// Hellos riceve ogni hello valido.
func (s *Server) Hellos() <-chan protocol.Hello { return s.hellos }

// Connects conta gli upgrade autenticati.
func (s *Server) Connects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connects
}

// AddHost crea un host già registrato e ne restituisce il token.
func (s *Server) AddHost() (hostID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addHostLocked()
}

func (s *Server) addHostLocked() (string, string) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	token := "mth_" + base64.RawURLEncoding.EncodeToString(b[:])
	id := protocol.NewUUID()
	s.hosts[hashToken(token)] = &host{id: id}
	return id, token
}

func hashToken(t string) string { h := sha256.Sum256([]byte(t)); return hex.EncodeToString(h[:]) }

func (s *Server) byID(id string) *host {
	for _, h := range s.hosts {
		if h.id == id {
			return h
		}
	}
	return nil
}

// Drop chiude di colpo la connessione dell'host, senza codice: simula una caduta di rete.
func (s *Server) Drop(hostID string) {
	s.mu.Lock()
	h := s.byID(hostID)
	var c *websocket.Conn
	if h != nil {
		c = h.conn
	}
	s.mu.Unlock()
	if c != nil {
		c.CloseNow()
	}
}

// Revoke revoca l'host: chiude con 4401 e rifiuta i tentativi successivi con 401 HOST_REVOKED.
func (s *Server) Revoke(hostID string) { s.closeWith(hostID, 4401, func(h *host) { h.revoked = true }) }

// Disable ferma l'host col kill-switch: chiude con 4403 e rifiuta con 403 HOST_DISABLED.
func (s *Server) Disable(hostID string) {
	s.closeWith(hostID, 4403, func(h *host) { h.disabled = true })
}

// Silence smette di mandare ping e messaggi: simula un backend muto.
func (s *Server) Silence() { s.mu.Lock(); s.silent = true; s.mu.Unlock() }

func (s *Server) closeWith(hostID string, code websocket.StatusCode, mark func(*host)) {
	s.mu.Lock()
	h := s.byID(hostID)
	var c *websocket.Conn
	if h != nil {
		mark(h)
		c = h.conn
	}
	s.mu.Unlock()
	if c != nil {
		_ = c.Close(code, "")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	exp, ok := s.installTokens[tok]
	if ok {
		delete(s.installTokens, tok) // monouso, consumato anche se il corpo è sbagliato
	}
	s.mu.Unlock()
	if !ok || time.Now().After(exp) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "INSTALL_TOKEN_INVALID"})
		return
	}
	var info register.Info
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&info); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BODY_INVALID"})
		return
	}
	s.mu.Lock()
	s.registrations = append(s.registrations, info)
	id, token := s.addHostLocked()
	extra := s.ExtraRegisterField
	s.mu.Unlock()
	resp := map[string]any{"hostId": id, "hostToken": token, "tenantLabel": "Acme", "protocol": protocol.Version}
	if extra {
		resp["tenantId"] = "da-non-accettare"
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	h := s.hosts[hashToken(tok)]
	switch {
	case h == nil:
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "HOST_TOKEN_INVALID"})
		return
	case h.revoked:
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "HOST_REVOKED"})
		return
	case h.disabled:
		s.mu.Unlock()
		writeJSON(w, http.StatusForbidden, map[string]string{"code": "HOST_DISABLED"})
		return
	}
	s.mu.Unlock()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxFrameBytes)

	s.mu.Lock()
	s.connects++
	old := h.conn
	h.conn = conn
	s.mu.Unlock()
	if old != nil {
		_ = old.Close(4409, "")
	}
	defer func() {
		s.mu.Lock()
		if h.conn == conn {
			h.conn = nil
		}
		s.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	hctx, hcancel := context.WithTimeout(ctx, s.HelloTimeout)
	typ, data, err := conn.Read(hctx)
	hcancel()
	if err != nil {
		_ = conn.Close(4408, "")
		return
	}
	env, err := protocol.DecodeEnvelope(data)
	var hello protocol.Hello
	if err == nil && typ == websocket.MessageText && env.Type == protocol.TypeHello {
		err = protocol.DecodeStrict(env.Body, &hello)
	} else if err == nil {
		err = protocol.ErrInvalid
	}
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "hello non valido")
		return
	}
	s.hellos <- hello

	welcome := protocol.Welcome{
		HostID: h.id, Protocol: protocol.Version, ServerTime: protocol.Timestamp(time.Now()),
		Config: protocol.HostConfig{DefaultIdleThresholdSeconds: 300, SessionIdleThresholds: map[string]int{}, MaxChannels: 12},
	}
	frame, _, _ := protocol.Encode(protocol.TypeWelcome, env.ID, welcome, time.Now())
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		return
	}

	if s.PingInterval > 0 {
		go func() {
			t := time.NewTicker(s.PingInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					s.mu.Lock()
					silent := s.silent
					s.mu.Unlock()
					if silent {
						continue
					}
					pctx, pc := context.WithTimeout(ctx, s.PingInterval)
					_ = conn.Ping(pctx)
					pc()
				}
			}
		}()
	}
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}
