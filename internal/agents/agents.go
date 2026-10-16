// Package agents traduce gli eventi degli agenti AI negli stati comuni di ADR 0410 D1 e li scrive nel
// giornale come agent.state (spec monsterterminal-saas.md §1.8, §1.21).
//
// Due sorgenti nella fetta 1: gli hook di Claude Code (adattatore con hook, ADR 0410 D2), che arrivano dal
// comando `hook` su un socket Unix locale, e il timer di inattività per le sessioni senza adattatore
// (livello generico, ADR 0410 D3). Gli eventi portano solo metadati: mai prompt, output o comandi.
package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
)

// HookMessage è la riga che il comando `hook` manda al connettore. Solo metadati (§1.21).
type HookMessage struct {
	Event            string `json:"event"`
	Session          string `json:"session"`
	ClaudeSession    string `json:"claudeSession,omitempty"`
	NotificationType string `json:"notificationType,omitempty"`
}

// MaxHookLine è la lunghezza massima di una riga sul socket.
const MaxHookLine = 4096

// StateFor traduce un evento di Claude Code nello stato comune (tabella di §1.21). ok=false per gli
// eventi che non producono stati.
func StateFor(m HookMessage) (state, waitingFor string, ok bool) {
	switch m.Event {
	case "UserPromptSubmit":
		return protocol.StateWorking, "", true
	case "Notification":
		switch m.NotificationType {
		case "permission_prompt":
			return protocol.StateWaiting, protocol.WaitPermission, true
		case "idle_prompt":
			return protocol.StateWaiting, protocol.WaitUnknown, true
		}
	case "Stop":
		return protocol.StateFinished, "", true
	case "StopFailure":
		return protocol.StateError, "", true
	case "SessionEnd":
		return protocol.StateIdle, "", true
	}
	return "", "", false
}

// Appender scrive un evento nel giornale (implementato da *journal.Journal).
type Appender interface {
	Append(kind string, data any) (protocol.Event, error)
}

type key struct{ agent, state, waiting string }

// Tracker tiene l'ultimo stato per sessione e scrive agent.state solo alle transizioni.
type Tracker struct {
	j   Appender
	now func() time.Time
	log *slog.Logger

	mu          sync.Mutex
	last        map[string]key
	adapterSeen map[string]time.Time // ultimo stato da un adattatore, per escludere il timer generico
}

// NewTracker crea il tracker.
func NewTracker(j Appender, now func() time.Time, log *slog.Logger) *Tracker {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Tracker{j: j, now: now, log: log, last: map[string]key{}, adapterSeen: map[string]time.Time{}}
}

// Apply registra uno stato. Restituisce true se è una transizione (e quindi è stato scritto).
func (t *Tracker) Apply(session, agent, state, source, waitingFor string) (bool, error) {
	if !protocol.IsSessionName(session) {
		return false, errors.New("nome di sessione fuori regola")
	}
	if state != protocol.StateWaiting {
		waitingFor = ""
	}
	t.mu.Lock()
	k := key{agent, state, waitingFor}
	if source != protocol.SourceIdle {
		t.adapterSeen[session] = t.now()
	}
	if prev, ok := t.last[session]; ok && prev == k {
		t.mu.Unlock()
		return false, nil
	}
	t.last[session] = k
	t.mu.Unlock()
	_, err := t.j.Append(protocol.KindAgentState, protocol.AgentState{
		Session: session, Agent: agent, State: state, Source: source, WaitingFor: waitingFor,
	})
	if err != nil {
		return true, err
	}
	t.log.Info("stato dell'agente", "sessione", session, "agente", agent, "stato", state, "attesa", waitingFor)
	return true, nil
}

// adapterActiveSince dice se un adattatore ha dato uno stato per la sessione dopo t0.
func (t *Tracker) adapterActiveSince(session string, t0 time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.adapterSeen[session]
	return ok && at.After(t0)
}

func (t *Tracker) lastState(session string) (key, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k, ok := t.last[session]
	return k, ok
}

// HandleHook applica una riga del socket.
func (t *Tracker) HandleHook(m HookMessage) error {
	state, waiting, ok := StateFor(m)
	if !ok {
		return nil
	}
	_, err := t.Apply(m.Session, protocol.AgentClaude, state, protocol.SourceHook, waiting)
	return err
}

// Listen apre il socket degli hook e lo serve finché ctx non termina. Il socket è 0600: lo usa solo
// l'utente del connettore, lo stesso che esegue Claude Code (ADR 0409 D5).
func Listen(ctx context.Context, path string, t *Tracker) error {
	_ = os.Remove(path) // un socket rimasto da un'uscita brusca
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	defer os.Remove(path)
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// Una connessione alla volta, nell'ordine di arrivo: due hook ravvicinati (Notification e poi
		// Stop) serviti in parallelo potrebbero applicarsi al contrario e lasciare lo stato sbagliato.
		// Le righe sono piccole e il tetto di 2 s impedisce a un client lento di fermare gli altri a lungo.
		t.serve(c)
	}
}

func (t *Tracker) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	sc := bufio.NewScanner(io.LimitReader(c, MaxHookLine+1))
	sc.Buffer(make([]byte, MaxHookLine+1), MaxHookLine+1)
	if !sc.Scan() {
		return
	}
	var m HookMessage
	if err := protocol.DecodeStrict(sc.Bytes(), &m); err != nil {
		t.log.Warn("riga di hook non valida, scartata")
		return
	}
	if err := t.HandleHook(m); err != nil {
		t.log.Warn("stato dell'agente non registrato", "errore", err.Error())
	}
}

// Send manda una riga al connettore attivo; errore se non risponde entro il tempo.
func Send(path string, m HookMessage, timeout time.Duration) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(raw) > MaxHookLine {
		return errors.New("riga di hook troppo lunga")
	}
	c, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	_, err = c.Write(append(raw, '\n'))
	return err
}
