package hostconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Corrispondenza alla spec §1.11 e ADR 0409 D6/D7: le impostazioni decise dal cliente sull'host
// (aggiornamenti automatici, installazione dal catalogo, utente) vengono dal file del connettore,
// con default «acceso» quando il file non dice altro.

func TestFileDiInstallSh(t *testing.T) {
	// Esattamente ciò che scrive packaging/install.sh (render_conf).
	in := "# MonsterTerminal connector — impostazioni decise dal cliente su questo host (§1.11).\n" +
		"# Cambiarle qui e riavviare il servizio: il backend non le può modificare.\n" +
		"user=monsterterminal\nauto_update=no\ncatalog_install=yes\n" +
		"server=https://connect.monsterterminal.com\ninstalled_version=0.2.0\n"
	cfg, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{AutoUpdate: false, CatalogInstall: true, User: "monsterterminal",
		Server: "https://connect.monsterterminal.com", InstalledVersion: "0.2.0"}
	if cfg != want {
		t.Fatalf("letto %+v, atteso %+v", cfg, want)
	}
}

func TestDefaultTuttoAcceso(t *testing.T) {
	cfg, err := Parse(strings.NewReader(""))
	if err != nil || cfg != Default() || !cfg.AutoUpdate || !cfg.CatalogInstall {
		t.Fatalf("file vuoto: %+v, %v", cfg, err)
	}
}

func TestFormeDeiBooleaniESpazi(t *testing.T) {
	for _, in := range []string{
		"auto_update = off\ncatalog_install=FALSE",
		"  auto_update=0  \n\n catalog_install = No \n",
	} {
		cfg, err := Parse(strings.NewReader(in))
		if err != nil || cfg.AutoUpdate || cfg.CatalogInstall {
			t.Fatalf("%q: %+v, %v", in, cfg, err)
		}
	}
}

func TestErroriConNumeroDiRiga(t *testing.T) {
	cases := map[string]string{
		"# ok\nauto_updat=no":               `riga 2: chiave sconosciuta "auto_updat"`,
		"auto_update=forse":                 `riga 1: auto_update: valore "forse" non valido`,
		"user=a\ncatalog_install":           "riga 2: attesa la forma chiave=valore",
		"auto_update=no\n\nauto_update=yes": `riga 3: chiave "auto_update" ripetuta`,
		"catalog_install=":                  `riga 1: catalog_install: valore "" non valido`,
	}
	for in, want := range cases {
		cfg, err := Parse(strings.NewReader(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: errore %v, atteso %q", in, err, want)
		}
		if cfg != Default() {
			t.Fatalf("%q: con errore la configurazione deve essere quella di default, non %+v", in, cfg)
		}
	}
}

func TestLoadFileAssenteEPresente(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "connector.conf")
	cfg, found, err := Load(p)
	if err != nil || found || cfg != Default() {
		t.Fatalf("assente: %+v %v %v", cfg, found, err)
	}
	if err := os.WriteFile(p, []byte("auto_update=no\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, found, err = Load(p)
	if err != nil || !found || cfg.AutoUpdate || !cfg.CatalogInstall {
		t.Fatalf("presente: %+v %v %v", cfg, found, err)
	}
	if err := os.WriteFile(p, []byte("boh=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, found, err = Load(p)
	if err == nil || !found || !strings.Contains(err.Error(), p) {
		t.Fatalf("malformato: l'errore deve nominare il file: %v (found %v)", err, found)
	}
}
