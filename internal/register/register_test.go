// Test della registrazione (spec monsterterminal-saas.md §1.3, §1.20) contro il server finto.
package register_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ciarasino/monsterterminal-connector/internal/fakeserver"
	"github.com/ciarasino/monsterterminal-connector/internal/register"
)

const installTok = "mti_installazione-di-prova-0001"

func info() register.Info {
	return register.Info{MachineID: "0123456789abcdef0123456789abcdef", Hostname: "srv-prova", OS: "linux",
		Distro: "ubuntu", DistroVersion: "24.04", Arch: "amd64", ConnectorVersion: "0.1.0-dev",
		RunAsUser: "monsterterminal"}
}

func TestRegistrazioneRiuscitaSalvaIlTokenCon0600(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	resp, err := c.Register(context.Background(), installTok, info())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Registrations(); len(got) != 1 || got[0] != info() {
		t.Fatalf("il server ha ricevuto %+v", got)
	}
	path := filepath.Join(t.TempDir(), "monsterterminal", "host-token")
	if err := register.SaveToken(path, resp.HostToken); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("permessi del file %v, attesi 0600", st.Mode().Perm())
	}
	dst, _ := os.Stat(filepath.Dir(path))
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("permessi della cartella %v, attesi 0700", dst.Mode().Perm())
	}
	got, err := register.LoadToken(path)
	if err != nil || got != resp.HostToken {
		t.Fatalf("token riletto %q, %v", got, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("file temporanei rimasti: %d voci", len(entries))
	}
}

func TestTokenDiInstallazioneMonouso(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	if _, err := c.Register(context.Background(), installTok, info()); err != nil {
		t.Fatal(err)
	}
	_, err := c.Register(context.Background(), installTok, info())
	var api *register.APIError
	if !errors.As(err, &api) || api.Status != 401 || api.Code != "INSTALL_TOKEN_INVALID" {
		t.Fatalf("secondo uso: atteso 401 INSTALL_TOKEN_INVALID, ottenuto %v", err)
	}
}

func TestTokenDiInstallazioneScaduto(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, -time.Second)
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	_, err := c.Register(context.Background(), installTok, info())
	var api *register.APIError
	if !errors.As(err, &api) || api.Code != "INSTALL_TOKEN_INVALID" {
		t.Fatalf("atteso INSTALL_TOKEN_INVALID, ottenuto %v", err)
	}
}

func TestTokenMalformatoNonParte(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	if _, err := c.Register(context.Background(), "abc", info()); !errors.Is(err, register.ErrInstallTokenFormat) {
		t.Fatalf("atteso ErrInstallTokenFormat, ottenuto %v", err)
	}
	if len(s.Registrations()) != 0 {
		t.Fatal("la richiesta è partita lo stesso")
	}
}

func TestRispostaConCampoSconosciutoRifiutata(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.ExtraRegisterField = true
	s.AddInstallToken(installTok, time.Hour)
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	if _, err := c.Register(context.Background(), installTok, info()); err == nil {
		t.Fatal("risposta con un campo in più accettata")
	}
}

func TestCertificatoNonFidatoRifiutato(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	c := register.Client{BaseURL: s.BaseURL()} // client di default: non si fida del certificato autofirmato
	if _, err := c.Register(context.Background(), installTok, info()); err == nil {
		t.Fatal("registrazione riuscita verso un certificato non fidato")
	}
	if len(s.Registrations()) != 0 {
		t.Fatal("il corpo è arrivato al server nonostante il certificato non fidato")
	}
}

func TestServizioNonHttpsRifiutato(t *testing.T) {
	c := register.Client{BaseURL: "http://connect.example"}
	if _, err := c.Register(context.Background(), installTok, info()); err == nil {
		t.Fatal("servizio http accettato")
	}
}

func TestLoadTokenRifiutaPermessiLarghi(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-token")
	if err := os.WriteFile(path, []byte("mth_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := register.LoadToken(path); !errors.Is(err, register.ErrTokenFilePermissions) {
		t.Fatalf("atteso ErrTokenFilePermissions, ottenuto %v", err)
	}
}
