package agents

// Rilevamento degli agenti installati sull'host (spec §1.11, ADR 0410 A4 decisione 9): il binario nel
// PATH dell'utente del connettore o nei percorsi noti, la versione da `<binario> --version` con un
// tetto di 5 secondi. Si rifà a ogni hello e alla fine di un'installazione dal catalogo riuscita;
// nessun evento agent.detected in questa fase.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

// VersionTimeout è il tetto di `<binario> --version`.
const VersionTimeout = 5 * time.Second

// KnownAgent è un agente che il connettore sa riconoscere: l'id del protocollo e il nome del binario.
type KnownAgent struct {
	ID     string
	Binary string
}

// KnownAgents sono gli agenti del catalogo della Fase 3, nell'ordine in cui si riportano in hello.
var KnownAgents = []KnownAgent{
	{ID: "claude-code", Binary: "claude"},
	{ID: "codex", Binary: "codex"},
	{ID: "opencode", Binary: "opencode"},
}

// DetectConfig configura il rilevamento.
type DetectConfig struct {
	// Home è la cartella dell'utente del connettore; vuota = nessun percorso nella home.
	Home string
	// PathEnv è il PATH in cui cercare; nil = quello del processo.
	PathEnv *string
	// ExtraDirs sono cartelle in più (quelle aggiunte con `catalog path-add`, che il PATH del servizio
	// non vede finché non riparte da una shell di login).
	ExtraDirs []string
	// SystemDirs sono i percorsi noti fuori dalla home; nil = DefaultSystemDirs.
	SystemDirs []string
	// Timeout di --version; zero = VersionTimeout.
	Timeout time.Duration
}

// DefaultSystemDirs sono i percorsi noti di sistema in cui un agente può stare fuori dal PATH del servizio.
var DefaultSystemDirs = []string{"/usr/local/bin", "/usr/bin"}

// homeDirs sono i percorsi noti nella home, relativi: npm con prefisso utente, i binari utente, il
// vecchio installer di Claude Code, l'installer nativo di OpenCode.
var homeDirs = []string{
	".npm-global/bin", ".local/bin", ".claude/local", ".opencode/bin", ".local/opencode",
}

// FindCommand cerca un eseguibile per nome: nel PATH, poi nelle cartelle in più, poi nei percorsi noti
// della home e di sistema. name non contiene mai una barra (lo garantisce lo schema della ricetta).
func FindCommand(cfg DetectConfig, name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, '/') {
		return "", false
	}
	path := os.Getenv("PATH")
	if cfg.PathEnv != nil {
		path = *cfg.PathEnv
	}
	dirs := filepath.SplitList(path)
	dirs = append(dirs, cfg.ExtraDirs...)
	if cfg.Home != "" {
		for _, d := range homeDirs {
			dirs = append(dirs, filepath.Join(cfg.Home, d))
		}
	}
	sys := cfg.SystemDirs
	if sys == nil {
		sys = DefaultSystemDirs
	}
	dirs = append(dirs, sys...)
	for _, d := range dirs {
		if d == "" || !filepath.IsAbs(d) {
			continue // una voce relativa del PATH dipenderebbe dalla cartella corrente
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

var versionRE = regexp.MustCompile(`\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?`)

// ParseVersion estrae la prima versione x.y.z (con l'eventuale pre-release) da un testo come
// «2.1.332 (Claude Code)», «v22.3.0» o «codex-cli 0.40.0». Vuota se non ce n'è.
func ParseVersion(out string) string {
	v := versionRE.FindString(out)
	if len(v) > 64 {
		v = v[:64]
	}
	return v
}

// CommandVersion esegue `<bin> --version` senza shell, con il tetto di timeout (e uccide il gruppo
// di processi oltre il tetto), e ne estrae la versione. ok=false se non risponde in tempo o esce
// con errore senza stampare una versione.
func CommandVersion(bin string, timeout time.Duration) (string, bool) {
	if timeout <= 0 {
		timeout = VersionTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: 4096}
	cmd.Stderr = cmd.Stdout
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", false
	}
	v := ParseVersion(out.String())
	if err != nil && v == "" {
		return "", false
	}
	return v, v != ""
}

type limitedWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		if len(p) > room {
			l.w.Write(p[:room])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
}

// Detect riporta gli agenti trovati, nell'ordine di KnownAgents. Un agente trovato la cui versione
// non risponde entro il tetto si riporta con versione vuota: il binario c'è, il pannello non mostra
// «Installa…». Le versioni si chiedono in parallelo: il rilevamento dura al più un tetto.
func Detect(cfg DetectConfig) []protocol.Agent {
	type found struct {
		id, bin, version string
	}
	var hits []*found
	for _, a := range KnownAgents {
		if p, ok := FindCommand(cfg, a.Binary); ok {
			hits = append(hits, &found{id: a.ID, bin: p})
		}
	}
	var wg sync.WaitGroup
	for _, h := range hits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.version, _ = CommandVersion(h.bin, cfg.Timeout)
		}()
	}
	wg.Wait()
	out := make([]protocol.Agent, 0, len(hits))
	for _, h := range hits {
		out = append(out, protocol.Agent{Agent: h.id, Version: h.version})
	}
	return out
}
