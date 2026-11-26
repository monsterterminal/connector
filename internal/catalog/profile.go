package catalog

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ProfileMarker chiude ogni riga scritta da `catalog path-add` in ~/.profile: la rende riconoscibile
// (idempotenza, rilevamento) senza toccare il resto del file.
const ProfileMarker = "# monsterterminal-connector catalog path-add"

// ResolveDir porta una cartella di un sottocomando del catalogo a un percorso assoluto: assoluta resta
// com'è, relativa si intende dalla home dell'utente del connettore (senza shell `~` non si espande).
// Rifiuta `~`, i segmenti `..` che escono dalla home e i caratteri che in ~/.profile cambierebbero
// significato.
func ResolveDir(home, dir string) (string, error) {
	switch {
	case dir == "":
		return "", errors.New("cartella vuota")
	case strings.HasPrefix(dir, "~"):
		return "", fmt.Errorf("%q: senza shell `~` non si espande, scrivere il percorso relativo alla home", dir)
	case strings.ContainsAny(dir, "\"'`$\\\n\r\t;&|<>(){}*?[]!#"):
		return "", fmt.Errorf("%q: caratteri non ammessi in una cartella del catalogo", dir)
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir), nil
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("home dell'utente del connettore sconosciuta")
	}
	abs := filepath.Join(home, dir)
	if rel, err := filepath.Rel(home, abs); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%q esce dalla home", dir)
	}
	return abs, nil
}

// profileLine è la riga di ~/.profile per una cartella: relativa a $HOME se sta nella home.
func profileLine(home, abs string) string {
	p := abs
	if rel, err := filepath.Rel(home, abs); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		p = "$HOME/" + rel
	}
	return fmt.Sprintf(`export PATH="%s:$PATH" %s`, p, ProfileMarker)
}

// PathAdd aggiunge la cartella al PATH dell'utente con una riga in ~/.profile, una volta sola:
// se la riga c'è già non cambia nulla (changed=false). Il file nasce 0644 se manca.
func PathAdd(home, dir string) (changed bool, line string, err error) {
	abs, err := ResolveDir(home, dir)
	if err != nil {
		return false, "", err
	}
	line = profileLine(home, abs)
	path := filepath.Join(home, ".profile")
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, line, err
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) == line {
			return false, line, nil
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return false, line, err
	}
	prefix := ""
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + line + "\n"); err != nil {
		f.Close()
		return false, line, err
	}
	return true, line, f.Close()
}

// ProfileDirs sono le cartelle aggiunte con PathAdd, lette da ~/.profile: il PATH del servizio non le
// vede finché il connettore non riparte da una shell di login, il rilevamento sì.
func ProfileDirs(home string) []string {
	f, err := os.Open(filepath.Join(home, ".profile"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var dirs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if !strings.HasSuffix(l, ProfileMarker) {
			continue
		}
		p, ok := strings.CutPrefix(l, `export PATH="`)
		if !ok {
			continue
		}
		p, _, ok = strings.Cut(p, `:$PATH"`)
		if !ok {
			continue
		}
		if rest, ok := strings.CutPrefix(p, "$HOME/"); ok {
			p = filepath.Join(home, rest)
		}
		if filepath.IsAbs(p) {
			dirs = append(dirs, p)
		}
	}
	return dirs
}
