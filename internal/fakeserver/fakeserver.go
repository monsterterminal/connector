// Package fakeserver è un backend finto che parla il protocollo 0.1.0, SOLO per i test.
//
// Segue il gateway vero (monsterapp, ADR 0409, verbale 2026-10-13) dove il connettore se ne
// accorge: registrazione col token di installazione monouso, NON consumato se il corpo non è
// valido o la macchina è ferma (§1.3); sostituzione solo col token attuale nell'header
// X-MonsterTerminal-Host-Token, stessa macchina (§1.3); kill-switch per macchina, 403 HOST_DISABLED
// anche alla registrazione; autenticazione dell'upgrade (§1.4), hello/welcome, ping (§1.2), una
// connessione per host (4409), revoca (4401), kill-switch (4403); giornale col segnalibro che fa
// fede (§1.12: ACK_AHEAD, SEQ_GAP, TRUNCATION_INCOHERENT, troncamento coerente); 50 messaggi al
// secondo per host (§1.17, 4429); rifiuti dell'upgrade 429 con Retry-After e 503; chiusura 1011.
// Non va mai importato dal codice di produzione.
package fakeserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/register"
)

// HeaderCurrentHostToken è l'header con cui il connettore prova il possesso dell'host da sostituire.
const HeaderCurrentHostToken = "X-MonsterTerminal-Host-Token"

// HeaderSchema è l'header con cui il connettore dichiara la versione degli schemi che parla (dalla
// 0.4.0, ADR 0418 D5): il server finto lo registra per ogni registrazione e non lo giudica.
const HeaderSchema = "X-MonsterTerminal-Schema"

// RateLimit è il limite di messaggi al secondo per host del backend (§1.17).
const RateLimit = 50

type host struct {
	id        string
	machineID string
	revoked   bool
	disabled  bool
	conn      *websocket.Conn
	bookmark  int64
	events    []protocol.Event
	hits      []time.Time
	// terminale (§1.7)
	output   map[uint32][]byte
	frames   []int // dimensione di ogni frame di output ricevuto
	replies  map[string]protocol.Envelope
	closed   []protocol.SessionClosed
	contexts []protocol.RequestContext
}

// Rejection è un rifiuto dell'upgrade preparato da un test.
type Rejection struct {
	Status     int
	Code       string
	RetryAfter string // header Retry-After, vuoto = assente
}

// Server è il backend finto.
type Server struct {
	// TLS è il server di prova avviato da New; è nil per un server nato con NewDetached.
	TLS *httptest.Server
	mux *http.ServeMux

	// Notify, se impostata, è chiamata fuori dal lock a ogni evento osservabile ("register",
	// "upgrade", "connect", "hello"): il banco la usa per riscrivere lo stato senza fare polling.
	Notify func(event string)

	// PingInterval è l'intervallo dei ping dell'heartbeat; zero = nessun ping.
	PingInterval time.Duration
	// HelloTimeout: senza hello entro questo tempo il server chiude con 4408.
	HelloTimeout time.Duration
	// ExtraRegisterField aggiunge un campo sconosciuto alla risposta di registrazione.
	ExtraRegisterField bool
	// MinConnectorVersion, se impostata, rifiuta con 426 CONNECTOR_TOO_OLD le registrazioni sotto
	// questa versione secondo CompareVersion (§1.15); il token non si consuma.
	MinConnectorVersion string
	CompareVersion      func(a, b string) int
	// MaxChannels è maxChannels della configurazione mandata in welcome; zero = 12.
	MaxChannels int
	// AckDelay rallenta l'elaborazione di ogni evento accettato prima dell'ack, come un DB lento:
	// gli eventi successivi restano in volo.
	AckDelay time.Duration

	mu              sync.Mutex
	installTokens   map[string]time.Time // token -> scadenza
	hosts           map[string]*host     // sha256(token dell'host) -> host
	disabledMachine map[string]bool
	registrations   []register.Info
	replaceHeaders  []string
	schemaHeaders   []string
	hellos          chan protocol.Hello
	connects        int
	upgrades        int
	silent          bool
	rejections      []Rejection
	closeAfterEvent map[int64]websocket.StatusCode
}

// New avvia il server su TLS con certificato autofirmato (httptest): è la forma dei test.
func New() *Server {
	s := NewDetached()
	s.TLS = httptest.NewTLSServer(s.Handler())
	return s
}

// NewDetached costruisce il server senza ascoltare: chi lo usa lo monta con Handler o Serve su un
// listener proprio (il banco di prova in packaging/banco, con il certificato della sua CA). BaseURL,
// WSURL e Client valgono solo per un server nato con New.
func NewDetached() *Server {
	s := &Server{
		installTokens:   map[string]time.Time{},
		hosts:           map[string]*host{},
		disabledMachine: map[string]bool{},
		closeAfterEvent: map[int64]websocket.StatusCode{},
		hellos:          make(chan protocol.Hello, 64),
		HelloTimeout:    10 * time.Second,
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("POST /v1/register", s.handleRegister)
	s.mux.HandleFunc("GET /v1", s.handleWS)
	return s
}

// Handler è il backend finto come http.Handler: POST /v1/register e l'upgrade GET /v1.
func (s *Server) Handler() http.Handler { return s.mux }

// Serve serve il backend finto sul listener dato (già TLS, se serve) finché il listener non si
// chiude; restituisce l'errore di http.Server.Serve, http.ErrServerClosed compreso.
func (s *Server) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
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
	if s.TLS != nil {
		s.TLS.Close()
	}
}

func (s *Server) notify(event string) {
	if s.Notify != nil {
		s.Notify(event)
	}
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

// InstallTokenUsable dice se il token di installazione è ancora utilizzabile (non consumato).
func (s *Server) InstallTokenUsable(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.installTokens[tok]
	return ok && time.Now().Before(exp)
}

// Registrations restituisce i corpi di registrazione accettati.
func (s *Server) Registrations() []register.Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]register.Info(nil), s.registrations...)
}

// ReplaceHeaders restituisce l'header della sostituzione di ogni registrazione ("" se assente).
func (s *Server) ReplaceHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.replaceHeaders...)
}

// SchemaHeaders restituisce l'header X-MonsterTerminal-Schema di ogni registrazione ("" se assente).
func (s *Server) SchemaHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.schemaHeaders...)
}

// Hellos riceve ogni hello valido. Il canale ha 64 posti: chi tiene il server a lungo lo svuota.
func (s *Server) Hellos() <-chan protocol.Hello { return s.hellos }

// Connects conta gli upgrade autenticati.
func (s *Server) Connects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connects
}

// Upgrades conta i tentativi di upgrade, rifiutati compresi.
func (s *Server) Upgrades() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upgrades
}

// AddHost crea un host già registrato e ne restituisce il token.
func (s *Server) AddHost() (hostID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addHostLocked("")
}

func (s *Server) addHostLocked(machineID string) (string, string) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	token := "mth_" + base64.RawURLEncoding.EncodeToString(b[:])
	id := protocol.NewUUID()
	s.hosts[hashToken(token)] = &host{id: id, machineID: machineID, output: map[uint32][]byte{}, replies: map[string]protocol.Envelope{}}
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

// HostRevoked dice se l'host è revocato.
func (s *Server) HostRevoked(hostID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.byID(hostID)
	return h != nil && h.revoked
}

// Bookmark è il segnalibro del backend per l'host: l'ultimo seq accettato.
func (s *Server) Bookmark(hostID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.byID(hostID); h != nil {
		return h.bookmark
	}
	return -1
}

// SetBookmark impone il segnalibro (per simulare un backend indietro, ACK_AHEAD).
func (s *Server) SetBookmark(hostID string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.byID(hostID); h != nil {
		h.bookmark = n
	}
}

// Events restituisce gli eventi accettati per l'host, in ordine.
func (s *Server) Events(hostID string) []protocol.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.byID(hostID); h != nil {
		return append([]protocol.Event(nil), h.events...)
	}
	return nil
}

// RejectUpgrades fa rifiutare i prossimi tentativi di upgrade, uno per elemento, nell'ordine.
func (s *Server) RejectUpgrades(r ...Rejection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejections = append(s.rejections, r...)
}

// CloseAfterEvent chiude la connessione con code dopo aver ACCETTATO l'evento seq, prima dell'ack:
// l'evento è nel segnalibro ma il connettore non lo sa (caduta fra scrittura e conferma).
func (s *Server) CloseAfterEvent(seq int64, code websocket.StatusCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeAfterEvent[seq] = code
}

// DisableMachine ferma la macchina col kill-switch: chiude i suoi host con 4403 e rifiuta anche le
// registrazioni da quella macchina con 403 HOST_DISABLED.
func (s *Server) DisableMachine(machineID string) {
	s.mu.Lock()
	s.disabledMachine[machineID] = true
	var ids []string
	for _, h := range s.hosts {
		if h.machineID == machineID {
			ids = append(ids, h.id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.Disable(id)
	}
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

// CloseWith chiude la connessione dell'host con un codice qualsiasi (1011, 4429...).
func (s *Server) CloseWith(hostID string, code websocket.StatusCode) {
	s.closeWith(hostID, code, func(*host) {})
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
	// Il token prima del corpo, e NON si consuma finché la registrazione non riesce.
	if !s.InstallTokenUsable(tok) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "INSTALL_TOKEN_INVALID"})
		return
	}
	var info register.Info
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&info); err != nil || info.MachineID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BODY_INVALID"})
		return
	}
	if s.MinConnectorVersion != "" && s.CompareVersion != nil && s.CompareVersion(info.ConnectorVersion, s.MinConnectorVersion) < 0 {
		writeJSON(w, http.StatusUpgradeRequired, map[string]string{"code": "CONNECTOR_TOO_OLD"})
		return
	}
	current := r.Header.Get(HeaderCurrentHostToken)
	s.mu.Lock()
	if s.disabledMachine[info.MachineID] {
		s.mu.Unlock()
		writeJSON(w, http.StatusForbidden, map[string]string{"code": "HOST_DISABLED"})
		return
	}
	if !s.consumeLocked(tok) {
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "INSTALL_TOKEN_INVALID"})
		return
	}
	s.registrations = append(s.registrations, info)
	s.replaceHeaders = append(s.replaceHeaders, current)
	s.schemaHeaders = append(s.schemaHeaders, r.Header.Get(HeaderSchema))
	// Sostituzione (§1.3): solo col token attuale di un host vivo della stessa macchina. Senza
	// prova, o con una prova che non torna, l'host nuovo nasce accanto al vecchio.
	var replaced *host
	if current != "" {
		if old := s.hosts[hashToken(current)]; old != nil && !old.revoked && old.machineID == info.MachineID {
			old.revoked = true
			replaced = old
		}
	}
	id, token := s.addHostLocked(info.MachineID)
	extra := s.ExtraRegisterField
	var oldConn *websocket.Conn
	if replaced != nil {
		oldConn = replaced.conn
	}
	s.mu.Unlock()
	if oldConn != nil {
		_ = oldConn.Close(4401, "")
	}
	s.notify("register")
	resp := map[string]any{"hostId": id, "hostToken": token, "tenantLabel": "Acme", "protocol": protocol.Version}
	if extra {
		resp["tenantId"] = "da-non-accettare"
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) consumeLocked(tok string) bool {
	exp, ok := s.installTokens[tok]
	if !ok || time.Now().After(exp) {
		return false
	}
	delete(s.installTokens, tok)
	return true
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.notify("upgrade")
	s.mu.Lock()
	s.upgrades++
	if len(s.rejections) > 0 {
		rej := s.rejections[0]
		s.rejections = s.rejections[1:]
		s.mu.Unlock()
		if rej.RetryAfter != "" {
			w.Header().Set("Retry-After", rej.RetryAfter)
		}
		writeJSON(w, rej.Status, map[string]string{"code": rej.Code})
		return
	}
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
	s.notify("connect")
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
	if !s.hit(h) {
		_ = conn.Close(4429, "")
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
	s.notify("hello")

	welcome := protocol.Welcome{
		HostID: h.id, Protocol: protocol.Version, ServerTime: protocol.Timestamp(time.Now()),
		Config: protocol.HostConfig{DefaultIdleThresholdSeconds: 300, SessionIdleThresholds: map[string]int{}, MaxChannels: s.maxChannels()},
	}
	send := func(typ, re string, body any) error {
		frame, _, _ := protocol.Encode(typ, re, body, time.Now())
		return conn.Write(ctx, websocket.MessageText, frame)
	}
	if err := send(protocol.TypeWelcome, env.ID, welcome); err != nil {
		return
	}
	// Fa fede il backend (§1.12), come onHello del gateway: ACK_AHEAD se il connettore è avanti,
	// events.ack col segnalibro se differisce e non è zero.
	s.mu.Lock()
	bookmark := h.bookmark
	s.mu.Unlock()
	if hello.LastAckedSeq > bookmark {
		_ = send(protocol.TypeError, env.ID, protocol.ErrorBody{Code: protocol.CodeAckAhead, Message: "lastAckedSeq oltre il segnalibro del backend"})
	}
	if hello.LastAckedSeq != bookmark && bookmark >= 1 {
		_ = send(protocol.TypeEventsAck, "", protocol.EventsAck{UpToSeq: bookmark})
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
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if !s.hit(h) {
			_ = conn.Close(4429, "")
			return
		}
		if typ != websocket.MessageText {
			if ft, ch, payload, ferr := protocol.DecodeFrame(data); ferr == nil && ft == protocol.FrameOutput {
				s.mu.Lock()
				h.output[ch] = append(h.output[ch], payload...)
				h.frames = append(h.frames, len(data))
				s.mu.Unlock()
			}
			continue
		}
		env, err := protocol.DecodeEnvelope(data)
		if err != nil {
			continue
		}
		if env.Type != protocol.TypeEvent {
			s.mu.Lock()
			if env.Re != "" {
				h.replies[env.Re] = env
			}
			if env.Type == protocol.TypeSessionClosed {
				var c protocol.SessionClosed
				if json.Unmarshal(env.Body, &c) == nil {
					h.closed = append(h.closed, c)
				}
			}
			if env.Type == protocol.TypeRequestContext {
				var c protocol.RequestContext
				if json.Unmarshal(env.Body, &c) == nil {
					h.contexts = append(h.contexts, c)
				}
			}
			s.mu.Unlock()
			continue
		}
		var ev protocol.Event
		if err := protocol.DecodeStrict(env.Body, &ev); err != nil {
			_ = send(protocol.TypeError, env.ID, protocol.ErrorBody{Code: protocol.CodeMessageInvalid, Message: err.Error()})
			continue
		}
		code, upTo := s.ingest(h, ev)
		if code != "" {
			_ = send(protocol.TypeError, env.ID, protocol.ErrorBody{Code: code, Message: "evento rifiutato"})
			continue
		}
		s.mu.Lock()
		closeCode, closeNow := s.closeAfterEvent[ev.Seq]
		delete(s.closeAfterEvent, ev.Seq)
		s.mu.Unlock()
		if closeNow {
			_ = conn.Close(closeCode, "")
			return
		}
		if s.AckDelay > 0 {
			time.Sleep(s.AckDelay)
		}
		if upTo >= 1 {
			_ = send(protocol.TypeEventsAck, env.ID, protocol.EventsAck{UpToSeq: upTo})
		}
	}
}

func (s *Server) maxChannels() int {
	if s.MaxChannels > 0 {
		return s.MaxChannels
	}
	return 12
}

func (s *Server) conn(hostID string) *websocket.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.byID(hostID); h != nil {
		return h.conn
	}
	return nil
}

// SendControl manda un messaggio di controllo all'host collegato e ne restituisce l'id.
func (s *Server) SendControl(hostID, typ string, body any) (string, error) {
	c := s.conn(hostID)
	if c == nil {
		return "", errors.New("host non collegato")
	}
	frame, id, err := protocol.Encode(typ, "", body, time.Now())
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return id, c.Write(ctx, websocket.MessageText, frame)
}

// SendInput manda un frame di input (0x02) al canale.
func (s *Server) SendInput(hostID string, ch uint32, data []byte) error {
	c := s.conn(hostID)
	if c == nil {
		return errors.New("host non collegato")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Write(ctx, websocket.MessageBinary, protocol.EncodeFrame(protocol.FrameInput, ch, data))
}

// Contexts sono i request.context ricevuti dall'host (§2.9 punto 2), nell'ordine di arrivo.
func (s *Server) Contexts(hostID string) []protocol.RequestContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.byID(hostID); h != nil {
		return append([]protocol.RequestContext(nil), h.contexts...)
	}
	return nil
}

// Reply restituisce la risposta del connettore al messaggio id, se è arrivata.
func (s *Server) Reply(hostID, id string) (protocol.Envelope, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.byID(hostID); h != nil {
		e, ok := h.replies[id]
		return e, ok
	}
	return protocol.Envelope{}, false
}

// Output è l'output ricevuto finora sul canale.
func (s *Server) Output(hostID string, ch uint32) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.byID(hostID); h != nil {
		return append([]byte(nil), h.output[ch]...)
	}
	return nil
}

// OutputFrames sono le dimensioni dei frame di output ricevuti, intestazione compresa.
func (s *Server) OutputFrames(hostID string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.byID(hostID); h != nil {
		return append([]int(nil), h.frames...)
	}
	return nil
}

// Closed sono i session.closed ricevuti dall'host.
func (s *Server) Closed(hostID string) []protocol.SessionClosed {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.byID(hostID); h != nil {
		return append([]protocol.SessionClosed(nil), h.closed...)
	}
	return nil
}

// hit applica il limite di 50 messaggi al secondo in finestra scorrevole (§1.17).
func (s *Server) hit(h *host) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cut := now.Add(-time.Second)
	i := 0
	for i < len(h.hits) && h.hits[i].Before(cut) {
		i++
	}
	h.hits = append(h.hits[i:], now)
	return len(h.hits) <= RateLimit
}

// ingest è classifyEvent del backend: codice di rifiuto ("" = accettato o duplicato) e segnalibro.
func (s *Server) ingest(h *host, ev protocol.Event) (string, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := h.bookmark
	if ev.Seq <= last {
		return "", last
	}
	if ev.Kind == protocol.KindJournalTruncated {
		var d protocol.JournalTruncated
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return protocol.CodeTruncationIncoherent, last
		}
		o, err1 := time.Parse(time.RFC3339Nano, d.OldestDroppedAt)
		n, err2 := time.Parse(time.RFC3339Nano, d.NewestDroppedAt)
		if err1 != nil || err2 != nil || d.DroppedCount != ev.Seq-last || d.FirstKeptSeq != ev.Seq+1 || o.After(n) {
			return protocol.CodeTruncationIncoherent, last
		}
	} else if ev.Seq != last+1 {
		return protocol.CodeSeqGap, last
	}
	h.bookmark = ev.Seq
	h.events = append(h.events, ev)
	return "", ev.Seq
}

// RetryAfter formatta i secondi per Rejection.RetryAfter.
func RetryAfter(d time.Duration) string { return strconv.Itoa(int(d / time.Second)) }
