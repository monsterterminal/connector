// Corrispondenza (regola #26) con ADR 0409 A2 e gli schemi 0.8.0 (http/register-check-*.json,
// HOST_OTHER_TENANT in http/error.json): un server registrato in uno spazio di lavoro non passa a un
// altro spazio; la verifica non consuma mai il token di installazione.
package register_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/register"
)

const tokB = "mti_installazione-spazio-b-0001"

// registeredInA registra info() nello spazio A e restituisce client, id e token dell'host.
func registeredInA(t *testing.T, s *fakeserver.Server) (register.Client, string, string) {
	t.Helper()
	s.AddInstallTokenFor(installTok, time.Hour, "spazio-a")
	c := register.Client{BaseURL: s.BaseURL(), HTTPClient: s.Client()}
	r, err := c.Register(context.Background(), installTok, info())
	if err != nil {
		t.Fatal(err)
	}
	return c, r.HostID, r.HostToken
}

// A2 (1): sostituzione verso un altro spazio → 409 HOST_OTHER_TENANT; token di B non consumato,
// host di A non revocato, nessun host nuovo.
func TestReplaceVersoUnAltroSpazioRifiutata(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	c, idA, tokA := registeredInA(t, s)
	s.AddInstallTokenFor(tokB, time.Hour, "spazio-b")
	_, err := c.Replace(context.Background(), tokB, tokA, info())
	var api *register.APIError
	if !errors.As(err, &api) || api.Status != 409 || api.Code != "HOST_OTHER_TENANT" {
		t.Fatalf("atteso 409 HOST_OTHER_TENANT, ottenuto %v", err)
	}
	if api.Hint() == "" {
		t.Error("HOST_OTHER_TENANT senza spiegazione per l'amministratore")
	}
	if !s.InstallTokenUsable(tokB) || s.HostRevoked(idA) || len(s.Registrations()) != 1 {
		t.Fatal("il rifiuto ha cambiato qualcosa: token consumato, host revocato o host nuovo")
	}
}

// A2 (2): la verifica, nei tre esiti del contratto, senza mai consumare il token.
func TestVerificaDelloSpazio(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	c, idA, tokA := registeredInA(t, s)

	s.AddInstallTokenFor(tokB, time.Hour, "spazio-b")
	_, err := c.Check(context.Background(), tokB, tokA, info().MachineID)
	var api *register.APIError
	if !errors.As(err, &api) || api.Status != 409 || api.Code != "HOST_OTHER_TENANT" {
		t.Fatalf("altro spazio: atteso 409 HOST_OTHER_TENANT, ottenuto %v", err)
	}

	tokA2 := "mti_installazione-spazio-a-0002"
	s.AddInstallTokenFor(tokA2, time.Hour, "spazio-a")
	res, err := c.Check(context.Background(), tokA2, tokA, info().MachineID)
	if err != nil || res.Result != register.CheckSameTenant || res.SameMachine == nil || !*res.SameMachine {
		t.Fatalf("stesso spazio, stessa macchina: %+v %v", res, err)
	}
	res, err = c.Check(context.Background(), tokA2, tokA, "ffffffffffffffffffffffffffffffff")
	if err != nil || res.Result != register.CheckSameTenant || res.SameMachine == nil || *res.SameMachine {
		t.Fatalf("stesso spazio, altra macchina: %+v %v", res, err)
	}

	s.Revoke(idA)
	res, err = c.Check(context.Background(), tokB, tokA, info().MachineID)
	if err != nil || res.Result != register.CheckNotRegistered || res.SameMachine != nil {
		t.Fatalf("host revocato: %+v %v", res, err)
	}
	if !s.InstallTokenUsable(tokB) || !s.InstallTokenUsable(tokA2) || s.Checks() != 4 {
		t.Fatal("la verifica ha consumato un token di installazione")
	}
}

// Un servizio che non conosce la verifica (404 di nginx, pagina HTML) è «non disponibile», mai un
// via libera.
func TestVerificaNonDisponibile(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	c, _, tokA := registeredInA(t, s)
	s.NoRegisterCheck = true
	s.AddInstallTokenFor(tokB, time.Hour, "spazio-b")
	_, err := c.Check(context.Background(), tokB, tokA, info().MachineID)
	if !errors.Is(err, register.ErrCheckUnavailable) {
		t.Fatalf("atteso ErrCheckUnavailable, ottenuto %v", err)
	}
}
