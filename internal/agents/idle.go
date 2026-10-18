package agents

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

// Livello generico (ADR 0410 D3, spec §1.21): una sessione senza adattatore da AdapterQuiet passa in
// attende-input («sconosciuto») quando l'output resta fermo oltre la soglia, e torna a lavora quando
// riprende. L'attività si legge da tmux (session_activity), mai l'output.
type IdleConfig struct {
	Tmux         string        // eseguibile di tmux; vuoto = "tmux"
	Socket       string        // socket del server tmux (-S); vuoto = quello di default
	Interval     time.Duration // ogni quanto si guarda; zero = 30 s
	AdapterQuiet time.Duration // da quanto un adattatore deve tacere; zero = 30 min
}

// Thresholds sono le soglie della configurazione dell'host (§1.11), aggiornabili.
type Thresholds struct {
	mu         sync.Mutex
	def        time.Duration
	perSession map[string]time.Duration
}

// NewThresholds parte dalla soglia di default della spec, 5 minuti.
func NewThresholds() *Thresholds {
	return &Thresholds{def: 5 * time.Minute, perSession: map[string]time.Duration{}}
}

// Set applica la configurazione dell'host.
func (th *Thresholds) Set(c protocol.HostConfig) {
	th.mu.Lock()
	defer th.mu.Unlock()
	if c.DefaultIdleThresholdSeconds > 0 {
		th.def = time.Duration(c.DefaultIdleThresholdSeconds) * time.Second
	}
	th.perSession = map[string]time.Duration{}
	for s, v := range c.SessionIdleThresholds {
		th.perSession[s] = time.Duration(v) * time.Second
	}
}

// For è la soglia della sessione.
func (th *Thresholds) For(session string) time.Duration {
	th.mu.Lock()
	defer th.mu.Unlock()
	if d, ok := th.perSession[session]; ok {
		return d
	}
	return th.def
}

// SessionActivity è una sessione tmux con l'istante dell'ultima attività.
type SessionActivity struct {
	Name     string
	Activity time.Time
}

// ListActivity legge le sessioni dal server tmux. Senza server l'elenco è vuoto.
func ListActivity(ctx context.Context, tmux, socket string) []SessionActivity {
	if tmux == "" {
		tmux = "tmux"
	}
	args := []string{"list-sessions", "-F", "#{session_name}\t#{session_activity}"}
	if socket != "" {
		args = append([]string{"-S", socket}, args...)
	}
	out, err := exec.CommandContext(ctx, tmux, args...).Output()
	if err != nil {
		return nil
	}
	var res []SessionActivity
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, at, ok := strings.Cut(line, "\t")
		if !ok || !protocol.IsSessionName(name) {
			continue
		}
		sec, err := strconv.ParseInt(at, 10, 64)
		if err != nil {
			continue
		}
		res = append(res, SessionActivity{Name: name, Activity: time.Unix(sec, 0)})
	}
	return res
}

// CheckIdle applica una lettura dell'attività: la logica del timer, senza tmux né attese.
func (t *Tracker) CheckIdle(sessions []SessionActivity, th *Thresholds, adapterQuiet time.Duration) error {
	now := t.now()
	for _, s := range sessions {
		if t.adapterActiveSince(s.Name, now.Add(-adapterQuiet)) {
			continue // la sessione la segue un adattatore
		}
		idle := now.Sub(s.Activity) >= th.For(s.Name)
		last, ok := t.lastState(s.Name)
		genericWaiting := ok && last.agent == protocol.AgentGeneric && last.state == protocol.StateWaiting
		switch {
		case idle && !genericWaiting:
			if _, err := t.Apply(s.Name, protocol.AgentGeneric, protocol.StateWaiting, protocol.SourceIdle, protocol.WaitUnknown); err != nil {
				return err
			}
		case !idle && genericWaiting:
			if _, err := t.Apply(s.Name, protocol.AgentGeneric, protocol.StateWorking, protocol.SourceIdle, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// WatchIdle guarda le sessioni finché ctx non termina.
func (t *Tracker) WatchIdle(ctx context.Context, cfg IdleConfig, th *Thresholds) {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.AdapterQuiet <= 0 {
		cfg.AdapterQuiet = 30 * time.Minute
	}
	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()
	for {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		sessions := ListActivity(lctx, cfg.Tmux, cfg.Socket)
		cancel()
		if err := t.CheckIdle(sessions, th, cfg.AdapterQuiet); err != nil {
			t.log.Warn("timer di inattività", "errore", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
