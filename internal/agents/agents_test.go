// Test dell'adattatore di Claude Code e del livello generico (spec monsterterminal-saas.md §1.21,
// ADR 0410 D1-D3). La tabella di corrispondenza è verificata riga per riga (regola #26).
package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

type fakeJournal struct {
	mu  sync.Mutex
	evs []protocol.AgentState
}

func (f *fakeJournal) Append(kind string, data any) (protocol.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if kind != protocol.KindAgentState {
		panic("kind " + kind)
	}
	f.evs = append(f.evs, data.(protocol.AgentState))
	return protocol.Event{}, nil
}

func (f *fakeJournal) all() []protocol.AgentState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.AgentState(nil), f.evs...)
}

// §1.21: la tabella evento di Claude Code → stato, riga per riga.
func TestCorrispondenzaHookStati(t *testing.T) {
	cases := []struct {
		ev, notif, state, waiting string
	}{
		{"UserPromptSubmit", "", protocol.StateWorking, ""},
		{"Notification", "permission_prompt", protocol.StateWaiting, protocol.WaitPermission},
		{"Notification", "idle_prompt", protocol.StateWaiting, protocol.WaitUnknown},
		{"PermissionRequest", "", protocol.StateWaiting, protocol.WaitPermission}, // fetta 2, §2.12
		{"PostToolUse", "", protocol.StateWorking, ""},                            // fetta 2, §2.12
		{"Stop", "", protocol.StateFinished, ""},
		{"StopFailure", "", protocol.StateError, ""},
		{"SessionEnd", "", protocol.StateIdle, ""},
	}
	for _, c := range cases {
		st, w, ok := StateFor(HookMessage{Event: c.ev, NotificationType: c.notif})
		if !ok || st != c.state || w != c.waiting {
			t.Errorf("%s/%s → %s/%s/%v, atteso %s/%s", c.ev, c.notif, st, w, ok, c.state, c.waiting)
		}
	}
	for _, m := range []HookMessage{{Event: "PreToolUse"}, {Event: "SessionStart"}, {Event: "Notification", NotificationType: "auth_success"}, {Event: "Notification"}} {
		if _, _, ok := StateFor(m); ok {
			t.Errorf("%+v non deve produrre uno stato", m)
		}
	}
	// Ogni evento della tabella è fra quelli che hooks install registra.
	for _, c := range cases {
		found := false
		for _, e := range HookEvents {
			if e == c.ev {
				found = true
			}
		}
		if !found {
			t.Errorf("%s non è installato da hooks install", c.ev)
		}
	}
}

func TestSoloLeTransizioniVannoNelGiornale(t *testing.T) {
	j := &fakeJournal{}
	tr := NewTracker(j, nil, nil)
	for _, ev := range []string{"UserPromptSubmit", "UserPromptSubmit", "Stop", "Stop", "UserPromptSubmit"} {
		if err := tr.HandleHook(HookMessage{Event: ev, Session: "lavoro"}); err != nil {
			t.Fatal(err)
		}
	}
	got := j.all()
	if len(got) != 3 || got[0].State != protocol.StateWorking || got[1].State != protocol.StateFinished || got[2].State != protocol.StateWorking {
		t.Fatalf("eventi %+v", got)
	}
	if got[0].Agent != protocol.AgentClaude || got[0].Source != protocol.SourceHook || got[0].Session != "lavoro" {
		t.Fatalf("campi %+v", got[0])
	}
	// waitingFor solo con attende-input (schema 0.2.0).
	if _, err := tr.Apply("lavoro", protocol.AgentClaude, protocol.StateFinished, protocol.SourceHook, protocol.WaitPermission); err != nil {
		t.Fatal(err)
	}
	for _, e := range j.all() {
		if e.State != protocol.StateWaiting && e.WaitingFor != "" {
			t.Fatalf("waitingFor fuori da attende-input: %+v", e)
		}
	}
	if _, err := tr.Apply("a;rm", protocol.AgentClaude, protocol.StateWorking, protocol.SourceHook, ""); err == nil {
		t.Fatal("nome di sessione fuori regola accettato")
	}
}

// Il dato scritto nel giornale non contiene mai testo dell'agente: solo i campi dello schema.
func TestEventoSoloMetadati(t *testing.T) {
	j := &fakeJournal{}
	tr := NewTracker(j, nil, nil)
	_ = tr.HandleHook(HookMessage{Event: "Notification", NotificationType: "permission_prompt", Session: "s", ClaudeSession: "abc"})
	raw, _ := json.Marshal(j.all()[0])
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for k := range m {
		if k != "session" && k != "agent" && k != "state" && k != "source" && k != "waitingFor" {
			t.Errorf("campo inatteso %s nel dato", k)
		}
	}
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "mta")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestSocketDegliHook(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "hook.sock")
	j := &fakeJournal{}
	tr := NewTracker(j, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Listen(ctx, sock, tr) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket non creato")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fi, _ := os.Stat(sock); fi.Mode().Perm() != 0o600 {
		t.Fatalf("permessi del socket %o, attesi 600", fi.Mode().Perm())
	}
	if err := Send(sock, HookMessage{Event: "UserPromptSubmit", Session: "lavoro"}, time.Second); err != nil {
		t.Fatal(err)
	}
	// Riga con un campo in più: scartata.
	if err := sendRaw(sock, `{"event":"Stop","session":"lavoro","prompt":"segreto"}`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	got := j.all()
	if len(got) != 1 || got[0].State != protocol.StateWorking {
		t.Fatalf("eventi %+v (la riga col campo in più doveva essere scartata)", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket non rimosso alla chiusura")
	}
	// Senza connettore Send fallisce subito, e il comando hook esce comunque (lo prova cmd/).
	if err := Send(sock, HookMessage{Event: "Stop", Session: "x"}, 200*time.Millisecond); err == nil {
		t.Fatal("Send senza connettore riuscito")
	}
}

func sendRaw(sock, line string) error {
	c, err := netDial(sock)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte(line + "\n"))
	return err
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// ADR 0410 D3: output fermo oltre la soglia → attende-input generico; ripresa → lavora.
func TestTimerDiInattivita(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	j := &fakeJournal{}
	tr := NewTracker(j, c.now, nil)
	th := NewThresholds()
	act := func(ago time.Duration) []SessionActivity {
		return []SessionActivity{{Name: "shell", Activity: c.t.Add(-ago)}}
	}
	_ = tr.CheckIdle(act(4*time.Minute), th, 30*time.Minute)
	if len(j.all()) != 0 {
		t.Fatal("sotto la soglia di 5 minuti non deve succedere nulla")
	}
	_ = tr.CheckIdle(act(6*time.Minute), th, 30*time.Minute)
	_ = tr.CheckIdle(act(7*time.Minute), th, 30*time.Minute) // nessun doppione
	got := j.all()
	if len(got) != 1 || got[0].State != protocol.StateWaiting || got[0].Agent != protocol.AgentGeneric ||
		got[0].Source != protocol.SourceIdle || got[0].WaitingFor != protocol.WaitUnknown {
		t.Fatalf("eventi %+v", got)
	}
	_ = tr.CheckIdle(act(10*time.Second), th, 30*time.Minute)
	if got := j.all(); len(got) != 2 || got[1].State != protocol.StateWorking {
		t.Fatalf("alla ripresa atteso lavora: %+v", got)
	}
	// Soglia per sessione dalla configurazione dell'host (§1.11).
	th.Set(protocol.HostConfig{DefaultIdleThresholdSeconds: 300, SessionIdleThresholds: map[string]int{"shell": 3600}})
	_ = tr.CheckIdle(act(10*time.Minute), th, 30*time.Minute)
	if len(j.all()) != 2 {
		t.Fatal("con soglia di un'ora, 10 minuti non bastano")
	}
}

// Una sessione seguita da un adattatore non passa sotto il timer finché l'adattatore non tace da 30 minuti.
func TestTimerNonToccaLeSessioniConAdattatore(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	j := &fakeJournal{}
	tr := NewTracker(j, c.now, nil)
	_ = tr.HandleHook(HookMessage{Event: "Stop", Session: "claude"})
	c.t = c.t.Add(20 * time.Minute)
	_ = tr.CheckIdle([]SessionActivity{{Name: "claude", Activity: c.t.Add(-20 * time.Minute)}}, NewThresholds(), 30*time.Minute)
	if len(j.all()) != 1 {
		t.Fatalf("il timer ha toccato una sessione con adattatore: %+v", j.all())
	}
	c.t = c.t.Add(15 * time.Minute)
	_ = tr.CheckIdle([]SessionActivity{{Name: "claude", Activity: c.t.Add(-35 * time.Minute)}}, NewThresholds(), 30*time.Minute)
	if got := j.all(); len(got) != 2 || got[1].Agent != protocol.AgentGeneric {
		t.Fatalf("dopo 30 minuti di silenzio dell'adattatore il timer deve valere: %+v", got)
	}
}

func TestInstallazioneDegliHook(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, ".claude", "settings.json")
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	bin, state := "/usr/local/bin/monsterterminal-connector", "/var/lib/monsterterminal"

	// File assente: si crea con tutti gli hook, permessi 0600.
	changed, backup, err := Install(path, bin, state, now)
	if err != nil || !changed || backup != "" {
		t.Fatalf("prima installazione: %v %q %v", changed, backup, err)
	}
	if !Installed(path) {
		t.Fatal("Installed dopo Install = false")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("permessi %o", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	for _, ev := range HookEvents {
		if !strings.Contains(string(raw), bin+" hook -state-dir "+state+" "+ev) {
			t.Errorf("manca l'hook di %s", ev)
		}
	}
	// §1.21 «Hook di permesso»: solo PermissionRequest ha il timeout lungo (la finestra adattiva, §2.12).
	var written map[string]any
	_ = json.Unmarshal(raw, &written)
	for _, ev := range HookEvents {
		entry := written["hooks"].(map[string]any)[ev].([]any)[0].(map[string]any)
		got := entry["hooks"].([]any)[0].(map[string]any)["timeout"].(float64)
		want := float64(hookTimeoutSeconds)
		if ev == "PermissionRequest" {
			want = PermissionHookTimeoutSeconds
		}
		if got != want {
			t.Errorf("timeout di %s: %v, atteso %v", ev, got, want)
		}
	}
	// Idempotente: nessuna scrittura, nessuna copia.
	if changed, backup, err := Install(path, bin, state, now); err != nil || changed || backup != "" {
		t.Fatalf("seconda installazione: %v %q %v", changed, backup, err)
	}

	// Impostazioni dell'utente con altri hook e altre chiavi: restano intatte.
	user := map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"Stop":       []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/home/u/notifica.sh"}}}},
			"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "/home/u/guardia.sh"}}}},
		},
	}
	raw, _ = json.Marshal(user)
	_ = os.WriteFile(path, raw, 0o644)
	_ = os.Chmod(path, 0o644) // WriteFile non cambia i permessi di un file esistente
	changed, backup, err = Install(path, bin, state, now)
	if err != nil || !changed || backup == "" {
		t.Fatalf("installazione su impostazioni esistenti: %v %q %v", changed, backup, err)
	}
	if b, _ := os.ReadFile(backup); string(b) != string(raw) {
		t.Fatal("la copia di sicurezza non è il file di prima")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Fatalf("permessi cambiati: %o", fi.Mode().Perm())
	}
	after, _ := os.ReadFile(path)
	for _, s := range []string{`"model": "opus"`, "/home/u/notifica.sh", "/home/u/guardia.sh", `"matcher": "Bash"`} {
		if !strings.Contains(string(after), s) {
			t.Errorf("perso %s", s)
		}
	}

	// Remove toglie solo i nostri.
	changed, _, err = Remove(path, now)
	if err != nil || !changed {
		t.Fatalf("rimozione: %v %v", changed, err)
	}
	after, _ = os.ReadFile(path)
	if strings.Contains(string(after), "monsterterminal-connector") {
		t.Fatal("restano hook del connettore")
	}
	for _, s := range []string{"/home/u/notifica.sh", "/home/u/guardia.sh", `"model": "opus"`} {
		if !strings.Contains(string(after), s) {
			t.Errorf("la rimozione ha perso %s", s)
		}
	}
	if Installed(path) {
		t.Fatal("Installed dopo Remove = true")
	}
	if changed, _, _ := Remove(path, now); changed {
		t.Fatal("seconda rimozione ha scritto")
	}
}

func TestInstallazioneRifiutaCasiPericolosi(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "settings.json")
	now := time.Now()
	_ = os.WriteFile(path, []byte("{non json"), 0o600)
	if _, _, err := Install(path, "/bin/monsterterminal-connector", "/var/lib/m", now); err == nil {
		t.Fatal("impostazioni non JSON modificate")
	}
	if b, _ := os.ReadFile(path); string(b) != "{non json" {
		t.Fatal("il file non JSON è stato toccato")
	}
	_ = os.WriteFile(path, []byte(`{"hooks": []}`), 0o600)
	if _, _, err := Install(path, "/bin/monsterterminal-connector", "/var/lib/m", now); err == nil {
		t.Fatal(`"hooks" non oggetto modificato`)
	}
	_ = os.WriteFile(path, []byte(`{}`), 0o600)
	for _, bad := range []string{"/opt/my dir/monsterterminal-connector", "/bin/x;rm"} {
		if _, _, err := Install(path, bad, "/var/lib/m", now); err == nil {
			t.Errorf("percorso %q accettato", bad)
		}
	}
}

func TestTrovaClaude(t *testing.T) {
	home := shortDir(t)
	t.Setenv("PATH", home) // nessun claude nel PATH
	if _, ok := FindClaude(home); ok {
		t.Fatal("claude trovato dove non c'è")
	}
	p := filepath.Join(home, ".local", "bin", "claude")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	if got, ok := FindClaude(home); !ok || got != p {
		t.Fatalf("claude non trovato in ~/.local/bin: %q", got)
	}
}

func netDial(sock string) (interface {
	Write([]byte) (int, error)
	Close() error
}, error) {
	return dialUnix(sock)
}

// Gli hook si applicano nell'ordine in cui arrivano al socket: due eventi ravvicinati non si invertono.
func TestOrdineDegliHook(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "hook.sock")
	j := &fakeJournal{}
	tr := NewTracker(j, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Listen(ctx, sock, tr) }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 50; i++ {
		ev := "UserPromptSubmit"
		if i%2 == 1 {
			ev = "Stop"
		}
		if err := Send(sock, HookMessage{Event: ev, Session: "s"}, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(j.all()) < 50 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := j.all()
	if len(got) != 50 {
		t.Fatalf("%d stati, attesi 50", len(got))
	}
	for i, e := range got {
		want := protocol.StateWorking
		if i%2 == 1 {
			want = protocol.StateFinished
		}
		if e.State != want {
			t.Fatalf("stato %d = %s, atteso %s: ordine non rispettato", i, e.State, want)
		}
	}
}
