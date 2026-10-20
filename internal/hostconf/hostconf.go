// Package hostconf legge le impostazioni che decide il cliente sull'host (spec §1.11): il backend
// non le può cambiare, il connettore le riporta in hello. Il file lo scrive install.sh
// (packaging/install.sh, render_conf) e il cliente lo può modificare a mano.
//
// Formato: una riga `chiave=valore`, righe vuote e commenti `#` ignorati, spazi attorno a chiave
// e valore tollerati. Le chiavi booleane accettano yes/no, true/false, on/off, 1/0.
//
// Il file si legge in modo severo: una chiave sconosciuta o un valore non interpretabile sono un
// errore con il numero di riga, non un'impostazione ignorata in silenzio — `auto_updat=no` che
// lascia gli aggiornamenti accesi è esattamente ciò che il cliente non si aspetta.
package hostconf

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// Config sono le impostazioni dell'host.
type Config struct {
	// AutoUpdate: aggiornamenti automatici firmati (ADR 0409 D6). Default: acceso.
	AutoUpdate bool
	// CatalogInstall: installazione degli agenti dal catalogo (ADR 0409 D7). Default: acceso.
	CatalogInstall bool
	// User è l'utente con cui gira il connettore, come lo ha scritto l'installatore (solo informativo:
	// chi gira davvero lo dice il sistema).
	User string
	// Server è l'origine del servizio come l'ha scritta l'installatore (informativo: run usa -server).
	Server string
	// InstalledVersion è la versione installata da install.sh (informativo).
	InstalledVersion string
}

// Default è la configurazione di un host senza file: tutto acceso, come decide install.sh senza
// opzioni (--no-auto-update, --no-catalog).
func Default() Config { return Config{AutoUpdate: true, CatalogInstall: true} }

// Load legge il file. Se non esiste restituisce Default() e found=false: il file è facoltativo
// (una compilazione a mano non lo ha), ma se c'è deve essere interpretabile per intero.
func Load(path string) (cfg Config, found bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), false, nil
	}
	if err != nil {
		return Default(), false, err
	}
	defer f.Close()
	cfg, err = Parse(f)
	if err != nil {
		return Default(), true, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, true, nil
}

// Parse interpreta il contenuto del file.
func Parse(r io.Reader) (Config, error) {
	cfg := Default()
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Default(), fmt.Errorf("riga %d: attesa la forma chiave=valore", n)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if seen[key] {
			return Default(), fmt.Errorf("riga %d: chiave %q ripetuta", n, key)
		}
		seen[key] = true
		switch key {
		case "auto_update", "catalog_install":
			b, err := parseBool(value)
			if err != nil {
				return Default(), fmt.Errorf("riga %d: %s: %w", n, key, err)
			}
			if key == "auto_update" {
				cfg.AutoUpdate = b
			} else {
				cfg.CatalogInstall = b
			}
		case "user":
			cfg.User = value
		case "server":
			cfg.Server = value
		case "installed_version":
			cfg.InstalledVersion = value
		default:
			return Default(), fmt.Errorf("riga %d: chiave sconosciuta %q", n, key)
		}
	}
	if err := sc.Err(); err != nil {
		return Default(), err
	}
	return cfg, nil
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "yes", "true", "on", "1":
		return true, nil
	case "no", "false", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("valore %q non valido: atteso yes oppure no", v)
}
