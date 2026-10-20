package main

import (
	"bufio"
	"bytes"
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

	"github.com/monsterterminal/connector/internal/agents"
	"github.com/monsterterminal/connector/internal/journal"
	"github.com/monsterterminal/connector/internal/protocol"
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
// ctx finisce col SIGTERM che Claude Code manda all'hook di permesso in attesa quando l'utente annulla
// il prompt con Esc (§2.14): l'hook chiude la connessione ed esce senza decisione.
func (a app) cmdHook(ctx context.Context, event, stateDir string) {
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
		SessionID        string          `json:"session_id"`
		NotificationType string          `json:"notification_type"`
		ToolName         string          `json:"tool_name"`
		ToolInput        json.RawMessage `json:"tool_input"`
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
	if event != "PermissionRequest" {
		_ = agents.Send(hookSocket(stateDir), msg, time.Second)
		return
	}
	// Hook di permesso (§1.21 «Hook di permesso», §2.12): porta al connettore lo strumento e il suo
	// input e aspetta la decisione dalla card per la finestra adattiva. Il prompt nel terminale
	// compare comunque in parallelo: chi risponde prima vince.
	if in.ToolName == "" {
		return
	}
	msg.Action = in.ToolName
	if in.ToolName == toolAskUserQuestion {
		// Fetta 3b (§2.14): le domande dell'agente arrivano come PermissionRequest di AskUserQuestion.
		msg.Kind = protocol.KindQuestion
		msg.Questions = askQuestions(in.ToolInput)
		msg.Detail, msg.DetailTruncated = protocol.TruncateDetail(questionDetail(msg.Questions))
	} else {
		msg.Detail, msg.DetailTruncated = protocol.TruncateDetail(permissionDetail(in.ToolInput))
	}
	d, err := agents.Ask(ctx, hookSocket(stateDir), msg, time.Second, time.Duration(agents.PermissionHookTimeoutSeconds)*time.Second)
	if err != nil || d.Behavior == "" {
		return
	}
	var decision map[string]any
	if msg.Kind == protocol.KindQuestion {
		decision = questionDecision(d, in.ToolInput, msg.Questions)
	} else {
		decision = permissionDecision(d)
	}
	if decision == nil {
		return
	}
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PermissionRequest",
		"decision":      decision,
	}}
	enc, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Fprintln(a.stdout, string(enc))
}

// toolAskUserQuestion è lo strumento con cui Claude Code fa le domande (banco del 2026-10-19, §2.14).
const toolAskUserQuestion = "AskUserQuestion"

// askQuestions decodifica tool_input.questions di AskUserQuestion nella forma di Claude Code
// (question, header facoltativo, options[]{label, description}, multiSelect); i campi in più
// dello strumento si ignorano, un input non decodificabile dà nessuna domanda.
func askQuestions(input json.RawMessage) []protocol.Question {
	if len(input) == 0 {
		return nil
	}
	var in struct {
		Questions []protocol.Question `json:"questions"`
	}
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	return in.Questions
}

// questionDetail è il contesto di una domanda (§2.14): il testo della prima domanda, più
// «(+N domande)» se sono più di una.
func questionDetail(qs []protocol.Question) string {
	if len(qs) == 0 {
		return ""
	}
	if len(qs) == 1 {
		return qs[0].Question
	}
	return fmt.Sprintf("%s (+%d domande)", qs[0].Question, len(qs)-1)
}

// questionDecision è la decisione dell'hook per AskUserQuestion (§2.14 fatto 3): allow con
// updatedInput = tool_input originale, tutti i campi, più answers {«testo della domanda»: «risposta»}.
// Le risposte del connettore sono per posizione: la chiave è il testo originale della domanda in
// tool_input (non quello troncato ai tetti dello schema), così Claude Code la riconosce. Senza
// risposte nessuna decisione: il prompt resta nel terminale.
func questionDecision(d agents.Decision, input json.RawMessage, qs []protocol.Question) map[string]any {
	if d.Behavior != "allow" || len(d.Answers) == 0 {
		return nil
	}
	updated := map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &updated); err != nil {
			return nil
		}
	}
	answers := map[string]string{}
	for i, a := range d.Answers {
		key := a.Question
		if i < len(qs) {
			key = qs[i].Question
		}
		answers[key] = a.Text
	}
	updated["answers"] = answers
	return map[string]any{"behavior": "allow", "updatedInput": updated}
}

// permissionDetail riduce tool_input al contesto della richiesta: il comando esatto quando c'è
// (Bash), altrimenti l'input compatto. Mai altro che questo (§1.8: il giornale resta pulito; qui si
// viaggia fuori dal giornale, §2.9 punto 2).
func permissionDetail(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var cmd struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &cmd) == nil && cmd.Command != "" {
		return cmd.Command
	}
	var compact bytes.Buffer
	if json.Compact(&compact, input) == nil {
		return compact.String()
	}
	return string(input)
}

// permissionDecision è l'oggetto decision dell'hook PermissionRequest di Claude Code.
func permissionDecision(d agents.Decision) map[string]any {
	out := map[string]any{"behavior": d.Behavior}
	if d.Behavior == "deny" && d.Message != "" {
		out["message"] = d.Message
	}
	return out
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

// startAgents avvia, per run, il socket degli hook, il timer di inattività e il gestore delle
// richieste di permesso (§2.12).
func (a app) startAgents(ctx context.Context, log *slog.Logger, j *journal.Journal, stateDir string) (*agents.Thresholds, *agents.Requests) {
	tr := agents.NewTracker(j, a.now, log)
	th := agents.NewThresholds()
	rq := agents.NewRequests(agents.RequestsConfig{Socket: a.tmuxSocket, Now: a.now, Logger: log, Probe: a.sessionProbe}, tr)
	go func() {
		if err := agents.Listen(ctx, hookSocket(stateDir), tr); err != nil {
			log.Warn("socket degli hook non disponibile: gli stati di Claude Code non arrivano", "errore", err.Error())
		}
	}()
	go tr.WatchIdle(ctx, agents.IdleConfig{Socket: a.tmuxSocket}, th)
	return th, rq
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
