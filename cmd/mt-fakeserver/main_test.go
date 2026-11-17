// Test di mt-fakeserver: forma dei token coniati (connector-tokens.ts, install.sh
// valid_install_token), token-file 0600, status-file riscritto a ogni evento con le registrazioni.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/register"
)

func TestTokenConiatoHaLaFormaDiInstallSh(t *testing.T) {
	re := regexp.MustCompile(`^mti_[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		tok, err := mintInstallToken()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(tok) {
			t.Fatalf("token %q fuori forma", tok)
		}
		if seen[tok] {
			t.Fatalf("token ripetuto %q", tok)
		}
		seen[tok] = true
	}
}

func TestTokenFileScritto0600UnoPerRiga(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install-token")
	if err := publishTokens(path, []string{"mti_a", "mti_b"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("permessi %v, attesi 0600", st.Mode().Perm())
	}
	body, _ := os.ReadFile(path)
	if string(body) != "mti_a\nmti_b\n" {
		t.Fatalf("contenuto %q", body)
	}
}

func TestStatusFileRiscrittoAOgniRegistrazione(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	path := filepath.Join(t.TempDir(), "status.json")
	w := &statusWriter{path: path, s: s}
	s.Notify = w.onEvent
	if err := w.write(); err != nil {
		t.Fatal(err)
	}
	s.AddInstallToken("mti_banco-di-prova-0123456789abcdef", time.Hour)
	info := register.Info{MachineID: "0123456789abcdef0123456789abcdef", Hostname: "cliente-banco", OS: "linux",
		Distro: "ubuntu", DistroVersion: "24.04", Arch: "amd64", ConnectorVersion: "0.0.0-banco", RunAsUser: "monsterterminal"}
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	if _, err := c.Register(context.Background(), "mti_banco-di-prova-0123456789abcdef", info); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st status
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("status-file non è JSON: %v\n%s", err, body)
	}
	if len(st.Registrations) != 1 || st.Registrations[0].Hostname != "cliente-banco" ||
		st.Registrations[0].Arch != "amd64" || st.Registrations[0].ConnectorVersion != "0.0.0-banco" ||
		st.Registrations[0].Replace || st.Hellos != 0 || st.UpdatedAt == "" ||
		len(st.DeclaredSchemas) != 1 || st.DeclaredSchemas[0] != protocol.SchemaVersion {
		t.Fatalf("status-file inatteso: %+v", st)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("file temporaneo rimasto accanto allo status-file")
	}
}

// I file di comando del banco (ADR 0438): push senza manifest è un errore; auto-update cambia
// welcome e config.update; offer risponde ai prossimi update.check; lo status-file porta i campi
// dell'aggiornamento (updateChecks, updateNones, updateResults, restarts, commandsRun).
func TestComandiDelBanco(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	path := filepath.Join(t.TempDir(), "status.json")
	w := &statusWriter{path: path, s: s}
	s.Notify = w.onEvent
	if err := w.runCommand([]byte(`{"action":"push"}`)); err == nil {
		t.Fatal("push senza manifest accettato")
	}
	if err := w.runCommand([]byte(`{"action":"boh"}`)); err == nil {
		t.Fatal("azione sconosciuta accettata")
	}
	if err := w.runCommand([]byte(`{"action":"session.open"}`)); err == nil {
		t.Fatal("session.open senza sessione accettato")
	}
	if err := w.runCommand([]byte(`{"action":"auto-update","value":false}`)); err != nil {
		t.Fatal(err)
	}
	man := `{"arch":"amd64","minProtocol":1,"os":"linux","sha256":"` + strings.Repeat("0", 64) + `","url":"https://github.com/monsterterminal/connector/releases/download/v9.9.9/x","version":"9.9.9"}`
	if err := w.runCommand([]byte(`{"action":"offer","manifest":` + man + `,"signature":"` + strings.Repeat("A", 86) + `=="}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.runCommand([]byte(`{"action":"push","manifest":` + man + `,"signature":"` + strings.Repeat("A", 86) + `=="}`)); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st status
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("status-file non è JSON: %v\n%s", err, body)
	}
	if st.CommandsRun != 3 || st.UpdateResults == nil || st.Restarts == nil || st.UpdateChecks != 0 || st.UpdateNones != 0 {
		t.Fatalf("status-file inatteso: %+v", st)
	}
	for _, want := range []string{`"updateChecks": 0`, `"updateNones": 0`, `"updateResults": []`, `"restarts": []`, `"commandsRun": 3`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("status-file senza %s:\n%s", want, body)
		}
	}
	// pollCommands consuma i file in ordine di nome e li cancella.
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "2.json"), []byte(`{"action":"auto-update"}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "1.json"), []byte(`{"action":"auto-update","value":true}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "nota.txt"), []byte("non un comando"), 0o600)
	go pollCommands(dir, w, 10*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir(dir)
		if len(entries) == 1 && entries[0].Name() == "nota.txt" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("comandi non consumati: %v", entries)
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.mu.Lock()
	n := w.st.CommandsRun
	w.mu.Unlock()
	if n != 5 {
		t.Fatalf("comandi eseguiti %d, attesi 5", n)
	}
}

func TestCertificatoAutofirmatoSenzaFile(t *testing.T) {
	c, err := loadOrMakeCert("", "")
	if err != nil || len(c.Certificate) != 1 {
		t.Fatalf("certificato in memoria: %v", err)
	}
	if _, err := loadOrMakeCert(filepath.Join(t.TempDir(), "no.crt"), filepath.Join(t.TempDir(), "no.key")); err == nil {
		t.Fatal("file assenti devono dare errore")
	}
}
