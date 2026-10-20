package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Corrispondenza alla spec §1.17 (due chiavi: aggiornamenti e ricette, le private mai sui server) e
// §1.10 (manifest firmato sulla forma canonica): le chiavi incorporate sono quelle che il custode ha
// generato il 2026-10-19 e dichiarato in SECURITY.md, riconosciute dall'impronta SHA-256 del DER.
const (
	updatesFingerprint = "90efb21f2321b1a530efff3973b5d8f4f354675af509958f3f08422cd1c43b0d"
	recipesFingerprint = "12fd1e92f867a787abb5a18d4b07d8cbaaac3363c10bd921cadce06bb85bbd54"
)

func TestChiaviIncorporateSonoQuelleDichiarate(t *testing.T) {
	for id, want := range map[KeyID]string{Updates: updatesFingerprint, Recipes: recipesFingerprint} {
		fp, err := Fingerprint(id)
		if err != nil || fp != want {
			t.Fatalf("%s: impronta %s (%v), attesa %s", id, fp, err, want)
		}
	}
	if u, _ := Fingerprint(Updates); u == recipesFingerprint {
		t.Fatal("le due chiavi devono essere diverse")
	}
	if _, err := Fingerprint("mt-altro"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("chiave sconosciuta: %v", err)
	}
}

// Tre posti, una chiave: install.sh, SECURITY.md e lo script di firma devono portare la stessa
// chiave del binario.
func TestInstallShESecurityMdPortanoLeStesseChiavi(t *testing.T) {
	sh := mustRead(t, "../../packaging/install.sh")
	if !strings.Contains(sh, `MT_UPDATES_PUBKEY_PEM="`+UpdatesPEM+`"`) {
		t.Fatal("packaging/install.sh non porta UpdatesPEM")
	}
	if !strings.Contains(sh, "MT_UPDATES_PUBKEY_FINGERPRINT="+updatesFingerprint) {
		t.Fatal("packaging/install.sh non dichiara l'impronta di Updates")
	}
	sec := mustRead(t, "../../SECURITY.md")
	for _, want := range []string{UpdatesPEM, RecipesPEM, updatesFingerprint, recipesFingerprint} {
		if !strings.Contains(sec, want) {
			t.Fatalf("SECURITY.md non contiene %q", want)
		}
	}
	m := regexp.MustCompile(`UPDATES_FINGERPRINT=([0-9a-f]+)`).FindStringSubmatch(mustRead(t, "../../scripts/firma-rilascio.sh"))
	if m == nil || m[1] != updatesFingerprint {
		t.Fatal("scripts/firma-rilascio.sh non pretende l'impronta di Updates")
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParsePublicKeyRifiutaAltreForme(t *testing.T) {
	if _, err := ParsePublicKey("non è pem"); err == nil {
		t.Fatal("testo qualsiasi accettato")
	}
	if _, err := ParsePublicKey(strings.Replace(UpdatesPEM, "PUBLIC KEY", "CERTIFICATE", 2)); err == nil {
		t.Fatal("blocco di tipo diverso accettato")
	}
	// Una PUBLIC KEY con DER non Ed25519 (qui: troncato): la forma giusta, il contenuto no.
	bad := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{0x30, 0x03, 0x02, 0x01, 0x01}})
	if _, err := ParsePublicKey(string(bad)); err == nil {
		t.Fatal("DER non Ed25519 accettato")
	}
}

// La verifica vera e propria si prova con una coppia generata qui: la privata di mt-updates-2026 non
// esiste su questa macchina, per contratto.
func TestVerificaGrezzaEd25519(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("abc  SHA256SUMS\n")
	sig := ed25519.Sign(priv, msg)
	if err := verifyWith(pub, msg, sig); err != nil {
		t.Fatalf("firma buona rifiutata: %v", err)
	}
	altered := append([]byte("x"), msg[1:]...)
	if err := verifyWith(pub, altered, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("messaggio alterato: %v", err)
	}
	corrupt := append([]byte{}, sig...)
	corrupt[10] ^= 0xff
	if err := verifyWith(pub, msg, corrupt); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("firma corrotta: %v", err)
	}
	if err := verifyWith(pub, msg, sig[:63]); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("firma corta: %v", err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := verifyWith(other, msg, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("altra chiave: %v", err)
	}
	// Con la chiave incorporata una firma di prova non può valere: nessuno qui ha la privata.
	if err := Verify(Updates, msg, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("firma di una chiave estranea accettata da Updates: %v", err)
	}
	// Il PEM prodotto da Go per la coppia di prova si rilegge con lo stesso parser delle chiavi vere,
	// e l'impronta coincide con quella di openssl (stesso DER SubjectPublicKeyInfo).
	der, _ := x509.MarshalPKIXPublicKey(pub)
	back, err := ParsePublicKey(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	if err != nil || !back.Equal(pub) {
		t.Fatalf("andata e ritorno PEM: %v", err)
	}
}

func TestParseManifestVerificaPrimaDiLeggere(t *testing.T) {
	canonical := `{"arch":"amd64","minProtocol":1,"os":"linux","sha256":"` + strings.Repeat("ab", 32) +
		`","url":"https://github.com/monsterterminal/connector/releases/download/v0.2.0/monsterterminal-connector_0.2.0_linux_amd64","version":"0.2.0"}`
	// Senza la privata di Updates nessuna firma vale: il manifest non si legge nemmeno se è perfetto.
	if _, err := ParseManifest([]byte(canonical), make([]byte, 64)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("manifest non firmato letto: %v", err)
	}
	// La lettura si prova sostituendo la chiave incorporata con una di prova, solo in questo test.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	saved := keys[Updates]
	keys[Updates] = pub
	t.Cleanup(func() { keys[Updates] = saved })
	sign := func(s string) []byte { return ed25519.Sign(priv, []byte(s)) }

	m, err := ParseManifest([]byte(canonical), sign(canonical))
	if err != nil || m.Version != "0.2.0" || m.Arch != "amd64" || m.MinProtocol != 1 {
		t.Fatalf("manifest buono: %+v, %v", m, err)
	}
	bad := map[string]string{
		"campo sconosciuto":  strings.Replace(canonical, `"version"`, `"extra":1,"version"`, 1),
		"version non semver": strings.Replace(canonical, `"0.2.0"`, `"v0.2.0"`, 1),
		"os sconosciuto":     strings.Replace(canonical, `"linux"`, `"windows"`, 1),
		"arch sconosciuta":   strings.Replace(canonical, `"amd64"`, `"i386"`, 1),
		"url non https":      strings.Replace(canonical, `https://`, `http://`, 1),
		"sha256 corto":       strings.Replace(canonical, strings.Repeat("ab", 32), "abcd", 1),
		"minProtocol zero":   strings.Replace(canonical, `"minProtocol":1`, `"minProtocol":0`, 1),
		"coda dopo oggetto":  canonical + "{}",
	}
	for name, raw := range bad {
		if _, err := ParseManifest([]byte(raw), sign(raw)); err == nil {
			t.Errorf("%s: accettato", name)
		}
	}
	// Firma di un altro manifest (stessa chiave): rifiutata.
	if _, err := ParseManifest([]byte(canonical), sign(bad["url non https"])); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("firma di un altro manifest: %v", err)
	}
}
