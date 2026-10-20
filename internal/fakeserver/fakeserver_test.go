// Test del server finto montato su un listener proprio (Handler e Serve), come lo usa il banco di
// prova con systemd (packaging/banco). Corrispondenza (regola #26): spec §1.3 (registrazione col
// token monouso, 401 INSTALL_TOKEN_INVALID), ADR 0418 D5 (header X-MonsterTerminal-Schema registrato).
package fakeserver_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/register"
)

const installTok = "mti_banco-di-prova-0123456789abcdef"

func info() register.Info {
	return register.Info{MachineID: "0123456789abcdef0123456789abcdef", Hostname: "cliente-banco", OS: "linux",
		Distro: "ubuntu", DistroVersion: "24.04", Arch: "amd64", ConnectorVersion: "0.0.0-banco",
		RunAsUser: "monsterterminal"}
}

func TestHandlerRegistraConTokenValidoENotificaGliEventi(t *testing.T) {
	s := fakeserver.NewDetached()
	defer s.Close()
	var events []string
	s.Notify = func(e string) { events = append(events, e) }
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	s.AddInstallToken(installTok, time.Hour)

	c := register.Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	resp, err := c.Register(context.Background(), installTok, info())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.HostToken, "mth_") || resp.HostID == "" {
		t.Fatalf("risposta inattesa: %+v", resp)
	}
	if got := s.Registrations(); len(got) != 1 || got[0] != info() {
		t.Fatalf("registrazioni: %+v", got)
	}
	if s.InstallTokenUsable(installTok) {
		t.Fatal("il token di installazione deve essere consumato")
	}
	if len(events) != 1 || events[0] != "register" {
		t.Fatalf("eventi notificati: %v", events)
	}
	// Lo stesso token non registra una seconda volta (§1.3).
	var apiErr *register.APIError
	if _, err := c.Register(context.Background(), installTok, info()); !errors.As(err, &apiErr) || apiErr.Code != "INSTALL_TOKEN_INVALID" {
		t.Fatalf("seconda registrazione: %v", err)
	}
}

func TestHandlerRegistraLHeaderDelloSchema(t *testing.T) {
	s := fakeserver.NewDetached()
	defer s.Close()
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()
	s.AddInstallToken(installTok, time.Hour)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/register",
		strings.NewReader(`{"machineId":"0123456789abcdef0123456789abcdef","hostname":"h","os":"linux","distro":"ubuntu","distroVersion":"24.04","arch":"amd64","connectorVersion":"0.4.0","runAsUser":"monsterterminal","runAsRoot":false}`))
	req.Header.Set("Authorization", "Bearer "+installTok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(fakeserver.HeaderSchema, "0.4.0")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("stato %d", res.StatusCode)
	}
	if got := s.SchemaHeaders(); len(got) != 1 || got[0] != "0.4.0" {
		t.Fatalf("header dello schema registrati: %v", got)
	}
}

func TestServeSuUnListenerTLSProprio(t *testing.T) {
	s := fakeserver.NewDetached()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	// Il certificato è quello di httptest, usato qui solo come coppia chiave/certificato di prova.
	probe := httptest.NewUnstartedServer(http.NotFoundHandler())
	probe.StartTLS()
	defer probe.Close()
	cert := probe.TLS.Certificates[0]
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	done := make(chan error, 1)
	go func() { done <- s.Serve(tlsLn) }()

	c := register.Client{BaseURL: "https://" + ln.Addr().String(), HTTPClient: probe.Client()}
	if _, err := c.Register(context.Background(), installTok, info()); err != nil {
		t.Fatal(err)
	}
	if len(s.Registrations()) != 1 {
		t.Fatal("registrazione non arrivata al server servito con Serve")
	}
	tlsLn.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve deve restituire l'errore di chiusura del listener")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve non è tornato dopo la chiusura del listener")
	}
}

func TestNewRestaCompatibile(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AddInstallToken(installTok, time.Hour)
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	if _, err := c.Register(context.Background(), installTok, info()); err != nil {
		t.Fatal(err)
	}
	if got := s.SchemaHeaders(); len(got) != 1 || got[0] != "" {
		t.Fatalf("senza header lo schema registrato è vuoto, trovato %v", got)
	}
}
