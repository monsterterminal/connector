// Il catalogo visto dal trasporto (spec §1.9, ADR 0410 A4): catalog.install va all'installatore prima
// del default; senza installatore resta TYPE_UNKNOWN come prima della v0.8.0. Progresso ed esito
// arrivano al backend con re = id del catalog.install.
package transport_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/catalog"
	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/transport"
)

func signedRecipe(t *testing.T) (json.RawMessage, string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := catalog.Canonical(catalog.Recipe{
		RecipeID: "prova", RecipeVersion: 1, ExpiresAt: "2099-01-01T00:00:00Z",
		Source: catalog.Source{Channel: "npm", Ref: "prova@1.0.0"}, Agent: "generic",
		Platforms: []catalog.Platform{{OS: "linux", Arch: []string{"amd64", "arm64"}, Distro: []string{"debian", "ubuntu", "rhel"}}},
		Requires:  []catalog.Require{},
		Steps:     []catalog.Step{{Run: []string{"/bin/true"}, TimeoutSeconds: 5}, {Run: []string{"/bin/true"}, TimeoutSeconds: 5}},
		Auth:      catalog.Auth{Instructions: "nothing"}, Start: []string{"true"},
		Adapter: catalog.Adapter{Kind: "generic", ID: "generic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw)), pub
}

func TestCatalogInstallDalTrasporto(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	raw, sig, pub := signedRecipe(t)
	in := catalog.New(catalog.Config{StateDir: t.TempDir(), Home: t.TempDir(), CatalogInstall: true, Key: pub,
		Platform: catalog.HostPlatform{OS: "linux", Arch: "amd64", Distro: "debian"}})
	r := start(t, s, tok, func(c *transport.Config) { c.Catalog = in })
	t.Cleanup(r.cancel)
	waitHello(t, s)
	waitWelcome(t, r)
	msgID, err := s.SendControl(id, protocol.TypeCatalogInstall, protocol.CatalogInstall{
		InstallID: "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b", Recipe: raw, Signature: sig,
		ApprovedBy: protocol.NewUUID(), ApprovedAt: protocol.Timestamp(time.Now()),
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "catalog.install.result", func() bool { return len(s.CatalogResults()) == 1 })
	if res := s.CatalogResults()[0]; res.Outcome != protocol.CatalogOK || res.InstallID != "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b" {
		t.Fatalf("esito: %+v", res)
	}
	if p := s.CatalogProgress(); len(p) != 2 || p[0].Step != 1 || p[1].Step != 2 || p[1].Of != 2 {
		t.Fatalf("progresso: %+v", p)
	}
	if env, ok := s.Reply(id, msgID); !ok || env.Type != protocol.TypeCatalogInstallResult && env.Type != protocol.TypeCatalogInstallProgress {
		t.Fatalf("risposta con re: %+v %v", env, ok)
	}
}

func TestCatalogInstallSenzaInstallatoreETypeUnknown(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	r := start(t, s, tok, nil)
	t.Cleanup(r.cancel)
	waitHello(t, s)
	waitWelcome(t, r)
	raw, sig, _ := signedRecipe(t)
	msgID, err := s.SendControl(id, protocol.TypeCatalogInstall, protocol.CatalogInstall{
		InstallID: protocol.NewUUID(), Recipe: raw, Signature: sig, ApprovedBy: protocol.NewUUID(), ApprovedAt: protocol.Timestamp(time.Now()),
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "risposta TYPE_UNKNOWN", func() bool { _, ok := s.Reply(id, msgID); return ok })
	env, _ := s.Reply(id, msgID)
	if env.Type != protocol.TypeError || !strings.Contains(string(env.Body), protocol.CodeTypeUnknown) {
		t.Fatalf("risposta: %s %s", env.Type, env.Body)
	}
}
