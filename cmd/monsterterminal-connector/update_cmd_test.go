// Test del cablaggio dell'aggiornamento automatico nel comando (ADR 0438 D3, D4, D6): run chiede
// update.check dopo welcome e tratta update.available fino alla richiesta nella cassetta delle
// lettere; apply-update e rollback-check come root finto (binario in una cartella temporanea,
// systemctl registrato); gli esiti lasciati dall'updater arrivano al backend come update.result.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/update"
)

// rewriteGitHub manda al server di prova le richieste per github.com, lasciando intatto il percorso:
// l'URL del manifest resta quello vero delle release.
type rewriteGitHub struct{ srv *httptest.Server }

func (r rewriteGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.srv.URL)
	req2 := req.Clone(req.Context())
	req2.URL.Scheme = u.Scheme
	req2.URL.Host = u.Host
	req2.Host = u.Host
	return r.srv.Client().Transport.RoundTrip(req2)
}

type updateHarness struct {
	*harness
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
	body  []byte
	files *httptest.Server
	calls []string
	bin   string
}

func newUpdateHarness(t *testing.T) *updateHarness {
	t.Helper()
	h := &updateHarness{harness: newHarness(t), body: []byte("binario nuovo 0.7.0")}
	var err error
	h.pub, h.priv, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/monsterterminal-connector_0.7.0_linux_amd64") {
			_, _ = w.Write(h.body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(h.files.Close)
	h.bin = filepath.Join(h.dir, "bin", "monsterterminal-connector")
	_ = os.MkdirAll(filepath.Dir(h.bin), 0o755)
	if err := os.WriteFile(h.bin, []byte("binario installato "+version), 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *updateHarness) app() app {
	a := h.harness.app()
	a.updateHTTP = &http.Client{Transport: rewriteGitHub{h.files}}
	a.updateTuning = func(c *update.Config) {
		c.Key = h.pub
		c.Platform = update.Platform{OS: "linux", Arch: "amd64", Protocol: protocol.Version, Version: version}
		c.MailboxInterval = 50 * time.Millisecond
		c.CheckInterval = time.Hour
	}
	a.executable = func() (string, error) { return h.bin, nil }
	a.systemctl = func(args ...string) error { h.calls = append(h.calls, strings.Join(args, " ")); return nil }
	a.getuid = os.Getuid
	a.updateKey = h.pub
	return a
}

func (h *updateHarness) start(t *testing.T) (<-chan int, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- h.app().run(ctx, h.args("run")) }()
	t.Cleanup(cancel)
	return done, cancel
}

func (h *updateHarness) manifest(ver string) protocol.UpdateManifest {
	sum := sha256.Sum256(h.body)
	return protocol.UpdateManifest{Arch: "amd64", MinProtocol: 1, OS: "linux", SHA256: hex.EncodeToString(sum[:]),
		URL: update.ReleasesPrefix + "v" + ver + "/monsterterminal-connector_" + ver + "_linux_amd64", Version: ver}
}

func (h *updateHarness) offer(t *testing.T, m protocol.UpdateManifest) fakeserver.Offer {
	t.Helper()
	canon, err := update.Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	return fakeserver.Offer{Manifest: m, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(h.priv, canon))}
}

func (h *updateHarness) updateDir() string { return filepath.Join(h.stateDir, update.UpdateSubdir) }

// requested dice se il connettore ha lasciato apply.request nella cassetta delle lettere.
func (h *updateHarness) requested() bool {
	_, err := os.Lstat(filepath.Join(h.updateDir(), update.FileApplyRequest))
	return err == nil
}

func (h *updateHarness) registerAndStart(t *testing.T) (string, <-chan int, context.CancelFunc) {
	t.Helper()
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	if code := h.register(t); code != exitOK {
		t.Fatalf("register: %d %s", code, h.stderr.String())
	}
	id := hostIDFromStdout(t, h.stdout.String())
	done, stop := h.start(t)
	waitHello(t, h.s)
	return id, done, stop
}

// D4: update.check subito dopo welcome, risposto con update.none; poi un'offerta valida in risposta a
// update.check finisce nella cassetta delle lettere, e il connettore non la applica da solo.
func TestRunChiedeEPreparaLAggiornamento(t *testing.T) {
	h := newUpdateHarness(t)
	id, done, stop := h.registerAndStart(t)
	eventually(t, "update.check e update.none", func() bool { return h.s.UpdateChecks() >= 1 && h.s.UpdateNones() >= 1 })
	m := h.manifest("0.7.0")
	h.s.SetOffer(func() *fakeserver.Offer { o := h.offer(t, m); return &o }())
	// Una caduta forza un nuovo welcome e un nuovo update.check, che ora riceve l'offerta.
	h.s.Drop(id)
	waitHello(t, h.s)
	eventually(t, "richiesta nella cassetta delle lettere", h.requested)
	if got, _ := os.ReadFile(filepath.Join(h.updateDir(), update.FileBinary)); string(got) != string(h.body) {
		t.Fatalf("binario scaricato %q", got)
	}
	if st, _ := os.Stat(filepath.Join(h.updateDir(), update.FileBinary)); st.Mode()&0o111 != 0 {
		t.Fatal("binario scaricato eseguibile")
	}
	if got, _ := os.ReadFile(h.bin); string(got) != "binario installato "+version {
		t.Fatal("il connettore ha toccato il binario installato: non è compito suo")
	}
	if rs := h.s.UpdateResults(id); len(rs) != 0 {
		t.Fatalf("update.result prima dell'installazione: %+v", rs)
	}
	stop()
	waitExit(t, done)
}

// Un'offerta spinta dal backend (senza re) con una firma non valida è rifiutata senza download; con
// l'interruttore del backend spento è skipped tenant-switch; col file locale spento host-config.
func TestRunRifiutaESaltaLeOfferte(t *testing.T) {
	h := newUpdateHarness(t)
	id, done, stop := h.registerAndStart(t)
	eventually(t, "primo update.check", func() bool { return h.s.UpdateChecks() >= 1 })
	m := h.manifest("0.7.0")
	bad := h.offer(t, m)
	bad.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
	if n := h.s.PushUpdate(bad); n != 1 {
		t.Fatalf("spinto a %d host", n)
	}
	eventually(t, "failed bad-signature", func() bool { return len(h.s.UpdateResults(id)) == 1 })
	if r := h.s.UpdateResults(id)[0]; r != (protocol.UpdateResult{Version: "0.7.0", Outcome: protocol.UpdateFailed, Reason: update.ReasonBadSignature}) {
		t.Fatalf("%+v", r)
	}
	off := false
	h.s.SetAutoUpdate(&off) // config.update subito (ADR 0438 D6)
	time.Sleep(100 * time.Millisecond)
	h.s.PushUpdate(h.offer(t, m))
	eventually(t, "skipped tenant-switch", func() bool { return len(h.s.UpdateResults(id)) == 2 })
	if r := h.s.UpdateResults(id)[1]; r != (protocol.UpdateResult{Version: "0.7.0", Outcome: protocol.UpdateSkipped, Reason: update.ReasonTenantSwitch}) {
		t.Fatalf("%+v", r)
	}
	// security ignora l'interruttore del backend: si scarica.
	sec := true
	ms := m
	ms.Security = &sec
	h.s.PushUpdate(h.offer(t, ms))
	eventually(t, "download con security", h.requested)
	if entries, _ := os.ReadDir(h.updateDir()); len(entries) != 4 {
		t.Fatalf("cassetta: %v", entries)
	}
	stop()
	waitExit(t, done)

	// Il file locale dell'host vince anche sulla sicurezza (ADR 0409 D6, ADR 0438 D6).
	update.RemoveDownload(h.updateDir())
	if err := os.WriteFile(h.confFile, []byte("auto_update=no\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done, stop = h.start(t)
	waitHello(t, h.s)
	eventually(t, "update.check", func() bool { return h.s.UpdateChecks() >= 2 })
	h.s.PushUpdate(h.offer(t, ms))
	eventually(t, "skipped host-config", func() bool { return len(h.s.UpdateResults(id)) == 3 })
	if r := h.s.UpdateResults(id)[2]; r != (protocol.UpdateResult{Version: "0.7.0", Outcome: protocol.UpdateSkipped, Reason: update.ReasonHostConfig}) {
		t.Fatalf("%+v", r)
	}
	if h.requested() {
		t.Fatal("download con auto_update=no")
	}
	stop()
	waitExit(t, done)
}

// apply-update e rollback-check dal comando: la richiesta preparata da run si installa (binario
// finto, systemctl registrato), poi il connettore nuovo riporta ok e il timer disarma; un secondo
// aggiornamento senza welcome torna indietro e il connettore riporta rolled-back.
func TestApplyUpdateERollbackCheckDalComando(t *testing.T) {
	h := newUpdateHarness(t)
	body1 := string(h.body)
	id, done, stop := h.registerAndStart(t)
	eventually(t, "update.check", func() bool { return h.s.UpdateChecks() >= 1 })
	m := h.manifest("0.7.0")
	h.s.PushUpdate(h.offer(t, m))
	eventually(t, "richiesta", h.requested)
	stop()
	waitExit(t, done)

	// L'updater, come lo lancia l'unità: il binario «installato» è quello di prova.
	a := h.app()
	if code := a.run(context.Background(), []string{"apply-update", "-state-dir", h.stateDir}); code != exitOK {
		t.Fatalf("apply-update: %d\n%s", code, h.stderr.String())
	}
	if got, _ := os.ReadFile(h.bin); string(got) != string(h.body) {
		t.Fatalf("binario installato %q", got)
	}
	if got, _ := os.ReadFile(h.bin + update.PrevSuffix); string(got) != "binario installato "+version {
		t.Fatalf(".prev %q", got)
	}
	if got := strings.Join(h.calls, ";"); got != "restart "+update.RollbackTimer+";restart "+update.ServiceName {
		t.Fatalf("systemctl: %s", got)
	}
	if h.requested() {
		t.Fatal("apply.request non consumato")
	}
	// Qui non si può eseguire il binario nuovo: si simula il suo avvio facendo girare run con la
	// versione 0.7.0, che trova applied.json, scrive il marcatore e riporta ok.
	old := version
	version = "0.7.0"
	t.Cleanup(func() { version = old })
	h.calls = nil
	done, stop = h.start(t)
	waitHello(t, h.s)
	eventually(t, "update.result ok", func() bool { return len(h.s.UpdateResults(id)) == 1 })
	if r := h.s.UpdateResults(id)[0]; r != (protocol.UpdateResult{Version: "0.7.0", Outcome: protocol.UpdateOK}) {
		t.Fatalf("%+v", r)
	}
	if !update.ConnectedExists(h.updateDir(), "0.7.0") {
		t.Fatal("marcatore di collegamento non scritto")
	}
	eventually(t, "connector.restarted update", func() bool {
		for _, r := range h.s.Restarts() {
			if r.Reason == "update" && r.PreviousVersion == old && r.Version == "0.7.0" {
				return true
			}
		}
		return false
	})
	stop()
	waitExit(t, done)
	// Il timer: trova il marcatore e disarma (col binario .prev, che qui è lo stesso programma).
	version = old
	if code := h.app().run(context.Background(), []string{"rollback-check", "-state-dir", h.stateDir}); code != exitOK {
		t.Fatalf("rollback-check: %d\n%s", code, h.stderr.String())
	}
	if _, err := os.Lstat(h.bin + update.PrevSuffix); !os.IsNotExist(err) {
		t.Fatal(".prev non tolto dal disarmo")
	}
	if got := strings.Join(h.calls, ";"); got != "stop "+update.RollbackTimer {
		t.Fatalf("systemctl del disarmo: %s", got)
	}

	// Secondo aggiornamento, la versione nuova non si collega mai: ritorno e riporto.
	version = "0.7.0"
	h.body = []byte("binario rotto 0.8.0")
	done, stop = h.start(t)
	waitHello(t, h.s)
	eventually(t, "update.check", func() bool { return h.s.UpdateChecks() >= 3 })
	h.s.PushUpdate(h.offer(t, h.manifest("0.8.0")))
	eventually(t, "richiesta 0.8.0", h.requested)
	stop()
	waitExit(t, done)
	h.calls = nil
	if code := h.app().run(context.Background(), []string{"apply-update", "-state-dir", h.stateDir}); code != exitOK {
		t.Fatalf("apply-update 0.8.0: %d\n%s", code, h.stderr.String())
	}
	if got, _ := os.ReadFile(h.bin); string(got) != "binario rotto 0.8.0" {
		t.Fatalf("binario installato %q", got)
	}
	h.calls = nil
	if code := h.app().run(context.Background(), []string{"rollback-check", "-state-dir", h.stateDir}); code != exitOK {
		t.Fatalf("rollback-check (ritorno): %d\n%s", code, h.stderr.String())
	}
	if got, _ := os.ReadFile(h.bin); string(got) != body1 {
		t.Fatalf("binario dopo il ritorno %q", got)
	}
	if got := strings.Join(h.calls, ";"); got != "restart "+update.ServiceName {
		t.Fatalf("systemctl del ritorno: %s", got)
	}
	// Il connettore 0.7.0 ripartito riporta il ritorno.
	done, stop = h.start(t)
	waitHello(t, h.s)
	eventually(t, "update.result rolled-back", func() bool { return len(h.s.UpdateResults(id)) == 2 })
	if r := h.s.UpdateResults(id)[1]; r != (protocol.UpdateResult{Version: "0.8.0", Outcome: protocol.UpdateRolledBack, Reason: update.ReasonNoWelcome}) {
		t.Fatalf("%+v", r)
	}
	stop()
	waitExit(t, done)
}

// Un binario non firmato messo nella cassetta (come farebbe un utente del connettore) non si
// installa: apply-update esce con errore, apply.rejected resta per il connettore, che lo riporta.
func TestApplyUpdateRifiutaIlNonFirmatoEIlConnettoreLoRiporta(t *testing.T) {
	h := newUpdateHarness(t)
	id, done, stop := h.registerAndStart(t)
	stop()
	waitExit(t, done)
	dir := h.updateDir()
	_ = os.MkdirAll(dir, 0o700)
	m := h.manifest("0.7.0")
	canon, _ := update.Canonical(m)
	_ = os.WriteFile(filepath.Join(dir, update.FileBinary), h.body, 0o600)
	if err := update.WriteRequest(dir, canon, make([]byte, 64), "0.7.0"); err != nil {
		t.Fatal(err)
	}
	if code := h.app().run(context.Background(), []string{"apply-update", "-state-dir", h.stateDir}); code != exitError {
		t.Fatalf("apply-update col non firmato: uscita %d", code)
	}
	if !strings.Contains(h.stderr.String(), update.ReasonBadSignature) {
		t.Fatalf("stderr senza il motivo:\n%s", h.stderr.String())
	}
	if got, _ := os.ReadFile(h.bin); string(got) != "binario installato "+version {
		t.Fatal("binario installato cambiato")
	}
	if len(h.calls) != 0 {
		t.Fatalf("systemctl chiamato: %v", h.calls)
	}
	done, stop = h.start(t)
	waitHello(t, h.s)
	eventually(t, "failed bad-signature riportato", func() bool { return len(h.s.UpdateResults(id)) == 1 })
	if r := h.s.UpdateResults(id)[0]; r != (protocol.UpdateResult{Version: "0.7.0", Outcome: protocol.UpdateFailed, Reason: update.ReasonBadSignature}) {
		t.Fatalf("%+v", r)
	}
	stop()
	waitExit(t, done)
	// Senza -state-dir valido i sottocomandi non vogliono argomenti in più.
	if code := h.app().run(context.Background(), []string{"apply-update", "extra"}); code != exitUsage {
		t.Fatalf("argomento in più: %d", code)
	}
}
