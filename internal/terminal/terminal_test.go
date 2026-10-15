// Test dei canali contro tmux vero, su un server privato (socket in una cartella temporanea): non
// toccano le sessioni dell'utente. Spec monsterterminal-saas.md §1.6, §1.7; ADR 0409 D4.
package terminal

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
)

type msg struct {
	typ, re string
	body    []byte
}

type fakeOut struct {
	mu       sync.Mutex
	msgs     []msg
	output   map[uint32]*bytes.Buffer
	discards []uint32
}

func (f *fakeOut) Control(typ, re string, body any) error {
	raw, _ := json.Marshal(body)
	f.mu.Lock()
	f.msgs = append(f.msgs, msg{typ, re, raw})
	f.mu.Unlock()
	return nil
}

func (f *fakeOut) Output(ch uint32, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.output[ch] == nil {
		f.output[ch] = &bytes.Buffer{}
	}
	f.output[ch].Write(data)
	return nil
}

func (f *fakeOut) Discard(ch uint32) { f.mu.Lock(); f.discards = append(f.discards, ch); f.mu.Unlock() }

func (f *fakeOut) out(ch uint32) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.output[ch] == nil {
		return ""
	}
	return f.output[ch].String()
}

func (f *fakeOut) find(typ, re string) (msg, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.msgs {
		if m.typ == typ && (re == "" || m.re == re) {
			return m, true
		}
	}
	return msg{}, false
}

type rig struct {
	t    *testing.T
	m    *Manager
	out  *fakeOut
	sock string
	n    int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux non installato")
	}
	dir, err := os.MkdirTemp("", "mt") // corto: i socket unix hanno un limite di ~100 caratteri
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s")
	r := &rig{t: t, out: &fakeOut{output: map[uint32]*bytes.Buffer{}}, sock: sock}
	r.m = New(Config{Socket: sock})
	r.m.Attach(r.out)
	t.Cleanup(func() {
		r.m.Detach(false)
		_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
	return r
}

func (r *rig) tmux(args ...string) (string, error) {
	out, err := exec.Command("tmux", append([]string{"-S", r.sock}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// send costruisce una busta come la manderebbe il backend.
func (r *rig) send(typ string, body any) string {
	r.t.Helper()
	r.n++
	raw, _ := json.Marshal(body)
	id := protocol.NewUUID()
	if !r.m.Handle(protocol.Envelope{Type: typ, ID: id, Body: raw}) {
		r.t.Fatalf("%s non gestito", typ)
	}
	return id
}

func (r *rig) open(ch uint32, session string, create bool, mode string) string {
	return r.send(protocol.TypeSessionOpen, protocol.SessionOpen{Channel: ch, Session: session, Create: create, Mode: mode, Cols: 100, Rows: 30})
}

func (r *rig) errorCode(re string) string {
	m, ok := r.out.find(protocol.TypeError, re)
	if !ok {
		return ""
	}
	var e protocol.ErrorBody
	_ = json.Unmarshal(m.body, &e)
	return e.Code
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("non avvenuto entro 10 s: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestApreScriveLeggeEChiudeSenzaUccidereLaSessione(t *testing.T) {
	r := newRig(t)
	id := r.open(1, "lavoro", true, protocol.ModeInteract)
	m, ok := r.out.find(protocol.TypeSessionOpened, id)
	if !ok {
		t.Fatalf("session.opened non inviato; messaggi %+v", r.out.msgs)
	}
	var op protocol.SessionOpened
	_ = json.Unmarshal(m.body, &op)
	if op.Channel != 1 || op.Cols != 100 || op.Rows != 30 {
		t.Fatalf("session.opened %+v", op)
	}
	r.m.Input(1, []byte("echo mt-$((40+2))-ok\r"))
	eventually(t, "output del comando", func() bool { return strings.Contains(r.out.out(1), "mt-42-ok") })

	r.send(protocol.TypeSessionClose, protocol.SessionClose{Channel: 1})
	if r.m.Open() != 0 {
		t.Fatal("canale ancora aperto dopo session.close")
	}
	if _, err := r.tmux("has-session", "-t", "=lavoro"); err != nil {
		t.Fatal("session.close ha ucciso la sessione tmux: deve solo staccare il client (ADR 0409 D4)")
	}
	if _, ok := r.out.find(protocol.TypeSessionClosed, ""); ok {
		t.Fatal("session.closed dopo una chiusura chiesta dal backend")
	}
	// Chiudere di nuovo non è un errore.
	r.send(protocol.TypeSessionClose, protocol.SessionClose{Channel: 1})
	if code := r.errorCode(""); code != "" {
		t.Fatalf("errore %s chiudendo un canale già chiuso", code)
	}
}

// Sola lettura: l'input si scarta sull'host (seconda barriera, §1.7).
func TestSolaLetturaScartaLInput(t *testing.T) {
	r := newRig(t)
	if _, err := r.tmux("new-session", "-d", "-s", "vista"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "non-deve-esistere")
	r.open(2, "vista", false, protocol.ModeView)
	eventually(t, "canale aperto", func() bool { return r.m.Open() == 1 })
	r.m.Input(2, []byte("touch "+marker+"\r"))
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("l'input di un canale in sola lettura ha eseguito un comando")
	}
	// Il client tmux è agganciato in sola lettura.
	if ro, _ := r.tmux("list-clients", "-F", "#{client_readonly}"); ro != "1" {
		t.Fatalf("client tmux non in sola lettura: %q", ro)
	}
}

func TestSessioneInesistenteSenzaCreate(t *testing.T) {
	r := newRig(t)
	if _, err := r.tmux("new-session", "-d", "-s", "lavoro"); err != nil {
		t.Fatal(err)
	}
	// «lav» non deve agganciare «lavoro» per prefisso.
	id := r.open(3, "lav", false, protocol.ModeInteract)
	if code := r.errorCode(id); code != protocol.CodeSessionNotFound {
		t.Fatalf("codice %q, atteso SESSION_NOT_FOUND", code)
	}
	if r.m.Open() != 0 {
		t.Fatal("canale aperto su una sessione inesistente")
	}
}

func TestNomeFuoriRegola(t *testing.T) {
	r := newRig(t)
	raw := []byte(`{"channel":4,"session":"a;rm -rf","create":true,"mode":"interact","cols":80,"rows":24}`)
	id := protocol.NewUUID()
	r.m.Handle(protocol.Envelope{Type: protocol.TypeSessionOpen, ID: id, Body: raw})
	if code := r.errorCode(id); code != protocol.CodeSessionNameInvalid {
		t.Fatalf("codice %q, atteso SESSION_NAME_INVALID", code)
	}
	if out, _ := r.tmux("list-sessions"); out != "" && !strings.Contains(out, "no server") && !strings.Contains(out, "error connecting") {
		t.Fatalf("una sessione è stata creata: %s", out)
	}
}

func TestTettoDeiCanali(t *testing.T) {
	r := newRig(t)
	r.m.SetMaxChannels(1)
	r.open(1, "uno", true, protocol.ModeInteract)
	id := r.open(2, "due", true, protocol.ModeInteract)
	if code := r.errorCode(id); code != protocol.CodeChannelLimit {
		t.Fatalf("codice %q, atteso CHANNEL_LIMIT", code)
	}
	id = r.open(1, "uno", false, protocol.ModeInteract)
	if code := r.errorCode(id); code != protocol.CodeMessageInvalid && code != protocol.CodeChannelLimit {
		t.Fatalf("canale duplicato: codice %q", code)
	}
}

func TestDuplicatoRifiutato(t *testing.T) {
	r := newRig(t)
	r.open(1, "uno", true, protocol.ModeInteract)
	id := r.open(1, "uno", false, protocol.ModeInteract)
	if code := r.errorCode(id); code != protocol.CodeMessageInvalid {
		t.Fatalf("canale duplicato: codice %q, atteso MESSAGE_INVALID", code)
	}
}

func TestRidimensionamento(t *testing.T) {
	r := newRig(t)
	r.open(1, "dim", true, protocol.ModeInteract)
	eventually(t, "client agganciato", func() bool {
		out, _ := r.tmux("list-clients", "-F", "#{client_width}x#{client_height}")
		return out == "100x30"
	})
	r.send(protocol.TypeSessionResize, protocol.SessionResize{Channel: 1, Cols: 132, Rows: 40})
	eventually(t, "nuove dimensioni", func() bool {
		out, _ := r.tmux("list-clients", "-F", "#{client_width}x#{client_height}")
		return out == "132x40"
	})
	id := r.send(protocol.TypeSessionResize, protocol.SessionResize{Channel: 9, Cols: 80, Rows: 24})
	if code := r.errorCode(id); code != protocol.CodeChannelUnknown {
		t.Fatalf("resize di un canale non aperto: codice %q", code)
	}
}

// La sessione finisce sull'host: il canale si chiude con session.closed «session-ended».
func TestSessioneFinitaSullHost(t *testing.T) {
	r := newRig(t)
	r.open(5, "breve", true, protocol.ModeInteract)
	eventually(t, "canale aperto", func() bool { return r.m.Open() == 1 })
	if _, err := r.tmux("kill-session", "-t", "=breve"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "session.closed", func() bool { _, ok := r.out.find(protocol.TypeSessionClosed, ""); return ok })
	m, _ := r.out.find(protocol.TypeSessionClosed, "")
	var c protocol.SessionClosed
	_ = json.Unmarshal(m.body, &c)
	if c.Channel != 5 || c.Reason != protocol.ClosedSessionEnded {
		t.Fatalf("session.closed %+v", c)
	}
	if r.m.Open() != 0 {
		t.Fatal("canale ancora registrato")
	}
}

// Caduta della connessione: i canali si chiudono, le sessioni restano; allo spegnimento il backend
// viene avvisato con «connector-shutdown».
func TestDetachLasciaVivereLeSessioni(t *testing.T) {
	r := newRig(t)
	r.open(1, "a", true, protocol.ModeInteract)
	r.open(2, "b", true, protocol.ModeInteract)
	eventually(t, "due canali", func() bool { return r.m.Open() == 2 })
	r.m.Detach(true)
	if r.m.Open() != 0 {
		t.Fatal("canali rimasti dopo Detach")
	}
	n := 0
	r.out.mu.Lock()
	for _, m := range r.out.msgs {
		if m.typ == protocol.TypeSessionClosed && strings.Contains(string(m.body), protocol.ClosedConnectorShutdown) {
			n++
		}
	}
	r.out.mu.Unlock()
	if n != 2 {
		t.Fatalf("session.closed connector-shutdown: %d, attesi 2", n)
	}
	for _, s := range []string{"a", "b"} {
		if _, err := r.tmux("has-session", "-t", "="+s); err != nil {
			t.Fatalf("la sessione %s non è sopravvissuta alla caduta", s)
		}
	}
	// Dopo la caduta l'input non va da nessuna parte e non rompe nulla.
	r.m.Input(1, []byte("x"))
}

func TestElencoDelleSessioni(t *testing.T) {
	r := newRig(t)
	id := r.send(protocol.TypeSessionsList, struct{}{})
	m, ok := r.out.find(protocol.TypeSessionsListResult, id)
	if !ok || string(m.body) != `{"sessions":[]}` {
		t.Fatalf("senza server tmux atteso elenco vuoto, trovato %s", m.body)
	}
	for _, s := range []string{"uno", "nome con spazi"} {
		if _, err := r.tmux("new-session", "-d", "-s", s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.tmux("new-window", "-t", "=uno"); err != nil {
		t.Fatal(err)
	}
	id = r.send(protocol.TypeSessionsList, struct{}{})
	m, _ = r.out.find(protocol.TypeSessionsListResult, id)
	var res protocol.SessionsListResult
	_ = json.Unmarshal(m.body, &res)
	if len(res.Sessions) != 1 || res.Sessions[0].Name != "uno" || res.Sessions[0].Windows != 2 || res.Sessions[0].Attached {
		t.Fatalf("elenco %+v (il nome fuori regola non va elencato)", res.Sessions)
	}
	if _, err := time.Parse(time.RFC3339Nano, res.Sessions[0].CreatedAt); err != nil {
		t.Fatalf("createdAt %q", res.Sessions[0].CreatedAt)
	}
}

func TestMessaggiNonDelTerminale(t *testing.T) {
	r := newRig(t)
	if r.m.Handle(protocol.Envelope{Type: protocol.TypeConfigUpdate, ID: protocol.NewUUID(), Body: []byte(`{}`)}) {
		t.Fatal("config.update preso dal terminale")
	}
	r.m.Input(77, []byte("x")) // canale mai aperto: si scarta senza messaggi
	r.out.mu.Lock()
	n := len(r.out.msgs)
	r.out.mu.Unlock()
	if n != 0 {
		t.Fatalf("messaggi inviati: %d", n)
	}
	for _, bad := range []string{`{"channel":1}`, `{"channel":1,"cols":10,"rows":0}`} {
		id := protocol.NewUUID()
		r.m.Handle(protocol.Envelope{Type: protocol.TypeSessionResize, ID: id, Body: []byte(bad)})
		if code := r.errorCode(id); code != protocol.CodeMessageInvalid {
			t.Errorf("%s: codice %q", bad, code)
		}
	}
}

// La seconda barriera in sé, senza tmux: in sola lettura Input non scrive nulla nel pty, anche se il
// client tmux accettasse input. In interazione lo scrive.
func TestBarrieraSullHostSenzaContareSuTmux(t *testing.T) {
	m := New(Config{})
	for _, view := range []bool{true, false} {
		rd, wr, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		m.channels[1] = &channel{id: 1, view: view, pty: wr, finished: make(chan struct{})}
		m.Input(1, []byte("rm -rf /\r"))
		wr.Close()
		got, _ := io.ReadAll(rd)
		rd.Close()
		if view && len(got) != 0 {
			t.Fatalf("in sola lettura sono arrivati %q al pty", got)
		}
		if !view && string(got) != "rm -rf /\r" {
			t.Fatalf("in interazione il pty ha ricevuto %q", got)
		}
	}
}
