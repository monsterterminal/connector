// Test del gestore lato connettore (ADR 0438 D3 punto 1, D4, D6) con una connessione finta: ciò che
// il connettore manda al backend (update.check, update.result, error) e ciò che lascia nella
// cassetta delle lettere. Il download va a un server di prova raggiunto riscrivendo l'host di GitHub
// nel trasporto HTTP: l'URL del manifest resta quello vero.
package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

type fakeOut struct {
	mu   sync.Mutex
	sent []protocol.Envelope
	fail bool // ogni Control fallisce: la connessione è caduta
}

func (f *fakeOut) Control(typ, re string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("connessione caduta")
	}
	f.sent = append(f.sent, protocol.Envelope{Type: typ, Re: re, Body: raw})
	return nil
}

func (f *fakeOut) setFail(v bool) {
	f.mu.Lock()
	f.fail = v
	f.mu.Unlock()
}

func (f *fakeOut) all() []protocol.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Envelope(nil), f.sent...)
}

func (f *fakeOut) results(t *testing.T) []protocol.UpdateResult {
	t.Helper()
	var out []protocol.UpdateResult
	for _, e := range f.all() {
		if e.Type == protocol.TypeUpdateResult {
			var r protocol.UpdateResult
			if err := json.Unmarshal(e.Body, &r); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeOut) count(typ string) int {
	n := 0
	for _, e := range f.all() {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// rewriteGitHub manda a srv le richieste per github.com, lasciando intatto il percorso.
type rewriteGitHub struct{ srv *httptest.Server }

func (r rewriteGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.srv.URL)
	req2 := req.Clone(req.Context())
	req2.URL.Scheme = u.Scheme
	req2.URL.Host = u.Host
	req2.Host = u.Host
	return r.srv.Client().Transport.RoundTrip(req2)
}

type mbench struct {
	m       *Manager
	out     *fakeOut
	dir     string
	priv    ed25519.PrivateKey
	srv     *httptest.Server
	served  map[string][]byte
	servMu  sync.Mutex
	gets    int
	hostCfg bool
}

func newMBench(t *testing.T, hostAuto bool) *mbench {
	t.Helper()
	pub, priv := keypair(t)
	b := &mbench{out: &fakeOut{}, dir: filepath.Join(t.TempDir(), "update"), priv: priv, served: map[string][]byte{}, hostCfg: hostAuto}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.servMu.Lock()
		b.gets++
		body, ok := b.served[r.URL.Path]
		b.servMu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(b.srv.Close)
	m, err := New(Config{
		Dir:             b.dir,
		Version:         "0.6.1",
		HostAutoUpdate:  hostAuto,
		HTTPClient:      &http.Client{Transport: rewriteGitHub{b.srv}},
		Key:             pub,
		Platform:        Platform{OS: "linux", Arch: "amd64", Protocol: 1, Version: "0.6.1"},
		CheckInterval:   time.Hour,
		MailboxInterval: 50 * time.Millisecond,
		DownloadTimeout: 5 * time.Second,
		ConnectedAfter:  200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	b.m = m
	return b
}

func (b *mbench) serve(m protocol.UpdateManifest, body []byte) {
	b.servMu.Lock()
	b.served[strings.TrimPrefix(m.URL, "https://github.com")] = body
	b.servMu.Unlock()
}

func (b *mbench) getCount() int {
	b.servMu.Lock()
	defer b.servMu.Unlock()
	return b.gets
}

func (b *mbench) available(t *testing.T, m protocol.UpdateManifest, sig []byte, re string) protocol.Envelope {
	t.Helper()
	if sig == nil {
		sig = sign(t, b.priv, m)
	}
	body, err := json.Marshal(protocol.UpdateAvailable{Version: m.Version, Manifest: m, Signature: base64.StdEncoding.EncodeToString(sig)})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Envelope{Type: protocol.TypeUpdateAvailable, ID: protocol.NewUUID(), Re: re, Body: body}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("non avvenuto entro 5 s: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManagerChiedeDopoWelcomeERispondeAUpdateNone(t *testing.T) {
	b := newMBench(t, true)
	b.m.Attach(b.out)
	defer b.m.Detach()
	waitFor(t, "update.check", func() bool { return b.out.count(protocol.TypeUpdateCheck) == 1 })
	checks := b.out.all()
	var ck protocol.Envelope
	for _, e := range checks {
		if e.Type == protocol.TypeUpdateCheck {
			ck = e
		}
	}
	if string(ck.Body) != "{}" || ck.Re != "" {
		t.Fatalf("update.check %s re=%q", ck.Body, ck.Re)
	}
	none := protocol.Envelope{Type: protocol.TypeUpdateNone, ID: protocol.NewUUID(), Re: "x", Body: []byte("{}")}
	if !b.m.Handle(none) {
		t.Fatal("update.none non gestito")
	}
	if !b.m.Handle(protocol.Envelope{Type: protocol.TypeUpdateNone, ID: protocol.NewUUID(), Body: []byte(`{"x":1}`)}) {
		t.Fatal("update.none malformato non gestito")
	}
	if b.out.count(protocol.TypeError) != 1 {
		t.Fatalf("update.none malformato senza MESSAGE_INVALID: %+v", b.out.all())
	}
	if b.m.Handle(protocol.Envelope{Type: "session.open"}) {
		t.Fatal("un tipo altrui è stato preso")
	}
	if len(b.out.results(t)) != 0 {
		t.Fatalf("update.result senza motivo: %+v", b.out.results(t))
	}
}

// Un update.available (con re, in risposta a update.check, o senza re, spinto dal backend) valido
// finisce nella cassetta delle lettere: binario, manifest canonico, firma, richiesta. Nessun
// update.result: lo manderà la versione nuova dopo welcome.
func TestManagerScaricaEPreparaLaRichiesta(t *testing.T) {
	for _, re := range []string{"", "con-re"} {
		t.Run("re="+re, func(t *testing.T) {
			b := newMBench(t, true)
			b.m.Attach(b.out)
			defer b.m.Detach()
			body := []byte("binario nuovo 0.7.0")
			m := withSHA(newManifest("0.7.0"), body)
			b.serve(m, body)
			if !b.m.Handle(b.available(t, m, nil, re)) {
				t.Fatal("update.available non gestito")
			}
			waitFor(t, "richiesta scritta", func() bool {
				_, err := os.Lstat(filepath.Join(b.dir, FileApplyRequest))
				return err == nil
			})
			b.m.Wait()
			if got, _ := os.ReadFile(filepath.Join(b.dir, FileBinary)); string(got) != string(body) {
				t.Fatalf("binario %q", got)
			}
			canon, _ := Canonical(m)
			if got, _ := os.ReadFile(filepath.Join(b.dir, FileManifest)); string(got) != string(canon) {
				t.Fatalf("manifest %s", got)
			}
			if got, _ := os.ReadFile(filepath.Join(b.dir, FileSignature)); string(got) != string(sign(t, b.priv, m)) {
				t.Fatal("firma diversa da quella ricevuta")
			}
			if got, _ := os.ReadFile(filepath.Join(b.dir, FileApplyRequest)); string(got) != "0.7.0\n" {
				t.Fatalf("apply.request: %q", got)
			}
			if st, _ := os.Stat(filepath.Join(b.dir, FileBinary)); st.Mode()&0o111 != 0 {
				t.Fatal("il binario scaricato è eseguibile")
			}
			if n := len(b.out.results(t)); n != 0 {
				t.Fatalf("update.result prima dell'installazione: %+v", b.out.results(t))
			}
			// Una seconda proposta con la richiesta in attesa non scarica di nuovo.
			gets := b.getCount()
			b.m.Handle(b.available(t, withSHA(newManifest("0.7.1"), body), nil, ""))
			b.m.Wait()
			if b.getCount() != gets {
				t.Fatal("secondo download con una richiesta in attesa")
			}
		})
	}
}

func TestManagerRifiutaSenzaScaricare(t *testing.T) {
	cases := []struct {
		name   string
		mod    func(*protocol.UpdateManifest)
		sig    func(t *testing.T, b *mbench, m protocol.UpdateManifest) []byte
		reason string
	}{
		{"firma non valida", nil, func(t *testing.T, b *mbench, m protocol.UpdateManifest) []byte { return make([]byte, 64) }, ReasonBadSignature},
		{"altra chiave", nil, func(t *testing.T, b *mbench, m protocol.UpdateManifest) []byte {
			_, priv := keypair(t)
			return sign(t, priv, m)
		}, ReasonBadSignature},
		{"non più recente", func(m *protocol.UpdateManifest) { m.Version = "0.6.1" }, nil, ReasonNotNewer},
		{"altra piattaforma", func(m *protocol.UpdateManifest) { m.Arch = "arm64" }, nil, ReasonWrongPlatform},
		{"protocollo oltre", func(m *protocol.UpdateManifest) { m.MinProtocol = 3 }, nil, ReasonProtocolTooOld},
		{"url fuori dal repository", func(m *protocol.UpdateManifest) { m.URL = "https://releases.banco/releases/download/v0.7.0/x" }, nil, ReasonBadURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newMBench(t, true)
			b.m.Attach(b.out)
			defer b.m.Detach()
			m := withSHA(newManifest("0.7.0"), []byte("x"))
			if c.mod != nil {
				c.mod(&m)
			}
			var sig []byte
			if c.sig != nil {
				sig = c.sig(t, b, m)
			}
			b.m.Handle(b.available(t, m, sig, ""))
			b.m.Wait()
			waitFor(t, "update.result", func() bool { return len(b.out.results(t)) == 1 })
			r := b.out.results(t)[0]
			if r.Outcome != protocol.UpdateFailed || r.Reason != c.reason || r.Version != m.Version {
				t.Fatalf("%+v, atteso failed %s", r, c.reason)
			}
			if b.getCount() != 0 {
				t.Fatal("download fatto nonostante il rifiuto")
			}
			if entries, _ := os.ReadDir(b.dir); len(entries) != 0 {
				t.Fatalf("file nella cassetta: %v", entries)
			}
		})
	}
	// Un update.available malformato è MESSAGE_INVALID, non un update.result.
	b := newMBench(t, true)
	b.m.Attach(b.out)
	defer b.m.Detach()
	env := b.available(t, withSHA(newManifest("0.7.0"), []byte("x")), nil, "")
	env.Body = []byte(strings.Replace(string(env.Body), `"version":"0.7.0","manifest"`, `"version":"0.7.1","manifest"`, 1))
	b.m.Handle(env)
	if b.out.count(protocol.TypeError) != 1 || len(b.out.results(t)) != 0 {
		t.Fatalf("messaggio malformato: %+v", b.out.all())
	}
}

// Download rifiutato: sha diverso, file assente (404), tetto.
func TestManagerRiportaIlDownloadFallito(t *testing.T) {
	b := newMBench(t, true)
	b.m.Attach(b.out)
	defer b.m.Detach()
	body := []byte("binario nuovo 0.7.0")
	m := withSHA(newManifest("0.7.0"), []byte("un altro contenuto"))
	b.serve(m, body)
	b.m.Handle(b.available(t, m, nil, ""))
	b.m.Wait()
	waitFor(t, "sha256-mismatch", func() bool { return len(b.out.results(t)) == 1 })
	if r := b.out.results(t)[0]; r.Outcome != protocol.UpdateFailed || r.Reason != ReasonSHA256Mismatch {
		t.Fatalf("%+v", r)
	}
	m2 := withSHA(newManifest("0.7.1"), body) // non servito: 404
	b.m.Handle(b.available(t, m2, nil, ""))
	b.m.Wait()
	waitFor(t, "download-failed", func() bool { return len(b.out.results(t)) == 2 })
	if r := b.out.results(t)[1]; r.Outcome != protocol.UpdateFailed || r.Reason != ReasonDownloadFailed || r.Version != "0.7.1" {
		t.Fatalf("%+v", r)
	}
	if entries, _ := os.ReadDir(b.dir); len(entries) != 0 {
		t.Fatalf("file nella cassetta dopo i rifiuti: %v", entries)
	}
}

// Gli interruttori (D6): skipped con host-config o tenant-switch, nessun download; security ignora
// solo quello del backend.
func TestManagerInterruttori(t *testing.T) {
	body := []byte("binario nuovo 0.7.0")
	sec := true
	cases := []struct {
		name          string
		host, backend bool
		security      *bool
		reason        string // vuoto = scarica
	}{
		{"file locale spento", false, true, nil, ReasonHostConfig},
		{"file locale spento anche con security", false, true, &sec, ReasonHostConfig},
		{"interruttore del backend spento", true, false, nil, ReasonTenantSwitch},
		{"backend spento ma security", true, false, &sec, ""},
		{"tutto acceso", true, true, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newMBench(t, c.host)
			b.m.Attach(b.out)
			defer b.m.Detach()
			off := c.backend
			b.m.SetBackendAutoUpdate(&off)
			m := withSHA(newManifest("0.7.0"), body)
			m.Security = c.security
			b.serve(m, body)
			b.m.Handle(b.available(t, m, nil, ""))
			b.m.Wait()
			if c.reason == "" {
				waitFor(t, "download", func() bool { _, err := os.Lstat(filepath.Join(b.dir, FileApplyRequest)); return err == nil })
				if len(b.out.results(t)) != 0 {
					t.Fatalf("update.result inatteso: %+v", b.out.results(t))
				}
				return
			}
			waitFor(t, "skipped", func() bool { return len(b.out.results(t)) == 1 })
			if r := b.out.results(t)[0]; r.Outcome != protocol.UpdateSkipped || r.Reason != c.reason || r.Version != "0.7.0" {
				t.Fatalf("%+v, atteso skipped %s", r, c.reason)
			}
			if b.getCount() != 0 {
				t.Fatal("download con l'interruttore spento")
			}
		})
	}
	// L'interruttore del backend assente vale acceso.
	b := newMBench(t, true)
	b.m.SetBackendAutoUpdate(nil)
	b.m.Attach(b.out)
	defer b.m.Detach()
	m := withSHA(newManifest("0.7.0"), body)
	b.serve(m, body)
	b.m.Handle(b.available(t, m, nil, ""))
	b.m.Wait()
	waitFor(t, "download con interruttore assente", func() bool { _, err := os.Lstat(filepath.Join(b.dir, FileApplyRequest)); return err == nil })
}

// Dopo welcome la cassetta si legge e si riporta; i file si tolgono SOLO dopo la consegna (punto 3
// della revisione); mentre si è collegati, un apply.rejected lasciato dall'updater arriva al backend
// entro l'intervallo di rilettura; l'ok della propria versione arriva dopo la finestra di collegamento
// (punto 7) e il marcatore si scrive solo allora.
func TestManagerRiportaLaCassetta(t *testing.T) {
	b := newMBench(t, true)
	mb, _ := OpenMailbox(b.dir)
	defer mb.Close()
	_ = mb.WriteOutcome(FileRolledBack, Outcome{Version: "0.7.0", Reason: ReasonNoWelcome})
	_ = mb.WriteOutcome(FileApplied, Pending{Version: "0.6.1", PreviousVersion: "0.6.0"})
	// Prima connessione: l'invio fallisce subito (connessione caduta): niente si consuma.
	b.out.setFail(true)
	b.m.Attach(b.out)
	time.Sleep(150 * time.Millisecond)
	b.m.Detach()
	if !mb.IsRegular(FileRolledBack) || !mb.IsRegular(FileApplied) {
		t.Fatal("esiti consumati senza consegna")
	}
	if mb.ConnectedExists("0.6.1") {
		t.Fatal("marcatore scritto prima della finestra di collegamento")
	}
	// Seconda connessione, che regge: rolled-back subito, ok dopo la finestra, poi tutto consumato.
	b.out.setFail(false)
	b.m.Attach(b.out)
	defer b.m.Detach()
	waitFor(t, "rolled-back dopo welcome", func() bool { return len(b.out.results(t)) == 1 })
	if r := b.out.results(t)[0]; r != (protocol.UpdateResult{Version: "0.7.0", Outcome: protocol.UpdateRolledBack, Reason: ReasonNoWelcome}) {
		t.Fatalf("esito: %+v", r)
	}
	waitFor(t, "rolled-back.json consumato dopo la consegna", func() bool { return !mb.IsRegular(FileRolledBack) })
	if mb.ConnectedExists("0.6.1") {
		t.Fatal("marcatore scritto prima della finestra")
	}
	waitFor(t, "ok dopo la finestra di collegamento", func() bool { return len(b.out.results(t)) == 2 })
	if r := b.out.results(t)[1]; r != (protocol.UpdateResult{Version: "0.6.1", Outcome: protocol.UpdateOK}) {
		t.Fatalf("ok: %+v", r)
	}
	if !mb.ConnectedExists("0.6.1") {
		t.Fatal("marcatore di collegamento non scritto")
	}
	waitFor(t, "applied.json consumato", func() bool { return !mb.IsRegular(FileApplied) })
	// Ora l'updater rifiuta qualcosa mentre siamo collegati: arriva come failed; un rifiuto per
	// interruttore arriva come skipped.
	_ = mb.WriteOutcome(FileApplyRejected, Outcome{Version: "0.8.0", Reason: ReasonBadSignature})
	waitFor(t, "apply.rejected riportato", func() bool { return len(b.out.results(t)) == 3 })
	if r := b.out.results(t)[2]; r != (protocol.UpdateResult{Version: "0.8.0", Outcome: protocol.UpdateFailed, Reason: ReasonBadSignature}) {
		t.Fatalf("rifiuto dell'updater: %+v", r)
	}
	waitFor(t, "apply.rejected consumato", func() bool { return !mb.IsRegular(FileApplyRejected) })
	_ = mb.WriteOutcome(FileApplyRejected, Outcome{Version: "0.8.0", Reason: ReasonHostConfig})
	waitFor(t, "host-config riportato", func() bool { return len(b.out.results(t)) == 4 })
	if r := b.out.results(t)[3]; r != (protocol.UpdateResult{Version: "0.8.0", Outcome: protocol.UpdateSkipped, Reason: ReasonHostConfig}) {
		t.Fatalf("rifiuto per interruttore: %+v", r)
	}
	// Un secondo Attach (nuovo welcome) non riporta due volte: aspetta il ciclo precedente.
	b.m.Detach()
	out2 := &fakeOut{}
	b.m.Attach(out2)
	waitFor(t, "update.check del secondo welcome", func() bool { return out2.count(protocol.TypeUpdateCheck) == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := len(out2.results(t)); n != 0 {
		t.Fatalf("esiti riportati di nuovo: %+v", out2.results(t))
	}
}

// Una connessione che cade prima della finestra non scrive il marcatore (crash-loop dopo welcome).
func TestManagerNienteMarcatoreSeLaConnessioneCade(t *testing.T) {
	b := newMBench(t, true)
	mb, _ := OpenMailbox(b.dir)
	defer mb.Close()
	_ = mb.WriteOutcome(FileApplied, Pending{Version: "0.6.1", PreviousVersion: "0.6.0"})
	for i := 0; i < 3; i++ {
		b.m.Attach(b.out)
		time.Sleep(60 * time.Millisecond) // meno della finestra (200 ms)
		b.m.Detach()
	}
	if mb.ConnectedExists("0.6.1") || len(b.out.results(t)) != 0 || !mb.IsRegular(FileApplied) {
		t.Fatalf("marcatore %v, esiti %+v", mb.ConnectedExists("0.6.1"), b.out.results(t))
	}
	b.m.Attach(b.out)
	defer b.m.Detach()
	waitFor(t, "ok con la connessione che regge", func() bool { return len(b.out.results(t)) == 1 })
	if !mb.ConnectedExists("0.6.1") {
		t.Fatal("marcatore non scritto dopo la finestra")
	}
}

func TestNextCheckConScarto(t *testing.T) {
	m, err := New(Config{Dir: t.TempDir(), Version: "0.6.1", HostAutoUpdate: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		d := m.nextCheck()
		if d < DefaultCheckInterval-DefaultCheckJitter || d > DefaultCheckInterval+DefaultCheckJitter {
			t.Fatalf("intervallo %v fuori da 6 h ± 30 min", d)
		}
	}
	m2, _ := New(Config{Dir: t.TempDir(), Version: "0.6.1", CheckInterval: time.Minute})
	if d := m2.nextCheck(); d != time.Minute {
		t.Fatalf("intervallo esplicito senza scarto: %v", d)
	}
}
