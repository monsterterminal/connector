package main

// Il catalogo dal comando (spec §1.9, §1.11, ADR 0410 A4): capacità e agenti in hello, installazione
// di una ricetta firmata con una chiave usa e getta fino a catalog.install.result ok e catalog.installed
// nel giornale, interruttore dell'host spento, e i sottocomandi catalog path-add e catalog extract.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/catalog"
	"github.com/monsterterminal/connector/internal/protocol"
)

type catalogHarness struct {
	*harness
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	detects atomic.Int32
}

func newCatalogHarness(t *testing.T) *catalogHarness {
	t.Helper()
	h := &catalogHarness{harness: newHarness(t)}
	if err := os.MkdirAll(filepath.Join(h.dir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	var err error
	if h.pub, h.priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
		t.Fatal(err)
	}
	h.harness.agents = func() []protocol.Agent {
		n := h.detects.Add(1)
		if n == 1 {
			return []protocol.Agent{} // al primo hello non c'è niente
		}
		return []protocol.Agent{{Agent: "generic", Version: "1.0.0"}}
	}
	return h
}

func (h *catalogHarness) app() app {
	a := h.harness.app()
	a.recipeKey = h.pub
	return a
}

func (h *catalogHarness) start(t *testing.T) (<-chan int, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- h.app().run(ctx, h.args("run")) }()
	t.Cleanup(cancel)
	return done, cancel
}

func (h *catalogHarness) registerAndStart(t *testing.T) (protocol.Hello, <-chan int, context.CancelFunc) {
	t.Helper()
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	if code := h.register(t); code != exitOK {
		t.Fatalf("register: %d %s", code, h.stderr.String())
	}
	done, stop := h.start(t)
	return waitHello(t, h.s), done, stop
}

func (h *catalogHarness) recipe(t *testing.T, steps ...catalog.Step) (protocol.CatalogInstall, []byte) {
	t.Helper()
	raw, err := catalog.Canonical(catalog.Recipe{
		RecipeID: "prova", RecipeVersion: 1, ExpiresAt: "2099-01-01T00:00:00Z",
		Source: catalog.Source{Channel: "npm", Ref: "prova@1.0.0"}, Agent: "generic",
		Platforms: []catalog.Platform{{OS: "linux", Arch: []string{"amd64"}, Distro: []string{"ubuntu"}}},
		Requires:  []catalog.Require{}, Steps: steps,
		Auth: catalog.Auth{Instructions: "nothing"}, Start: []string{"prova"},
		Adapter: catalog.Adapter{Kind: "generic", ID: "generic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.CatalogInstall{InstallID: protocol.NewUUID(), Recipe: raw,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(h.priv, raw))}, raw
}

func TestRunInstallaUnaRicettaFirmata(t *testing.T) {
	h := newCatalogHarness(t)
	hello, done, stop := h.registerAndStart(t)
	defer func() { stop(); waitExit(t, done) }()
	if strings.Join(hello.Capabilities, ",") != "catalog-install,auto-update" || !hello.CatalogInstallEnabled {
		t.Fatalf("hello: capacità %v, catalogInstallEnabled %v", hello.Capabilities, hello.CatalogInstallEnabled)
	}
	if hello.Agents == nil || len(hello.Agents) != 0 {
		t.Fatalf("hello.agents: %v", hello.Agents)
	}
	// Il passo crea un file relativo alla home: i passi girano nella home dell'utente del connettore.
	ci, raw := h.recipe(t, catalog.Step{Run: []string{"/usr/bin/touch", "installato"}, TimeoutSeconds: 5})
	if n := h.s.CatalogInstallAll(ci.InstallID, raw, ci.Signature); n != 1 {
		t.Fatalf("catalog.install a %d host", n)
	}
	eventually(t, "catalog.install.result", func() bool { return len(h.s.CatalogResults()) == 1 })
	if res := h.s.CatalogResults()[0]; res.Outcome != protocol.CatalogOK {
		t.Fatalf("esito: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "home", "installato")); err != nil {
		t.Fatal("il passo non ha girato nella home")
	}
	if p := h.s.CatalogProgress(); len(p) != 1 || p[0].Step != 1 || p[0].Of != 1 {
		t.Fatalf("progresso: %+v", p)
	}
	// catalog.installed nel giornale, con la versione del rilevamento rifatto dopo l'installazione.
	eventually(t, "catalog.installed", func() bool { return len(h.s.CatalogInstalled()) == 1 })
	if ev := h.s.CatalogInstalled()[0]; ev != (protocol.CatalogInstalled{RecipeID: "prova", RecipeVersion: 1, Agent: "generic", Version: "1.0.0"}) {
		t.Fatalf("catalog.installed: %+v", ev)
	}
	if s, ok, err := catalog.ReadState(h.stateDir, "prova"); err != nil || !ok || s.RecipeVersion != 1 {
		t.Fatalf("file di stato: %+v %v %v", s, ok, err)
	}
	// La stessa ricetta di nuovo: downgrade.
	ci2, raw2 := h.recipe(t, catalog.Step{Run: []string{"/bin/true"}, TimeoutSeconds: 5})
	h.s.CatalogInstallAll(ci2.InstallID, raw2, ci2.Signature)
	eventually(t, "secondo esito", func() bool { return len(h.s.CatalogResults()) == 2 })
	if res := h.s.CatalogResults()[1]; res.Outcome != protocol.CatalogRefused || res.Reason != protocol.ReasonDowngrade {
		t.Fatalf("seconda installazione: %+v", res)
	}
}

// Con catalog_install=no: niente capacità, catalogInstallEnabled falso, e ogni ricetta è
// disabled-on-host senza che nessun passo giri.
func TestRunCatalogoSpentoSullHost(t *testing.T) {
	h := newCatalogHarness(t)
	if err := os.MkdirAll(filepath.Dir(h.confFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.confFile, []byte("catalog_install=no\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hello, done, stop := h.registerAndStart(t)
	defer func() { stop(); waitExit(t, done) }()
	if strings.Join(hello.Capabilities, ",") != "auto-update" || hello.CatalogInstallEnabled {
		t.Fatalf("hello: capacità %v, catalogInstallEnabled %v", hello.Capabilities, hello.CatalogInstallEnabled)
	}
	ci, raw := h.recipe(t, catalog.Step{Run: []string{"/usr/bin/touch", "mai"}, TimeoutSeconds: 5})
	h.s.CatalogInstallAll(ci.InstallID, raw, ci.Signature)
	eventually(t, "catalog.install.result", func() bool { return len(h.s.CatalogResults()) == 1 })
	if res := h.s.CatalogResults()[0]; res.Outcome != protocol.CatalogRefused || res.Reason != protocol.ReasonDisabledOnHost {
		t.Fatalf("esito: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "home", "mai")); err == nil {
		t.Fatal("un passo ha girato con il catalogo spento")
	}
}

// La chiave incorporata è quella delle ricette: una ricetta firmata con una chiave usa e getta, senza
// sostituirla, è signature-invalid.
func TestRunRicettaConUnAltraChiave(t *testing.T) {
	h := newCatalogHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- h.harness.app().run(ctx, h.args("run")) }() // senza recipeKey
	defer func() { cancel(); waitExit(t, done) }()
	waitHello(t, h.s)
	ci, raw := h.recipe(t, catalog.Step{Run: []string{"/bin/true"}, TimeoutSeconds: 5})
	h.s.CatalogInstallAll(ci.InstallID, raw, ci.Signature)
	eventually(t, "catalog.install.result", func() bool { return len(h.s.CatalogResults()) == 1 })
	if res := h.s.CatalogResults()[0]; res.Outcome != protocol.CatalogRefused || res.Reason != protocol.ReasonSignatureInvalid {
		t.Fatalf("esito: %+v", res)
	}
}

func TestComandoCatalogPathAdd(t *testing.T) {
	h := newHarness(t)
	home := filepath.Join(h.dir, "home")
	_ = os.MkdirAll(home, 0o755)
	for i := 0; i < 2; i++ {
		if code := h.app().run(context.Background(), []string{"catalog", "path-add", ".local/opencode"}); code != exitOK {
			t.Fatalf("path-add: %d %s", code, h.stderr.String())
		}
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".profile"))
	if strings.Count(string(raw), catalog.ProfileMarker) != 1 || !strings.Contains(string(raw), `"$HOME/.local/opencode:$PATH"`) {
		t.Fatalf(".profile: %q", raw)
	}
	if !strings.Contains(h.stdout.String(), "Già presente") {
		t.Fatalf("seconda volta: %q", h.stdout.String())
	}
	for _, args := range [][]string{{"catalog", "path-add", "~/.local/bin"}, {"catalog", "path-add", "../fuori"}} {
		if code := h.app().run(context.Background(), args); code != exitError {
			t.Errorf("%v: uscita %d", args, code)
		}
	}
	for _, args := range [][]string{{"catalog"}, {"catalog", "path-add"}, {"catalog", "path-add", "a", "b"}, {"catalog", "boh", "x"}, {"catalog", "extract", "a"}} {
		if code := h.app().run(context.Background(), args); code == exitOK {
			t.Errorf("%v accettato", args)
		}
	}
}

// L'ambiente dei passi: il PATH del servizio allungato con le cartelle di path-add e della home, una
// sola voce PATH, HOME presente.
func TestAmbienteDeiPassi(t *testing.T) {
	home := t.TempDir()
	if _, _, err := catalog.PathAdd(home, ".local/opencode"); err != nil {
		t.Fatal(err)
	}
	env := catalogEnv(home)
	var paths []string
	hasHome := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			paths = append(paths, kv)
		}
		if strings.HasPrefix(kv, "HOME=") {
			hasHome = true
		}
	}
	if len(paths) != 1 || !hasHome {
		t.Fatalf("PATH %v, HOME %v", paths, hasHome)
	}
	for _, d := range []string{filepath.Join(home, ".local/opencode"), filepath.Join(home, ".npm-global/bin"), filepath.Join(home, ".local/bin")} {
		if !strings.Contains(paths[0], ":"+d) {
			t.Errorf("PATH senza %s: %s", d, paths[0])
		}
	}
	if len(catalogEnv("")) != len(os.Environ()) {
		t.Fatal("senza home l'ambiente deve restare quello del processo")
	}
}

func TestComandoCatalogExtract(t *testing.T) {
	h := newHarness(t)
	home := filepath.Join(h.dir, "home")
	_ = os.MkdirAll(home, 0o755)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "opencode", Mode: 0o755, Size: 3, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("bin"))
	tw.Close()
	gz.Close()
	arc := filepath.Join(home, "a.tar.gz")
	_ = os.WriteFile(arc, buf.Bytes(), 0o600)
	// L'archivio con percorso assoluto, la cartella relativa alla home.
	if code := h.app().run(context.Background(), []string{"catalog", "extract", arc, ".local/opencode"}); code != exitOK {
		t.Fatalf("extract: %d %s", code, h.stderr.String())
	}
	if fi, err := os.Stat(filepath.Join(home, ".local/opencode/opencode")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("estratto: %v", err)
	}
	// Un archivio con una voce che esce: errore, niente fuori.
	buf.Reset()
	gz = gzip.NewWriter(&buf)
	tw = tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "../../evaso", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	_ = os.WriteFile(arc, buf.Bytes(), 0o600)
	if code := h.app().run(context.Background(), []string{"catalog", "extract", "a.tar.gz", "dest"}); code != exitError {
		t.Fatalf("traversal: uscita %d", code)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "evaso")); err == nil {
		t.Fatal("voce scritta fuori dalla cartella")
	}
}
