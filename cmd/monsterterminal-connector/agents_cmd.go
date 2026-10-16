package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ciarasino/monsterterminal-connector/internal/agents"
	"github.com/ciarasino/monsterterminal-connector/internal/journal"
	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
)

// hookSocket è il socket locale degli hook nella cartella di stato (spec §1.21).
func hookSocket(stateDir string) string { return filepath.Join(stateDir, "hook.sock") }

// tmuxPaneSession ricava il nome della sessione tmux del pannello. tmux trova il proprio server dalla
// variabile TMUX, ereditata dal processo dell'hook.
func tmuxPaneSession(pane string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "-t", pane, "#S").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// cmdHook è il comando che Claude Code esegue a ogni evento (spec §1.21). Non scrive mai sullo standard
// output, non fallisce mai: fuori da tmux, senza connettore attivo o con un evento ignoto non fa nulla.
func (a app) cmdHook(event, stateDir string) {
	known := false
	for _, e := range agents.HookEvents {
		if e == event {
			known = true
		}
	}
	pane := a.getenv("TMUX_PANE")
	// L'input va letto comunque: Claude Code lo scrive e si aspetta che venga consumato.
	raw, _ := io.ReadAll(io.LimitReader(a.stdin, 1<<20))
	if !known || pane == "" {
		return
	}
	var in struct {
		SessionID        string `json:"session_id"`
		NotificationType string `json:"notification_type"`
	}
	_ = json.Unmarshal(raw, &in)
	session, err := a.paneSession(pane)
	if err != nil || !protocol.IsSessionName(session) {
		return
	}
	msg := agents.HookMessage{Event: event, Session: session, NotificationType: in.NotificationType}
	if len(in.SessionID) <= 128 {
		msg.ClaudeSession = in.SessionID
	}
	_ = agents.Send(hookSocket(stateDir), msg, time.Second)
}

// cmdHooks installa o toglie gli hook di Claude Code, con la domanda di consenso (spec §1.21).
func (a app) cmdHooks(action, stateDir string, yes bool) error {
	home, err := a.home()
	if err != nil {
		return err
	}
	path := agents.SettingsPath(home)
	switch action {
	case "remove":
		changed, backup, err := agents.Remove(path, a.now())
		if err != nil {
			return err
		}
		if !changed {
			fmt.Fprintln(a.stdout, "Nessun hook del connettore da togliere.")
			return nil
		}
		fmt.Fprintf(a.stdout, "Hook del connettore tolti da %s (copia di prima: %s).\n", path, backup)
		return nil
	case "install":
	default:
		return fmt.Errorf("hooks: azione %q sconosciuta (install o remove)", action)
	}
	if _, ok := agents.FindClaude(home); !ok {
		fmt.Fprintln(a.stdout, "Claude Code non trovato su questo host: nessun hook installato.")
		return nil
	}
	if agents.Installed(path) {
		fmt.Fprintln(a.stdout, "Le notifiche di Claude Code sono già attive.")
		return nil
	}
	if !yes {
		fmt.Fprint(a.stdout, "Ho trovato Claude Code: attivo le notifiche? [S/n] ")
		line, err := bufio.NewReader(a.stdin).ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(a.stdout)
			return errors.New("nessuna risposta: notifiche non attivate (per rispondere sì senza terminale usare -yes)")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "s", "si", "sì", "y", "yes":
		default:
			fmt.Fprintln(a.stdout, "Notifiche non attivate.")
			return nil
		}
	}
	bin, err := a.executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	_, backup, err := agents.Install(path, bin, abs, a.now())
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("Notifiche di Claude Code attivate: hook aggiunti in %s.", path)
	if backup != "" {
		msg += fmt.Sprintf(" Copia di prima: %s.", backup)
	}
	fmt.Fprintln(a.stdout, msg)
	return nil
}

// startAgents avvia, per run, il socket degli hook e il timer di inattività.
func (a app) startAgents(ctx context.Context, log *slog.Logger, j *journal.Journal, stateDir string) *agents.Thresholds {
	tr := agents.NewTracker(j, a.now, log)
	th := agents.NewThresholds()
	go func() {
		if err := agents.Listen(ctx, hookSocket(stateDir), tr); err != nil {
			log.Warn("socket degli hook non disponibile: gli stati di Claude Code non arrivano", "errore", err.Error())
		}
	}()
	go tr.WatchIdle(ctx, agents.IdleConfig{Socket: a.tmuxSocket}, th)
	return th
}

// capabilities dichiarate in hello (§1.6): `hooks` se gli hook di Claude Code sono installati.
func (a app) capabilities() []string {
	home, err := a.home()
	if err != nil {
		return []string{}
	}
	if agents.Installed(agents.SettingsPath(home)) {
		return []string{"hooks"}
	}
	return []string{}
}
