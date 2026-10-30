// register -check e register -replace verso un altro spazio di lavoro: esiti su stdout e codici di
// uscita che install.sh legge. Corrispondenza (#26): ADR 0409 A2, schemi 0.8.0.
package main

import (
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/register"
)

const tokB = "mti_installazione-spazio-b-0001"

func registeredInA(t *testing.T, h *harness) string {
	t.Helper()
	h.s.AddInstallTokenFor(installTok, time.Hour, "spazio-a")
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	if code := h.register(t); code != exitOK {
		t.Fatalf("registrazione in A: uscita %d, %s", code, h.stderr.String())
	}
	tok, err := register.LoadToken(h.tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestRegisterCheckEsiti(t *testing.T) {
	h := newHarness(t)
	tokA := registeredInA(t, h)

	h.s.AddInstallTokenFor("mti_installazione-spazio-a-0002", time.Hour, "spazio-a")
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = "mti_installazione-spazio-a-0002"
	h.stdout.b.Reset()
	if code := h.register(t, "-check"); code != exitOK || strings.TrimSpace(h.stdout.String()) != "same-tenant same-machine" {
		t.Fatalf("stesso spazio: uscita %d, stdout %q, stderr %s", code, h.stdout.String(), h.stderr.String())
	}

	h.s.AddInstallTokenFor(tokB, time.Hour, "spazio-b")
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = tokB
	h.stdout.b.Reset()
	if code := h.register(t, "-check"); code != exitOtherTenant || h.stdout.String() != "" ||
		!strings.Contains(h.stderr.String(), "altro spazio di lavoro") {
		t.Fatalf("altro spazio: uscita %d, stdout %q, stderr %s", code, h.stdout.String(), h.stderr.String())
	}
	if cur, _ := register.LoadToken(h.tokenFile); cur != tokA || !h.s.InstallTokenUsable(tokB) {
		t.Fatal("la verifica ha toccato il token dell'host o consumato quello di installazione")
	}
	if strings.Contains(h.stdout.String()+h.stderr.String(), tokB) {
		t.Fatal("il token di installazione è finito nell'output")
	}

	h.s.NoRegisterCheck = true
	if code := h.register(t, "-check"); code != exitCheckUnavailable {
		t.Fatalf("verifica non disponibile: uscita %d, stderr %s", code, h.stderr.String())
	}
}

func TestRegisterCheckSenzaHostRegistrato(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	if code := h.register(t, "-check"); code != exitError || h.s.Checks() != 0 {
		t.Fatalf("uscita %d, verifiche %d", code, h.s.Checks())
	}
}

func TestRegisterCheckConReplaceEUsoSbagliato(t *testing.T) {
	h := newHarness(t)
	if code := h.register(t, "-check", "-replace"); code != exitUsage {
		t.Fatalf("uscita %d", code)
	}
}

// -replace con il token di installazione di B su un host di A: rifiuto, file del token e giornale
// di A intatti (ADR 0409 A2: A non perde il server).
func TestRegisterReplaceVersoUnAltroSpazio(t *testing.T) {
	h := newHarness(t)
	tokA := registeredInA(t, h)
	h.s.AddInstallTokenFor(tokB, time.Hour, "spazio-b")
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = tokB
	if code := h.register(t, "-replace"); code != exitOtherTenant {
		t.Fatalf("uscita %d, stderr %s", code, h.stderr.String())
	}
	if cur, _ := register.LoadToken(h.tokenFile); cur != tokA {
		t.Fatal("il token dell'host di A è stato sovrascritto")
	}
	if !h.s.InstallTokenUsable(tokB) || len(h.s.Registrations()) != 1 {
		t.Fatal("token di B consumato o host nuovo creato")
	}
}
