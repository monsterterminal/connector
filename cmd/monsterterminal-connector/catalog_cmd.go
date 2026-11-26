package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/monsterterminal/connector/internal/agents"
	"github.com/monsterterminal/connector/internal/catalog"
	"github.com/monsterterminal/connector/internal/protocol"
)

// catalogArgs è il numero di argomenti posizionali di `catalog <azione> …`, azione compresa.
func catalogArgs(action string) int {
	switch action {
	case "path-add":
		return 2
	case "extract":
		return 3
	}
	return 1
}

// cmdCatalog esegue i due aiutanti delle ricette (§1.9): `catalog path-add <cartella>` aggiunge una
// riga idempotente in ~/.profile, `catalog extract <archivio> <cartella>` apre un tar.gz o uno zip.
// Le cartelle e l'archivio, se relativi, si intendono dalla home dell'utente del connettore.
func (a app) cmdCatalog(args []string) error {
	home, err := a.home()
	if err != nil {
		return fmt.Errorf("catalog: home dell'utente: %w", err)
	}
	switch args[0] {
	case "path-add":
		changed, line, err := catalog.PathAdd(home, args[1])
		if err != nil {
			return fmt.Errorf("catalog path-add: %w", err)
		}
		if changed {
			fmt.Fprintf(a.stdout, "Aggiunto a %s: %s\n", filepath.Join(home, ".profile"), line)
		} else {
			fmt.Fprintf(a.stdout, "Già presente in %s: %s\n", filepath.Join(home, ".profile"), line)
		}
		return nil
	case "extract":
		archive, err := catalog.ResolveDir(home, args[1])
		if err != nil {
			return fmt.Errorf("catalog extract: archivio: %w", err)
		}
		dest, err := catalog.ResolveDir(home, args[2])
		if err != nil {
			return fmt.Errorf("catalog extract: cartella: %w", err)
		}
		if err := catalog.Extract(archive, dest, catalog.ExtractLimits{}); err != nil {
			return fmt.Errorf("catalog extract: %w", err)
		}
		fmt.Fprintf(a.stdout, "Estratto %s in %s\n", filepath.Base(archive), dest)
		return nil
	}
	return errors.New("catalog: azione sconosciuta (path-add CARTELLA oppure extract ARCHIVIO CARTELLA)")
}

// detect rileva gli agenti dell'host: nel PATH del servizio, nelle cartelle aggiunte con path-add e
// nei percorsi noti (§1.11).
func (a app) detect(home string) []protocol.Agent {
	if a.detectAgents != nil {
		return a.detectAgents(home)
	}
	return agents.Detect(agents.DetectConfig{Home: home, ExtraDirs: catalog.ProfileDirs(home)})
}

// lookup cerca un comando per i requires di una ricetta e ne legge la versione.
func (a app) lookup(home, name string) (string, bool) {
	if a.lookupVersion != nil {
		return a.lookupVersion(home, name)
	}
	p, ok := agents.FindCommand(agents.DetectConfig{Home: home, ExtraDirs: catalog.ProfileDirs(home)}, name)
	if !ok {
		return "", false
	}
	v, _ := agents.CommandVersion(p, agents.VersionTimeout)
	return v, true
}

// catalogEnv è l'ambiente dei passi run: quello del servizio, con HOME e con il PATH allungato dalle
// cartelle della home in cui una ricetta installa (path-add e npm con prefisso utente), così un passo
// trova ciò che il precedente ha appena installato.
func catalogEnv(home string) []string {
	env := os.Environ()
	if home == "" {
		return env
	}
	extra := append(catalog.ProfileDirs(home), filepath.Join(home, ".npm-global", "bin"), filepath.Join(home, ".local", "bin"))
	out := make([]string, 0, len(env)+2)
	path, hasHome := "", false
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "PATH="):
			path = strings.TrimPrefix(kv, "PATH=")
		case strings.HasPrefix(kv, "HOME="):
			hasHome = true
			out = append(out, kv)
		default:
			out = append(out, kv)
		}
	}
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	out = append(out, "PATH="+path+":"+strings.Join(extra, ":"))
	if !hasHome {
		out = append(out, "HOME="+home)
	}
	return out
}
