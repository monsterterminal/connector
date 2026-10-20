// Test della registrazione (spec monsterterminal-saas.md §1.3, §1.20) contro il server finto.
package register_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/register"
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

// §1.3 (emendamento 2026-10-14): la sostituzione porta il token attuale nell'header; il backend
// revoca il vecchio host solo se il token è di un host vivo della stessa macchina.
func TestSostituzioneColTokenAttuale(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	s.AddInstallToken(installTok+"b", time.Hour)
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	first, err := c.Register(context.Background(), installTok, info())
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Replace(context.Background(), installTok+"b", first.HostToken, info())
	if err != nil {
		t.Fatal(err)
	}
	if h := s.ReplaceHeaders(); len(h) != 2 || h[0] != "" || h[1] != first.HostToken {
		t.Fatalf("header della sostituzione ricevuti: %q", h)
	}
	if !s.HostRevoked(first.HostID) || s.HostRevoked(second.HostID) {
		t.Fatal("la sostituzione doveva revocare il vecchio host e solo quello")
	}
}

// Senza prova di possesso non si sostituisce nulla: l'host nuovo nasce accanto al vecchio.
func TestRegistrazioneSenzaTokenAttualeNonSostituisce(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	s.AddInstallToken(installTok+"b", time.Hour)
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	first, _ := c.Register(context.Background(), installTok, info())
	if _, err := c.Register(context.Background(), installTok+"b", info()); err != nil {
		t.Fatal(err)
	}
	if s.HostRevoked(first.HostID) {
		t.Fatal("senza header il vecchio host non va revocato")
	}
}

func TestTokenAttualeMalformatoNonParte(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	if _, err := c.Replace(context.Background(), installTok, "non-un-token", info()); err == nil {
		t.Fatal("token attuale malformato accettato")
	}
	if len(s.Registrations()) != 0 || !s.InstallTokenUsable(installTok) {
		t.Fatal("la richiesta non doveva partire")
	}
}

// Corpo non valido: 400 BODY_INVALID e il token NON si consuma (ADR 0409 D3), si può riusare.
func TestCorpoNonValidoNonConsumaIlToken(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	bad := info()
	bad.MachineID = ""
	_, err := c.Register(context.Background(), installTok, bad)
	var api *register.APIError
	if !errors.As(err, &api) || api.Status != 400 || api.Code != "BODY_INVALID" {
		t.Fatalf("atteso 400 BODY_INVALID, trovato %v", err)
	}
	if _, err := c.Register(context.Background(), installTok, info()); err != nil {
		t.Fatalf("il token doveva essere ancora valido: %v", err)
	}
}

// Kill-switch della macchina: 403 HOST_DISABLED anche alla registrazione, token non consumato,
// e il messaggio dice cosa fare.
func TestMacchinaFermataRifiutataAllaRegistrazione(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	s.DisableMachine(info().MachineID)
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	_, err := c.Register(context.Background(), installTok, info())
	var api *register.APIError
	if !errors.As(err, &api) || api.Status != 403 || api.Code != "HOST_DISABLED" {
		t.Fatalf("atteso 403 HOST_DISABLED, trovato %v", err)
	}
	if !s.InstallTokenUsable(installTok) {
		t.Fatal("il token non doveva essere consumato")
	}
	if api.Hint() == "" || !strings.Contains(err.Error(), "kill-switch") {
		t.Fatalf("messaggio senza spiegazione: %v", err)
	}
}

func TestOgniCodiceNotoHaUnaSpiegazione(t *testing.T) {
	for _, code := range []string{"INSTALL_TOKEN_INVALID", "BODY_INVALID", "SERVER_LIMIT", "HOST_DISABLED", "BILLING_REQUIRED", "CONNECTOR_TOO_OLD", "RATE_LIMITED", "UNAVAILABLE"} {
		if (&register.APIError{Code: code}).Hint() == "" {
			t.Errorf("%s senza spiegazione", code)
		}
	}
	if (&register.APIError{Code: "SCONOSCIUTO"}).Hint() != "" {
		t.Error("un codice sconosciuto non deve inventare spiegazioni")
	}
}

// Barriera del billing (ADR 0418 D5, spec §3.13): 402 BILLING_REQUIRED alla registrazione, token
// non consumato, messaggio che manda l'owner alla pagina del billing e non parla di kill-switch.
func TestAbbonamentoMancanteRifiutatoAllaRegistrazione(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	s.RequireBilling()
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	_, err := c.Register(context.Background(), installTok, info())
	var api *register.APIError
	if !errors.As(err, &api) || api.Status != 402 || api.Code != "BILLING_REQUIRED" {
		t.Fatalf("atteso 402 BILLING_REQUIRED, trovato %v", err)
	}
	if !s.InstallTokenUsable(installTok) {
		t.Fatal("il token non doveva essere consumato")
	}
	msg := err.Error()
	if !strings.Contains(msg, "abbonamento") || !strings.Contains(msg, "/admin/billing") || strings.Contains(msg, "kill-switch") {
		t.Fatalf("messaggio sbagliato: %v", err)
	}
	// Tolta la barriera (il tenant paga), lo stesso token registra.
	s.AllowBilling()
	if _, err := c.Register(context.Background(), installTok, info()); err != nil {
		t.Fatalf("dopo il pagamento la registrazione doveva riuscire: %v", err)
	}
}

// La registrazione dichiara la versione degli schemi (§1.15): è l'header da cui il backend decide
// fra BILLING_REQUIRED e HOST_DISABLED.
func TestLaRegistrazioneDichiaraLaVersioneDegliSchemi(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	if _, err := c.Register(context.Background(), installTok, info()); err != nil {
		t.Fatal(err)
	}
	got := s.DeclaredSchemas()
	if len(got) != 1 || got[0] != protocol.SchemaVersion || protocol.SchemaVersion != "0.5.0" {
		t.Fatalf("X-MonsterTerminal-Schema dichiarati: %v, atteso [0.5.0]", got)
	}
}
