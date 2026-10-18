// Cablaggio dell'adattatore di Claude Code nel comando (spec monsterterminal-saas.md §1.21): la domanda di
// consenso di `hooks install`, `hooks remove`, il comando `hook` che porta lo stato fino al backend, la
// capacità `hooks` in hello.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/agents"
	"github.com/monsterterminal/connector/internal/protocol"
)

// conClaude prepara una home con Claude Code in ~/.local/bin e toglie il claude vero dal PATH.
func conClaude(t *testing.T, h *harness, presente bool) string {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	home := filepath.Join(h.dir, "home")
	if presente {
		p := filepath.Join(home, ".local", "bin", "claude")
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	}
	return agents.SettingsPath(home)
}

func (h *harness) hooks(t *testing.T, action string, extra ...string) int {
	t.Helper()
	return h.app().run(context.Background(), append([]string{"hooks", "-state-dir", h.stateDir}, append(extra, action)...))
}

func TestHooksInstallChiedeIlConsenso(t *testing.T) {
	h := newHarness(t)
	settings := conClaude(t, h, true)
	h.stdin = "\n" // Invio = sì, come suggerisce [S/n]
	if code := h.hooks(t, "install"); code != exitOK {
		t.Fatalf("uscita %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "Ho trovato Claude Code: attivo le notifiche? [S/n]") {
		t.Fatalf("domanda di consenso assente: %q", h.stdout.String())
	}
	if !agents.Installed(settings) {
		t.Fatal("hook non installati dopo il sì")
	}
	raw, _ := os.ReadFile(settings)
	if !strings.Contains(string(raw), "/usr/local/bin/monsterterminal-connector hook -state-dir "+h.stateDir+" Stop") {
		t.Fatalf("comando di hook inatteso: %s", raw)
	}
	h.stdout.mu.Lock()
	h.stdout.b.Reset()
	h.stdout.mu.Unlock()
	if code := h.hooks(t, "install"); code != exitOK || !strings.Contains(h.stdout.String(), "già attive") {
		t.Fatalf("seconda volta: %d %q", code, h.stdout.String())
	}
}

func TestHooksInstallRifiutato(t *testing.T) {
	h := newHarness(t)
	settings := conClaude(t, h, true)
	h.stdin = "n\n"
	if code := h.hooks(t, "install"); code != exitOK || !strings.Contains(h.stdout.String(), "Notifiche non attivate") {
		t.Fatalf("uscita %d, %q", code, h.stdout.String())
	}
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Fatal("impostazioni toccate nonostante il no")
	}
}

func TestHooksInstallSenzaRispostaESenzaClaude(t *testing.T) {
	h := newHarness(t)
	settings := conClaude(t, h, true)
	h.stdin = "" // nessun terminale: senza -yes non si installa
	if code := h.hooks(t, "install"); code != exitError || !strings.Contains(h.stderr.String(), "-yes") {
		t.Fatalf("uscita %d, %q", code, h.stderr.String())
	}
	if agents.Installed(settings) {
		t.Fatal("installati senza consenso")
	}
	if code := h.hooks(t, "install", "-yes"); code != exitOK || !agents.Installed(settings) {
		t.Fatalf("-yes: uscita %d", code)
	}

	h2 := newHarness(t)
	conClaude(t, h2, false)
	if code := h2.hooks(t, "install", "-yes"); code != exitOK || !strings.Contains(h2.stdout.String(), "Claude Code non trovato") {
		t.Fatalf("senza Claude Code: %d %q", code, h2.stdout.String())
	}
}

func TestHooksRemoveEUso(t *testing.T) {
	h := newHarness(t)
	settings := conClaude(t, h, true)
	h.hooks(t, "install", "-yes")
	if code := h.hooks(t, "remove"); code != exitOK || agents.Installed(settings) {
		t.Fatalf("remove: %d", code)
	}
	if code := h.hooks(t, "boh"); code != exitError {
		t.Fatalf("azione sconosciuta: %d", code)
	}
	if code := h.app().run(context.Background(), []string{"hooks"}); code != exitUsage {
		t.Fatalf("senza azione: %d", code)
	}
}

func (h *harness) hook(event, pane, stdin string) (int, string) {
	h.env["TMUX_PANE"] = pane
	a := h.app()
	a.stdin = strings.NewReader(stdin)
	out := &syncBuf{}
	a.stdout = out
	code := a.run(context.Background(), []string{"hook", "-state-dir", h.stateDir, event})
	return code, out.String()
}

// Dal comando hook al backend: lo stato arriva come agent.state, senza il testo dell'agente.
func TestHookPortaLoStatoFinoAlBackend(t *testing.T) {
	h := newHarness(t)
	settings := conClaude(t, h, true)
	h.panes = map[string]string{"%3": "lavoro"}
	h.hooks(t, "install", "-yes")
	if !agents.Installed(settings) {
		t.Fatal("hook non installati")
	}
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	id := hostIDFromStdout(t, h.stdout.String())
	done, stop := h.start(t)
	defer func() { stop(); waitExit(t, done) }()
	if hl := waitHello(t, h.s); len(hl.Capabilities) != 1 || hl.Capabilities[0] != "hooks" {
		t.Fatalf("capabilities in hello: %v", hl.Capabilities)
	}
	eventually(t, "socket degli hook", func() bool { _, err := os.Stat(hookSocket(h.stateDir)); return err == nil })

	code, out := h.hook("Notification", "%3", `{"session_id":"abc","hook_event_name":"Notification","notification_type":"permission_prompt","message":"Claude vuole eseguire rm -rf segreto"}`)
	if code != exitOK || out != "" {
		t.Fatalf("hook: uscita %d, stdout %q", code, out)
	}
	h.hook("Stop", "%3", `{"session_id":"abc","last_assistant_message":"segreto"}`)
	eventually(t, "due agent.state al backend", func() bool { return len(stati(h, id)) == 2 })
	got := stati(h, id)
	if got[0].State != protocol.StateWaiting || got[0].WaitingFor != protocol.WaitPermission || got[0].Session != "lavoro" ||
		got[0].Agent != protocol.AgentClaude || got[0].Source != protocol.SourceHook || got[1].State != protocol.StateFinished {
		t.Fatalf("stati %+v", got)
	}
	for _, ev := range h.s.Events(id) {
		if strings.Contains(string(ev.Data), "segreto") {
			t.Fatalf("testo dell'agente nel giornale: %s", ev.Data)
		}
	}
}

func stati(h *harness, id string) []protocol.AgentState {
	var out []protocol.AgentState
	for _, ev := range h.s.Events(id) {
		if ev.Kind != protocol.KindAgentState {
			continue
		}
		var a protocol.AgentState
		_ = json.Unmarshal(ev.Data, &a)
		if a.Agent == protocol.AgentClaude {
			out = append(out, a)
		}
	}
	return out
}

// Il comando hook non disturba mai Claude Code: niente uscita, codice 0, anche fuori da tmux, senza
// connettore, con un evento ignoto o un pannello sconosciuto.
func TestHookNonBloccaMai(t *testing.T) {
	h := newHarness(t)
	h.panes = map[string]string{"%1": "lavoro"}
	for _, c := range []struct{ ev, pane string }{
		{"Stop", ""},           // fuori da tmux
		{"Stop", "%1"},         // nessun connettore in ascolto
		{"PreToolUse", "%1"},   // evento che non produce stati
		{"Stop", "%9"},         // pannello sconosciuto
		{"Notification", "%1"}, // stdin vuoto
	} {
		began := time.Now()
		code, out := h.hook(c.ev, c.pane, "")
		if code != exitOK || out != "" || time.Since(began) > 3*time.Second {
			t.Errorf("%+v: uscita %d, stdout %q, %v", c, code, out, time.Since(began))
		}
	}
	if code := h.app().run(context.Background(), []string{"hook", "-state-dir", h.stateDir}); code != exitOK {
		t.Errorf("hook senza evento: %d", code)
	}
}

// I flag si possono scrivere anche dopo l'azione, come nel README: `hooks install -yes`.
func TestHooksFlagDopoLAzione(t *testing.T) {
	h := newHarness(t)
	settings := conClaude(t, h, true)
	code := h.app().run(context.Background(), []string{"hooks", "install", "-yes", "-state-dir", h.stateDir})
	if code != exitOK || !agents.Installed(settings) {
		t.Fatalf("hooks install -yes -state-dir: uscita %d, stderr %s", code, h.stderr.String())
	}
	raw, _ := os.ReadFile(settings)
	if !strings.Contains(string(raw), "hook -state-dir "+h.stateDir+" Stop") {
		t.Fatalf("-state-dir dopo l'azione ignorato: %s", raw)
	}
	if code := h.app().run(context.Background(), []string{"hooks", "remove", "extra"}); code != exitUsage {
		t.Fatalf("argomento in più: uscita %d", code)
	}
}

// Fetta 2 (spec §1.21 «Hook di permesso», §2.12): l'hook PermissionRequest porta il contesto al backend
// fuori dal giornale, lo stato attende-input/permesso col requestId nel giornale, e resta in attesa;
// request.respond dalla card diventa la decisione dell'hook e request.respond.result «consegnata».
func TestHookDiPermessoFinoAllaCard(t *testing.T) {
	h := newHarness(t)
	conClaude(t, h, true)
	h.panes = map[string]string{"%3": "lavoro"}
	h.attached = true
	h.hooks(t, "install", "-yes")
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	id := hostIDFromStdout(t, h.stdout.String())
	done, stop := h.start(t)
	defer func() { stop(); waitExit(t, done) }()
	waitHello(t, h.s)
	eventually(t, "socket degli hook", func() bool { _, err := os.Stat(hookSocket(h.stateDir)); return err == nil })

	type res struct {
		code int
		out  string
	}
	got := make(chan res, 1)
	go func() {
		code, out := h.hook("PermissionRequest", "%3", `{"session_id":"abc","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf /tmp/segreto","description":"pulisce"}}`)
		got <- res{code, out}
	}()
	eventually(t, "request.context al backend", func() bool { return len(h.s.Contexts(id)) == 1 })
	ctx := h.s.Contexts(id)[0]
	if ctx.Action != "Bash" || ctx.Detail != "rm -rf /tmp/segreto" || ctx.Kind != protocol.KindPermission || ctx.Deadline == "" {
		t.Fatalf("contesto %+v", ctx)
	}
	eventually(t, "agent.state col requestId", func() bool {
		st := stati(h, id)
		return len(st) == 1 && st[0].RequestID == ctx.RequestID && st[0].WaitingFor == protocol.WaitPermission
	})
	for _, ev := range h.s.Events(id) {
		if strings.Contains(string(ev.Data), "segreto") {
			t.Fatalf("contesto nel giornale: %s", ev.Data)
		}
	}
	select {
	case r := <-got:
		t.Fatalf("l'hook è uscito prima della risposta: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	rid, err := h.s.SendControl(id, protocol.TypeRequestRespond, protocol.RequestRespond{RequestID: ctx.RequestID, Answer: protocol.AnswerApprove})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "request.respond.result", func() bool { _, ok := h.s.Reply(id, rid); return ok })
	reply, _ := h.s.Reply(id, rid)
	var result protocol.RequestRespondResult
	_ = json.Unmarshal(reply.Body, &result)
	if reply.Type != protocol.TypeRequestRespondResult || result.Outcome != protocol.OutcomeDelivered || result.RequestID != ctx.RequestID {
		t.Fatalf("esito %s %+v", reply.Type, result)
	}
	select {
	case r := <-got:
		if r.code != exitOK || !strings.Contains(r.out, `"hookEventName":"PermissionRequest"`) || !strings.Contains(r.out, `"behavior":"allow"`) {
			t.Fatalf("hook: uscita %d, stdout %q", r.code, r.out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("l'hook non è uscito dopo la risposta")
	}
	// Risposta tardiva: superata.
	rid2, _ := h.s.SendControl(id, protocol.TypeRequestRespond, protocol.RequestRespond{RequestID: ctx.RequestID, Answer: protocol.AnswerDeny})
	eventually(t, "secondo esito", func() bool { _, ok := h.s.Reply(id, rid2); return ok })
	reply2, _ := h.s.Reply(id, rid2)
	_ = json.Unmarshal(reply2.Body, &result)
	if result.Outcome != protocol.OutcomeSuperseded {
		t.Fatalf("risposta tardiva: %+v", result)
	}
}

// §1.21 fatto 3: la risposta nel terminale non uccide l'hook; PostToolUse → lavora chiude la richiesta
// e l'hook esce senza decisione (stdout vuoto). Senza strumento l'hook non apre niente.
func TestHookDiPermessoSuperatoNelTerminale(t *testing.T) {
	h := newHarness(t)
	conClaude(t, h, true)
	h.panes = map[string]string{"%3": "lavoro"}
	h.hooks(t, "install", "-yes")
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	id := hostIDFromStdout(t, h.stdout.String())
	done, stop := h.start(t)
	defer func() { stop(); waitExit(t, done) }()
	waitHello(t, h.s)
	eventually(t, "socket degli hook", func() bool { _, err := os.Stat(hookSocket(h.stateDir)); return err == nil })
	if code, out := h.hook("PermissionRequest", "%3", `{"session_id":"abc"}`); code != exitOK || out != "" {
		t.Fatalf("senza tool_name: %d %q", code, out)
	}
	type res struct {
		code int
		out  string
	}
	got := make(chan res, 1)
	go func() {
		code, out := h.hook("PermissionRequest", "%3", `{"tool_name":"Write","tool_input":{"file_path":"/tmp/a","content":"x"}}`)
		got <- res{code, out}
	}()
	eventually(t, "request.context", func() bool { return len(h.s.Contexts(id)) == 1 })
	if c := h.s.Contexts(id)[0]; c.Detail != `{"file_path":"/tmp/a","content":"x"}` {
		t.Fatalf("contesto compatto atteso: %q", c.Detail)
	}
	h.hook("PostToolUse", "%3", `{"tool_name":"Write"}`)
	select {
	case r := <-got:
		if r.code != exitOK || r.out != "" {
			t.Fatalf("hook superato: %d %q", r.code, r.out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("l'hook non è uscito al cambio di stato")
	}
	eventually(t, "lavora nel giornale", func() bool {
		st := stati(h, id)
		return len(st) == 2 && st[1].State == protocol.StateWorking
	})
}

// TD-MT-CONNECTOR-SOCKET-LOCK: due run sulla stessa cartella di stato si cancellerebbero il socket a
// vicenda; il secondo si ferma subito con un messaggio chiaro.
func TestUnSoloConnettorePerCartellaDiStato(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	done, stop := h.start(t)
	defer func() { stop(); waitExit(t, done) }()
	waitHello(t, h.s)
	h2 := *h
	h2.stderr = &syncBuf{}
	code := h2.app().run(context.Background(), h2.args("run"))
	if code != exitError || !strings.Contains(h2.stderr.String(), "già in esecuzione") {
		t.Fatalf("secondo run: uscita %d, stderr %q", code, h2.stderr.String())
	}
}
