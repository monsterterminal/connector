package main

// Lo scenario del catalogo del backend finto (ADR 0410 A4): il comando catalog.install manda una
// ricetta firmata con una chiave usa e getta a un connettore collegato (il trasporto vero con
// l'installatore vero, che verifica con quella chiave), e lo status-file raccoglie progresso, esito
// ed evento catalog.installed.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/catalog"
	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/journal"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/transport"
)

func TestComandoCatalogInstall(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	_, tok := s.AddHost()
	path := filepath.Join(t.TempDir(), "status.json")
	w := &statusWriter{path: path, s: s}
	s.Notify = w.onEvent

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	j, err := journal.Open(filepath.Join(state, "journal"), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	in := catalog.New(catalog.Config{StateDir: state, Home: t.TempDir(), CatalogInstall: true, Key: pub,
		Platform: catalog.HostPlatform{OS: "linux", Arch: "amd64", Distro: "debian"}, Journal: j,
		Detect: func() []protocol.Agent { return []protocol.Agent{{Agent: "generic", Version: "1.0.0"}} }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = transport.Run(ctx, transport.Config{
			URL: s.WSURL(), Token: tok, HTTPClient: s.Client(), Catalog: in, Events: j,
			SyncGrace: 20 * time.Millisecond,
			Hello: func() protocol.Hello {
				return protocol.Hello{ConnectorVersion: "0.8.0", Protocol: 1, OS: "linux", Arch: "amd64", RunAsUser: "mt",
					Capabilities: []string{protocol.CapabilityCatalogInstall}, CatalogInstallEnabled: true, Agents: []protocol.Agent{}}
			},
		})
	}()
	select {
	case <-s.Hellos():
	case <-time.After(10 * time.Second):
		t.Fatal("hello non arrivato")
	}

	raw, err := catalog.Canonical(catalog.Recipe{
		RecipeID: "banco", RecipeVersion: 1, ExpiresAt: "2099-01-01T00:00:00Z",
		Source: catalog.Source{Channel: "release", Ref: "https://example.com/v1/banco"}, Agent: "generic",
		Platforms: []catalog.Platform{{OS: "linux", Arch: []string{"amd64"}, Distro: []string{"debian"}}},
		Requires:  []catalog.Require{},
		Steps:     []catalog.Step{{Run: []string{"/bin/true"}, TimeoutSeconds: 5}, {Run: []string{"/bin/true"}, TimeoutSeconds: 5}},
		Auth:      catalog.Auth{Instructions: "nothing"}, Start: []string{"true"},
		Adapter: catalog.Adapter{Kind: "generic", ID: "generic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw))
	if err := w.runCommand([]byte(`{"action":"catalog.install"}`)); err == nil {
		t.Fatal("catalog.install senza ricetta accettato")
	}
	cmd := `{"action":"catalog.install","installId":"6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b","recipe":` + string(raw) + `,"signature":"` + sig + `"}`
	if err := w.runCommand([]byte(cmd)); err != nil {
		t.Fatal(err)
	}
	var st status
	deadline := time.Now().Add(10 * time.Second)
	for {
		body, _ := os.ReadFile(path)
		st = status{}
		_ = json.Unmarshal(body, &st)
		if len(st.CatalogResults) == 1 && len(st.CatalogInstalled) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status-file senza esito o evento:\n%s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r := st.CatalogResults[0]; r.Outcome != protocol.CatalogOK || r.InstallID != "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b" {
		t.Fatalf("esito: %+v", r)
	}
	if len(st.CatalogProgress) != 2 || st.CatalogProgress[1].Step != 2 {
		t.Fatalf("progresso: %+v", st.CatalogProgress)
	}
	if ev := st.CatalogInstalled[0]; ev.RecipeID != "banco" || ev.Version != "1.0.0" {
		t.Fatalf("catalog.installed: %+v", ev)
	}
}
