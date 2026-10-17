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

	"github.com/ciarasino/monsterterminal-connector/internal/agents"
	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
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
