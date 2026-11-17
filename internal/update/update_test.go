// Test della verifica di un manifest (ADR 0438 D3 punto 1, D6). La firma si prova con una coppia
// Ed25519 generata qui: la privata di mt-updates-2026 non esiste su questa macchina, per contratto;
// con la chiave incorporata una firma di prova non deve MAI valere.
package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/signing"
)

func testManifest() protocol.UpdateManifest {
	return protocol.UpdateManifest{
		Arch: "amd64", MinProtocol: 1, OS: "linux", SHA256: strings.Repeat("ab", 32),
		URL:     ReleasesPrefix + "v0.7.0/monsterterminal-connector_0.7.0_linux_amd64",
		Version: "0.7.0",
	}
}

func testPlatform() Platform {
	return Platform{OS: "linux", Arch: "amd64", Protocol: 1, Version: "0.6.1"}
}

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func sign(t *testing.T, priv ed25519.PrivateKey, m protocol.UpdateManifest) []byte {
	t.Helper()
	canon, err := Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	return ed25519.Sign(priv, canon)
}

// La forma canonica è ESATTAMENTE quella che release.yml scrive con printf: chiavi in ordine
// alfabetico, senza spazi, security solo se presente, nessun escape.
func TestFormaCanonicaComeReleaseYml(t *testing.T) {
	m := testManifest()
	want := `{"arch":"amd64","minProtocol":1,"os":"linux","sha256":"` + strings.Repeat("ab", 32) + `","url":"` + m.URL + `","version":"0.7.0"}`
	got, err := Canonical(m)
	if err != nil || string(got) != want {
		t.Fatalf("canonica senza security:\n%s\nattesa\n%s (%v)", got, want, err)
	}
	sec := true
	m.Security = &sec
	wantSec := strings.Replace(want, `"os":"linux",`, `"os":"linux","security":true,`, 1)
	if got, _ := Canonical(m); string(got) != wantSec {
		t.Fatalf("canonica con security:\n%s\nattesa\n%s", got, wantSec)
	}
	// security:false esplicito resta nella forma: ciò che è stato firmato si riproduce com'è.
	sec = false
	if got, _ := Canonical(m); !strings.Contains(string(got), `"security":false,`) {
		t.Fatalf("security false perso: %s", got)
	}
	// Gli URL con & o < non si escapano (printf della CI non lo fa).
	m.Security = nil
	m.URL = ReleasesPrefix + "v0.7.0/a&b<c"
	if got, _ := Canonical(m); !strings.Contains(string(got), `"url":"`+m.URL+`"`) {
		t.Fatalf("escape HTML nella canonica: %s", got)
	}
}

func TestVerificaAccettaSoloLaFirmaDellaChiaveGiusta(t *testing.T) {
	pub, priv := keypair(t)
	m := testManifest()
	sig := sign(t, priv, m)
	if err := Verify(m, sig, pub, testPlatform()); err != nil {
		t.Fatalf("manifest buono rifiutato: %v", err)
	}
	// Con security la firma copre anche quel campo.
	sec := true
	m.Security = &sec
	if err := Verify(m, sig, pub, testPlatform()); ReasonOf(err, "") != ReasonBadSignature {
		t.Fatalf("security aggiunto dopo la firma: %v", err)
	}
	if err := Verify(m, sign(t, priv, m), pub, testPlatform()); err != nil {
		t.Fatalf("manifest di sicurezza firmato rifiutato: %v", err)
	}
	m.Security = nil
	other, _ := keypair(t)
	if err := Verify(m, sig, other, testPlatform()); ReasonOf(err, "") != ReasonBadSignature {
		t.Fatalf("altra chiave: %v", err)
	}
	if err := Verify(m, sig[:63], pub, testPlatform()); ReasonOf(err, "") != ReasonBadSignature {
		t.Fatalf("firma corta: %v", err)
	}
	altered := m
	altered.Version = "0.7.1"
	if err := Verify(altered, sig, pub, testPlatform()); ReasonOf(err, "") != ReasonBadSignature {
		t.Fatalf("manifest alterato: %v", err)
	}
	// La chiave incorporata mt-updates-2026: nessuno qui ha la privata, quindi nessuna firma vale.
	if err := VerifyWithEmbeddedKey(m, sig, testPlatform()); ReasonOf(err, "") != ReasonBadSignature {
		t.Fatalf("firma di prova accettata dalla chiave incorporata: %v", err)
	}
	// E la chiave delle ricette non vale per un aggiornamento (§1.17: due chiavi, mai scambiate).
	recipes, _ := signing.PublicKey(signing.Recipes)
	if err := Verify(m, sig, recipes, testPlatform()); ReasonOf(err, "") != ReasonBadSignature {
		t.Fatalf("chiave delle ricette: %v", err)
	}
}

// L'ordine dei controlli e i motivi: la firma prima di tutto, poi piattaforma, protocollo, versione,
// URL (ADR 0438 D3 punto 1).
func TestVerificaMotiviEOrdine(t *testing.T) {
	pub, priv := keypair(t)
	p := testPlatform()
	cases := []struct {
		name   string
		mod    func(*protocol.UpdateManifest)
		plat   func(*Platform)
		reason string
	}{
		{"altra architettura", func(m *protocol.UpdateManifest) { m.Arch = "arm64" }, nil, ReasonWrongPlatform},
		{"altro sistema", func(m *protocol.UpdateManifest) { m.OS = "darwin" }, nil, ReasonWrongPlatform},
		{"protocollo troppo nuovo", func(m *protocol.UpdateManifest) { m.MinProtocol = 2 }, nil, ReasonProtocolTooOld},
		{"stessa versione", func(m *protocol.UpdateManifest) { m.Version = "0.6.1" }, nil, ReasonNotNewer},
		{"versione precedente (mai downgrade da manifest)", func(m *protocol.UpdateManifest) { m.Version = "0.6.0" }, nil, ReasonNotNewer},
		{"pre-release della versione in esecuzione", func(m *protocol.UpdateManifest) { m.Version = "0.6.1-rc.1" }, nil, ReasonNotNewer},
		{"pre-release della successiva: maggiore", func(m *protocol.UpdateManifest) { m.Version = "0.7.0-rc.1" }, nil, ""},
		{"release dopo la sua pre-release in esecuzione", nil, func(p *Platform) { p.Version = "0.7.0-rc.1" }, ""},
		{"pre-release numerica minore", func(m *protocol.UpdateManifest) { m.Version = "0.7.0-rc.2" }, func(p *Platform) { p.Version = "0.7.0-rc.10" }, ReasonNotNewer},
		{"url di un altro repository", func(m *protocol.UpdateManifest) {
			m.URL = "https://github.com/altro/connector/releases/download/v0.7.0/x"
		}, nil, ReasonBadURL},
		{"url con host simile", func(m *protocol.UpdateManifest) {
			m.URL = "https://github.com.evil.example/monsterterminal/connector/releases/download/v0.7.0/x"
		}, nil, ReasonBadURL},
		{"url col solo prefisso", func(m *protocol.UpdateManifest) { m.URL = ReleasesPrefix }, nil, ReasonBadURL},
		{"url http", func(m *protocol.UpdateManifest) { m.URL = "http" + strings.TrimPrefix(m.URL, "https") }, nil, ReasonBadURL},
		{"url di un banco", func(m *protocol.UpdateManifest) { m.URL = "https://releases.banco/releases/download/v0.7.0/x" }, nil, ReasonBadURL},
		{"piattaforma e protocollo sbagliati insieme: vince la piattaforma", func(m *protocol.UpdateManifest) { m.Arch = "arm64"; m.MinProtocol = 9 }, nil, ReasonWrongPlatform},
	}
	for _, c := range cases {
		m := testManifest()
		if c.mod != nil {
			c.mod(&m)
		}
		pl := p
		if c.plat != nil {
			c.plat(&pl)
		}
		err := Verify(m, sign(t, priv, m), pub, pl)
		if got := ReasonOf(err, ""); (err == nil) != (c.reason == "") || (err != nil && got != c.reason) {
			t.Errorf("%s: %v (motivo %q, atteso %q)", c.name, err, got, c.reason)
		}
	}
	// Un manifest NON firmato con piattaforma sbagliata dice bad-signature, non wrong-platform: un
	// manifest non autentico non racconta niente di sé.
	m := testManifest()
	m.Arch = "arm64"
	if err := Verify(m, make([]byte, 64), pub, p); ReasonOf(err, "") != ReasonBadSignature {
		t.Fatalf("non firmato con piattaforma sbagliata: %v", err)
	}
}

// Gli interruttori (ADR 0438 D6): il file locale vince sempre, security ignora solo il backend.
func TestDecideInterruttori(t *testing.T) {
	cases := []struct {
		host, backend, security bool
		want                    string
	}{
		{true, true, false, ""},
		{true, true, true, ""},
		{true, false, false, ReasonTenantSwitch},
		{true, false, true, ""}, // security ignora l'interruttore del backend
		{false, true, false, ReasonHostConfig},
		{false, true, true, ReasonHostConfig}, // ma MAI il file locale
		{false, false, true, ReasonHostConfig},
		{false, false, false, ReasonHostConfig},
	}
	for _, c := range cases {
		if got := Decide(c.host, c.backend, c.security); got != c.want {
			t.Errorf("host=%v backend=%v security=%v: %q, atteso %q", c.host, c.backend, c.security, got, c.want)
		}
	}
}

func TestReasonOf(t *testing.T) {
	if r := ReasonOf(errors.New("x"), ""); r != ReasonDownloadFailed {
		t.Fatalf("errore generico: %s", r)
	}
	if r := ReasonOf(errors.New("x"), ReasonBadSignature); r != ReasonBadSignature {
		t.Fatalf("fallback: %s", r)
	}
	rej := reject(ReasonTooLarge, "troppo")
	if r := ReasonOf(rej, ReasonBadSignature); r != ReasonTooLarge || rej.Error() != "too-large: troppo" {
		t.Fatalf("rejection: %s %q", r, rej.Error())
	}
}

// Corrispondenza (regola #26) all'ADR 0438 D3 punto 1 e D6: l'elenco chiuso dei motivi di
// update.result e il prefisso esatto delle release del repository pubblico.
func TestCorrispondenzaADR0438Motivi(t *testing.T) {
	want := []string{"bad-signature", "not-newer", "wrong-platform", "protocol-too-old", "bad-url",
		"download-failed", "sha256-mismatch", "too-large", "host-config", "tenant-switch"}
	if strings.Join(Reasons, ",") != strings.Join(want, ",") {
		t.Fatalf("motivi %v, attesi (ADR 0438 D3+D6) %v", Reasons, want)
	}
	if ReleasesPrefix != "https://github.com/monsterterminal/connector/releases/download/" {
		t.Fatalf("prefisso delle release %q", ReleasesPrefix)
	}
	if MaxBinaryBytes != 64<<20 {
		t.Fatalf("tetto del download %d, atteso 64 MiB", MaxBinaryBytes)
	}
	// Ogni motivo sta nel tetto di update.result.reason dello schema.
	for _, r := range append(Reasons, ReasonNoWelcome) {
		if len(r) > protocol.MaxUpdateReasonLength {
			t.Errorf("motivo %q oltre il tetto", r)
		}
	}
}
