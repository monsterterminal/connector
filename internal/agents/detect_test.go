package agents

// Test del rilevamento degli agenti (spec §1.11, §1.18, ADR 0410 A4 decisione 9): binario assente,
// presente con la versione, `--version` che non risponde entro il tetto.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func detectCfg(path string, home string) DetectConfig {
	return DetectConfig{Home: home, PathEnv: &path, SystemDirs: []string{}, Timeout: 300 * time.Millisecond}
}

func TestDetectAssente(t *testing.T) {
	got := Detect(detectCfg(t.TempDir(), t.TempDir()))
	if got == nil || len(got) != 0 {
		t.Fatalf("nessun agente: %v (deve essere una lista vuota, non nil: hello la vuole)", got)
	}
}

func TestDetectPresenteConVersione(t *testing.T) {
	bin, home := t.TempDir(), t.TempDir()
	script(t, bin, "claude", `echo "2.1.332 (Claude Code)"`)
	script(t, filepath.Join(home, ".npm-global", "bin"), "codex", `echo "codex-cli 0.40.0"`)
	extra := t.TempDir()
	script(t, extra, "opencode", `echo "v2.3.0-beta.1"`)
	// Un file non eseguibile con il nome giusto non è un agente.
	_ = os.WriteFile(filepath.Join(bin, "codex"), []byte("x"), 0o644)
	cfg := detectCfg(bin, home)
	cfg.ExtraDirs = []string{extra}
	got := Detect(cfg)
	want := []protocol.Agent{{Agent: "claude-code", Version: "2.1.332"}, {Agent: "codex", Version: "0.40.0"}, {Agent: "opencode", Version: "2.3.0-beta.1"}}
	if len(got) != len(want) {
		t.Fatalf("rilevati %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("agente %d: %+v, atteso %+v", i, got[i], want[i])
		}
	}
}

func TestDetectVersionCheNonRisponde(t *testing.T) {
	bin := t.TempDir()
	script(t, bin, "claude", "sleep 30")
	script(t, bin, "codex", "sleep 30")
	start := time.Now()
	got := Detect(detectCfg(bin, ""))
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("il tetto non ha fermato --version: %s (e le versioni si chiedono in parallelo)", d)
	}
	// Il binario c'è: l'agente si riporta, con la versione vuota.
	if len(got) != 2 || got[0] != (protocol.Agent{Agent: "claude-code"}) || got[1] != (protocol.Agent{Agent: "codex"}) {
		t.Fatalf("rilevati %v", got)
	}
}

func TestCommandVersionEsceConErrore(t *testing.T) {
	bin := t.TempDir()
	p := script(t, bin, "node", `echo "v22.3.0"; exit 1`)
	if v, ok := CommandVersion(p, time.Second); !ok || v != "22.3.0" {
		t.Fatalf("versione stampata con uscita 1: %q %v", v, ok)
	}
	q := script(t, bin, "rotto", `echo "boh" >&2; exit 1`)
	if v, ok := CommandVersion(q, time.Second); ok || v != "" {
		t.Fatalf("senza versione: %q %v", v, ok)
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.332 (Claude Code)": "2.1.332", "v22.3.0\n": "22.3.0", "codex-cli 0.40.0": "0.40.0",
		"opencode 1.0.0-rc.2 build": "1.0.0-rc.2", "nessuna": "", "1.2": "",
	} {
		if got := ParseVersion(in); got != want {
			t.Errorf("ParseVersion(%q) = %q, atteso %q", in, got, want)
		}
	}
}

func TestFindCommandIgnoraIPercorsiRelativiEINomiConBarra(t *testing.T) {
	bin := t.TempDir()
	script(t, bin, "node", "echo 1.0.0")
	if _, ok := FindCommand(detectCfg("relativo:"+bin, ""), "../node"); ok {
		t.Fatal("nome con barra accettato")
	}
	if p, ok := FindCommand(detectCfg("relativo:"+bin, ""), "node"); !ok || p != filepath.Join(bin, "node") {
		t.Fatalf("FindCommand: %q %v", p, ok)
	}
}
