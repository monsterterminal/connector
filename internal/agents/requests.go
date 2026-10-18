package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

// Le richieste dell'agente (spec §2.9, §2.12; ADR 0410). Un hook PermissionRequest di Claude Code
// apre una richiesta: il connettore manda il contesto al backend fuori dal giornale (request.context),
// scrive attende-input/permesso col requestId nel giornale, e tiene l'hook in attesa per la finestra
// adattiva. Chi risponde prima vince: il prompt nel terminale (che Claude Code mostra in parallelo,
// §1.21) o la card (request.respond). L'hook riceve una riga Decision; una riga vuota = nessuna
// decisione, il prompt resta.

// Out è la connessione vista dal gestore: solo messaggi di controllo.
type Out interface {
	Control(typ, re string, body any) error
}

// SessionProbe dice se un client è collegato alla sessione tmux e quando è stata l'ultima attività.
type SessionProbe func(session string) (attached bool, activity time.Time, ok bool)

// RequestsConfig configura il gestore.
type RequestsConfig struct {
	// ShortWindow vale con un client collegato e attivo; LongWindow altrimenti (decisione BOSS
	// 2026-10-17: 2 e 60 minuti).
	ShortWindow, LongWindow time.Duration
	// ActiveWithin: da quanto l'ultima attività deve essere recente perché il client conti come attivo.
	ActiveWithin time.Duration
	// Probe legge la sessione tmux; nil = tmux dal PATH sul socket Socket.
	Probe  SessionProbe
	Tmux   string
	Socket string
	Now    func() time.Time
	Logger *slog.Logger
}

type request struct {
	id       string
	session  string
	deadline time.Time
	conn     net.Conn
	ctx      protocol.RequestContext
	decision chan Decision // bufferizzato: la risposta dalla card
	answered bool
	closed   bool
}

// Requests tiene le richieste aperte dell'host.
type Requests struct {
	cfg RequestsConfig
	tr  *Tracker

	mu        sync.Mutex
	out       Out
	byID      map[string]*request
	bySession map[string]*request
}

// NewRequests crea il gestore e lo aggancia al tracker: riceve gli hook di permesso e osserva le
// transizioni per chiudere da sé le richieste superate.
func NewRequests(cfg RequestsConfig, tr *Tracker) *Requests {
	if cfg.ShortWindow == 0 {
		cfg.ShortWindow = 2 * time.Minute
	}
	if cfg.LongWindow == 0 {
		cfg.LongWindow = 60 * time.Minute
	}
	if cfg.ActiveWithin == 0 {
		cfg.ActiveWithin = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Probe == nil {
		cfg.Probe = tmuxProbe(cfg.Tmux, cfg.Socket)
	}
	r := &Requests{cfg: cfg, tr: tr, byID: map[string]*request{}, bySession: map[string]*request{}}
	tr.mu.Lock()
	tr.Permission = r
	tr.mu.Unlock()
	tr.OnTransition(r.onTransition)
	return r
}

// Attach collega il gestore a una connessione nuova e rimanda il contesto delle richieste ancora
// aperte (§2.12: request.context non ha conferma né giornale).
func (r *Requests) Attach(out Out) {
	r.mu.Lock()
	r.out = out
	open := make([]protocol.RequestContext, 0, len(r.byID))
	for _, q := range r.byID {
		if !q.closed {
			open = append(open, q.ctx)
		}
	}
	r.mu.Unlock()
	for _, c := range open {
		_ = out.Control(protocol.TypeRequestContext, "", c)
	}
}

// Detach scollega il gestore: le richieste restano aperte, gli hook continuano ad aspettare.
func (r *Requests) Detach() {
	r.mu.Lock()
	r.out = nil
	r.mu.Unlock()
}

// Handle tratta request.respond (§2.9 punto 3) e risponde con request.respond.result.
func (r *Requests) Handle(env protocol.Envelope) bool {
	if env.Type != protocol.TypeRequestRespond {
		return false
	}
	body, err := protocol.DecodeRequestRespond(env.Body)
	if err != nil {
		msg := err.Error()
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		r.send(protocol.TypeError, env.ID, protocol.ErrorBody{Code: protocol.CodeMessageInvalid, Message: msg})
		return true
	}
	outcome := r.respond(body)
	r.send(protocol.TypeRequestRespondResult, env.ID, protocol.RequestRespondResult{RequestID: body.RequestID, Outcome: outcome})
	return true
}

// Window è la finestra adattiva della sessione (§2.12).
func (r *Requests) Window(session string) time.Duration {
	attached, activity, ok := r.cfg.Probe(session)
	if ok && attached && r.cfg.Now().Sub(activity) < r.cfg.ActiveWithin {
		return r.cfg.ShortWindow
	}
	return r.cfg.LongWindow
}

// Open conta le richieste aperte (per i test e i log).
func (r *Requests) Open() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

// HandlePermission prende in carico un hook di permesso: apre la richiesta e tiene la connessione
// finché arriva una decisione o scade la finestra. Gira nella propria goroutine.
func (r *Requests) HandlePermission(m HookMessage, c net.Conn) {
	defer c.Close()
	if !protocol.IsSessionName(m.Session) || m.Action == "" {
		return
	}
	detail, truncated := protocol.TruncateDetail(m.Detail)
	action := m.Action
	if len(action) > protocol.MaxRequestAction {
		action = action[:protocol.MaxRequestAction]
	}
	now := r.cfg.Now()
	q := &request{
		id: protocol.NewUUID(), session: m.Session, conn: c, decision: make(chan Decision, 1),
		deadline: now.Add(r.Window(m.Session)),
	}
	q.ctx = protocol.RequestContext{
		RequestID: q.id, Session: m.Session, Agent: protocol.AgentClaude, Kind: protocol.KindPermission,
		Action: action, Detail: detail, Deadline: protocol.Deadline(q.deadline),
	}
	r.mu.Lock()
	if prev := r.bySession[m.Session]; prev != nil {
		r.closeLocked(prev) // una sola richiesta aperta per sessione (§2.12)
	}
	r.byID[q.id] = q
	r.bySession[m.Session] = q
	out := r.out
	r.mu.Unlock()
	if truncated || m.DetailTruncated {
		r.cfg.Logger.Info("contesto della richiesta troncato", "sessione", m.Session)
	}
	if out != nil {
		_ = out.Control(protocol.TypeRequestContext, "", q.ctx)
	}
	if _, err := r.tr.ApplyRequest(m.Session, protocol.AgentClaude, protocol.StateWaiting, protocol.SourceHook, protocol.WaitPermission, q.id); err != nil {
		r.cfg.Logger.Warn("stato dell'agente non registrato", "errore", err.Error())
	}
	r.cfg.Logger.Info("richiesta aperta", "sessione", m.Session, "requestId", q.id, "finestra", q.deadline.Sub(now).String())

	timer := time.NewTimer(q.deadline.Sub(now))
	defer timer.Stop()
	var d Decision
	select {
	case d = <-q.decision:
	case <-timer.C:
		r.cfg.Logger.Info("finestra scaduta, il prompt resta nel terminale", "requestId", q.id)
	}
	r.mu.Lock()
	r.forgetLocked(q)
	r.mu.Unlock()
	writeDecision(c, d)
}

// respond consegna una risposta dalla card: consegnata se l'hook è ancora in attesa, superata se la
// richiesta non è aperta (sconosciuta, scaduta, già risposta o superata da un cambio di stato).
func (r *Requests) respond(b protocol.RequestRespond) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.byID[b.RequestID]
	if q == nil || q.closed || q.answered {
		return protocol.OutcomeSuperseded
	}
	q.answered = true
	d := Decision{}
	switch b.Answer {
	case protocol.AnswerApprove:
		d.Behavior = "allow"
	case protocol.AnswerDeny:
		d = Decision{Behavior: "deny", Message: "Negato dalla card di MonsterTerminal."}
	case protocol.AnswerText:
		// Il testo arriva a Claude Code come motivo del rifiuto: lo legge come indicazione dell'utente
		// senza che nessuno scriva nel terminale (§2.12).
		d = Decision{Behavior: "deny", Message: b.Text}
	}
	q.decision <- d
	return protocol.OutcomeDelivered
}

// onTransition chiude come superata la richiesta aperta di una sessione il cui stato non è più
// l'attesa di quel permesso (risposta data nel terminale: PostToolUse → lavora, Stop, SessionEnd).
func (r *Requests) onTransition(tr Transition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.bySession[tr.Session]
	if q == nil || q.closed || q.answered {
		return
	}
	if tr.State == protocol.StateWaiting && tr.RequestID == q.id {
		return
	}
	r.cfg.Logger.Info("richiesta superata dal cambio di stato", "requestId", q.id, "stato", tr.State)
	r.closeLocked(q)
}

// closeLocked chiude una richiesta senza decisione: l'hook esce e il prompt resta (o è già risolto).
func (r *Requests) closeLocked(q *request) {
	if q.closed {
		return
	}
	q.closed = true
	select {
	case q.decision <- Decision{}:
	default:
	}
	r.forgetLocked(q)
}

func (r *Requests) forgetLocked(q *request) {
	q.closed = true
	delete(r.byID, q.id)
	if r.bySession[q.session] == q {
		delete(r.bySession, q.session)
	}
}

func (r *Requests) send(typ, re string, body any) {
	r.mu.Lock()
	out := r.out
	r.mu.Unlock()
	if out != nil {
		_ = out.Control(typ, re, body)
	}
}

func writeDecision(c net.Conn, d Decision) {
	raw, err := json.Marshal(d)
	if err != nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.Write(append(raw, '\n'))
}

// tmuxProbe legge session_attached e session_activity della sessione dal server tmux.
func tmuxProbe(tmux, socket string) SessionProbe {
	if tmux == "" {
		tmux = "tmux"
	}
	return func(session string) (bool, time.Time, bool) {
		if !protocol.IsSessionName(session) {
			return false, time.Time{}, false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		args := []string{"display-message", "-p", "-t", session + ":", "#{session_attached}\t#{session_activity}"}
		if socket != "" {
			args = append([]string{"-S", socket}, args...)
		}
		out, err := exec.CommandContext(ctx, tmux, args...).Output()
		if err != nil {
			return false, time.Time{}, false
		}
		a, at, ok := strings.Cut(strings.TrimSpace(string(out)), "\t")
		if !ok {
			return false, time.Time{}, false
		}
		n, err1 := strconv.Atoi(a)
		sec, err2 := strconv.ParseInt(at, 10, 64)
		if err1 != nil || err2 != nil {
			return false, time.Time{}, false
		}
		return n > 0, time.Unix(sec, 0), true
	}
}

// readLine legge una riga dal socket (usato dai test dell'hook).
func readLine(c net.Conn, wait time.Duration) (string, bool) {
	_ = c.SetReadDeadline(time.Now().Add(wait))
	sc := bufio.NewScanner(io.LimitReader(c, MaxHookLine+1))
	sc.Buffer(make([]byte, MaxHookLine+1), MaxHookLine+1)
	if !sc.Scan() {
		return "", false
	}
	return sc.Text(), true
}
