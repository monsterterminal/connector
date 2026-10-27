// Test dei comandi del terminale unico (schemi 0.6.0, ADR 0409 A1.2) contro tmux vero, sul server privato
// del banco di terminal_test.go. Asse A: ogni comando fa ciò che dice; asse B: risposte e codici di
// errore come da A1.2 (risposta <tipo>.result per re; SESSION_NOT_FOUND, SESSION_NAME_INVALID,
// CHANNEL_UNKNOWN, SESSION_EXISTS, CAPTURE_TOO_LARGE, TMUX_UNAVAILABLE).
package terminal

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

// result decodifica la risposta <typ> a re, o fallisce col contenuto dei messaggi.
func (r *rig) result(typ, re string, into any) {
	r.t.Helper()
	m, ok := r.out.find(typ, re)
	if !ok {
		m, _ := r.out.find(protocol.TypeError, re)
		r.t.Fatalf("%s non ricevuto: errore %s", typ, m.body)
	}
	if err := json.Unmarshal(m.body, into); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) newSession(name string, extra ...string) {
	r.t.Helper()
	if out, err := r.tmux(append([]string{"new-session", "-d", "-s", name}, extra...)...); err != nil {
		r.t.Fatalf("new-session %s: %v %s", name, err, out)
	}
}

func TestKillChiudeLaSessioneEICanaliRicevonoSessionEnded(t *testing.T) {
	r := newRig(t)
	r.newSession("lavoro")
	r.open(1, "lavoro", false, protocol.ModeInteract)
	eventually(t, "canale aperto", func() bool { return r.m.Open() == 1 })

	id := r.send(protocol.TypeSessionKill, protocol.SessionBody{Session: "lavoro"})
	var res protocol.SessionBody
	r.result(protocol.TypeSessionKillResult, id, &res)
	if res.Session != "lavoro" {
		t.Fatalf("kill.result %+v", res)
	}
	if _, err := r.tmux("has-session", "-t", "=lavoro"); err == nil {
		t.Fatal("la sessione è ancora viva dopo session.kill")
	}
	eventually(t, "session.closed session-ended", func() bool {
		m, ok := r.out.find(protocol.TypeSessionClosed, "")
		var c protocol.SessionClosed
		_ = json.Unmarshal(m.body, &c)
		return ok && c.Channel == 1 && c.Reason == protocol.ClosedSessionEnded
	})
	if code := r.errorCode(r.send(protocol.TypeSessionKill, protocol.SessionBody{Session: "lavoro"})); code != protocol.CodeSessionNotFound {
		t.Errorf("kill di una sessione inesistente: %q", code)
	}
	if code := r.errorCode(r.send(protocol.TypeSessionKill, map[string]string{"session": "a b"})); code != protocol.CodeSessionNameInvalid {
		t.Errorf("kill con un nome fuori regola: %q", code)
	}
}

// «lav» non deve chiudere «lavoro» per prefisso: il bersaglio è esatto.
func TestKillNonPrendePerPrefisso(t *testing.T) {
	r := newRig(t)
	r.newSession("lavoro")
	if code := r.errorCode(r.send(protocol.TypeSessionKill, protocol.SessionBody{Session: "lav"})); code != protocol.CodeSessionNotFound {
		t.Fatalf("kill di «lav»: %q", code)
	}
	if _, err := r.tmux("has-session", "-t", "=lavoro"); err != nil {
		t.Fatal("session.kill «lav» ha chiuso «lavoro»")
	}
}

func TestRenameTieneICanaliEDistingueINomiInUso(t *testing.T) {
	r := newRig(t)
	r.newSession("vecchio")
	r.newSession("altro")
	r.open(1, "vecchio", false, protocol.ModeInteract)
	eventually(t, "canale aperto", func() bool { return r.m.Open() == 1 })

	if code := r.errorCode(r.send(protocol.TypeSessionRename, protocol.SessionRename{Session: "vecchio", Name: "altro"})); code != protocol.CodeSessionExists {
		t.Fatalf("rename verso un nome in uso: %q", code)
	}
	id := r.send(protocol.TypeSessionRename, protocol.SessionRename{Session: "vecchio", Name: "nuovo"})
	var res protocol.SessionRename
	r.result(protocol.TypeSessionRenameResult, id, &res)
	if res.Session != "vecchio" || res.Name != "nuovo" {
		t.Fatalf("rename.result %+v", res)
	}
	if _, err := r.tmux("has-session", "-t", "=nuovo"); err != nil {
		t.Fatal("la sessione non si chiama nuovo")
	}
	// Il canale resta aperto e segue il nome nuovo: l'interruzione colpisce «nuovo».
	if r.m.Open() != 1 {
		t.Fatal("il canale si è chiuso con la rinomina")
	}
	r.result(protocol.TypeSessionInterruptResult, r.send(protocol.TypeSessionInterrupt, protocol.ChannelBody{Channel: 1}), &protocol.ChannelBody{})
	// Stesso nome: risposta normale.
	r.result(protocol.TypeSessionRenameResult, r.send(protocol.TypeSessionRename, protocol.SessionRename{Session: "nuovo", Name: "nuovo"}), &res)
	if code := r.errorCode(r.send(protocol.TypeSessionRename, protocol.SessionRename{Session: "manca", Name: "x"})); code != protocol.CodeSessionNotFound {
		t.Errorf("rename di una sessione inesistente: %q", code)
	}
	if code := r.errorCode(r.send(protocol.TypeSessionRename, map[string]string{"session": "nuovo", "name": "-x"})); code != protocol.CodeSessionNameInvalid {
		t.Errorf("rename verso un nome fuori regola: %q", code)
	}
}

func TestZoomImpostaOInverteEConUnSoloPaneNonFaNulla(t *testing.T) {
	r := newRig(t)
	r.newSession("z")
	zoom := func(on *bool) protocol.SessionZoomResult {
		t.Helper()
		var res protocol.SessionZoomResult
		r.result(protocol.TypeSessionZoomResult, r.send(protocol.TypeSessionZoom, protocol.SessionZoom{Session: "z", On: on}), &res)
		return res
	}
	yes, no := true, false
	if res := zoom(&yes); res.On || res.Panes != 1 {
		t.Fatalf("un solo pane: %+v, atteso on false e panes 1", res)
	}
	if _, err := r.tmux("split-window", "-d", "-t", "=z:"); err != nil {
		t.Fatal(err)
	}
	if res := zoom(&yes); !res.On || res.Panes != 2 {
		t.Fatalf("zoom acceso: %+v", res)
	}
	if res := zoom(&yes); !res.On {
		t.Fatalf("on=true due volte deve restare acceso (imposta, non inverte): %+v", res)
	}
	if res := zoom(nil); res.On {
		t.Fatalf("senza on si inverte: %+v", res)
	}
	if res := zoom(nil); !res.On {
		t.Fatalf("senza on si inverte di nuovo: %+v", res)
	}
	if res := zoom(&no); res.On {
		t.Fatalf("on=false: %+v", res)
	}
	if flag, _ := r.tmux("display-message", "-p", "-t", "=z:", "#{window_zoomed_flag}"); flag != "0" {
		t.Fatalf("tmux dice zoom %q dopo on=false", flag)
	}
}

func TestCatturaPerRangeETettoDeiDuecentocinquantaseiKiB(t *testing.T) {
	r := newRig(t)
	r.newSession("cap", "-x", "200", "-y", "20", "sh", "-c", "for i in $(seq 1 300); do echo riga-$i; done; echo segno-finale; sleep 600")
	capture := func(rng string) string {
		t.Helper()
		var res protocol.SessionCaptureResult
		r.result(protocol.TypeSessionCaptureResult, r.send(protocol.TypeSessionCapture, protocol.SessionCapture{Session: "cap", Range: rng}), &res)
		return res.Text
	}
	eventually(t, "output nella pane", func() bool { return strings.Contains(capture("screen"), "segno-finale") })
	if s := capture("screen"); strings.Contains(s, "riga-10\n") {
		t.Error("screen contiene la storia")
	}
	if s := capture("last200"); !strings.Contains(s, "riga-150\n") || strings.Contains(s, "riga-50\n") {
		t.Error("last200 non è la storia delle ultime 200 righe")
	}
	if s := capture("last1000"); !strings.Contains(s, "riga-1\n") {
		t.Error("last1000 senza le prime righe")
	}
	// view: fuori dalla copy-mode è lo schermo; scorrendo è ciò che l'utente guarda.
	if s := capture("view"); !strings.Contains(s, "segno-finale") {
		t.Error("view fuori dalla copy-mode senza lo schermo vivo")
	}
	if _, err := r.tmux("copy-mode", "-t", "=cap:"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.tmux("send-keys", "-t", "=cap:", "-X", "-N", "100", "scroll-up"); err != nil {
		t.Fatal(err)
	}
	if s := capture("view"); strings.Contains(s, "segno-finale") || !strings.Contains(s, "riga-") {
		t.Errorf("view in copy-mode non segue lo scorrimento:\n%s", s)
	}
	if code := r.errorCode(r.send(protocol.TypeSessionCapture, map[string]string{"session": "cap", "range": "all"})); code != protocol.CodeMessageInvalid {
		t.Errorf("range fuori enum: %q", code)
	}
	if code := r.errorCode(r.send(protocol.TypeSessionCapture, protocol.SessionCapture{Session: "manca", Range: "screen"})); code != protocol.CodeSessionNotFound {
		t.Errorf("sessione inesistente: %q", code)
	}
	// Oltre 256 KiB: 1000 righe di 400 caratteri sono ~400 KiB.
	r.newSession("grande", "-x", "500", "-y", "20", "sh", "-c", "awk 'BEGIN{for(i=0;i<1200;i++)printf \"%0400d\\n\", i}'; echo fatto; sleep 600")
	eventually(t, "output grande", func() bool {
		out, _ := r.tmux("capture-pane", "-p", "-t", "=grande:")
		return strings.Contains(out, "fatto")
	})
	if code := r.errorCode(r.send(protocol.TypeSessionCapture, protocol.SessionCapture{Session: "grande", Range: "last1000"})); code != protocol.CodeCaptureTooLarge {
		t.Errorf("cattura oltre 256 KiB: %q", code)
	}
	var res protocol.SessionCaptureResult
	r.result(protocol.TypeSessionCaptureResult, r.send(protocol.TypeSessionCapture, protocol.SessionCapture{Session: "grande", Range: "last200"}), &res)
	if len(res.Text) > protocol.MaxCaptureBytes {
		t.Errorf("last200 di %d byte oltre il tetto", len(res.Text))
	}
}

func TestBufferRecenteONull(t *testing.T) {
	r := newRig(t)
	r.newSession("b")
	r.open(1, "b", false, protocol.ModeView)
	eventually(t, "canale aperto", func() bool { return r.m.Open() == 1 })
	var res protocol.SessionBufferResult
	r.result(protocol.TypeSessionBufferResult, r.send(protocol.TypeSessionBuffer, protocol.ChannelBody{Channel: 1}), &res)
	if res.Text != nil {
		t.Fatalf("nessun buffer: atteso null, trovato %q", *res.Text)
	}
	if _, err := r.tmux("set-buffer", "selezione è qui"); err != nil {
		t.Fatal(err)
	}
	id := r.send(protocol.TypeSessionBuffer, protocol.ChannelBody{Channel: 1})
	r.result(protocol.TypeSessionBufferResult, id, &res)
	if res.Text == nil || *res.Text != "selezione è qui" || res.Channel != 1 {
		t.Fatalf("buffer recente: %+v", res)
	}
	if code := r.errorCode(r.send(protocol.TypeSessionBuffer, protocol.ChannelBody{Channel: 9})); code != protocol.CodeChannelUnknown {
		t.Errorf("canale sconosciuto: %q", code)
	}
}

func TestTroncaUTF8SenzaSpezzareICaratteri(t *testing.T) {
	if got := truncateUTF8("aèb", 2); got != "a" {
		t.Errorf("%q", got)
	}
	if got := truncateUTF8("aèb", 3); got != "aè" {
		t.Errorf("%q", got)
	}
	if got := truncateUTF8("abc", 5); got != "abc" {
		t.Errorf("%q", got)
	}
}

func TestInterruptEsceDallaCopyModeEFermaIlProcesso(t *testing.T) {
	r := newRig(t)
	r.newSession("int", "sh")
	r.open(1, "int", false, protocol.ModeInteract)
	r.open(2, "int", false, protocol.ModeView)
	eventually(t, "canali aperti", func() bool { return r.m.Open() == 2 })
	if _, err := r.tmux("send-keys", "-t", "=int:", "sleep 300", "Enter"); err != nil {
		t.Fatal(err)
	}
	cmdNow := func() string {
		s, _ := r.tmux("display-message", "-p", "-t", "=int:", "#{pane_current_command}")
		return s
	}
	eventually(t, "sleep in esecuzione", func() bool { return cmdNow() == "sleep" })
	if _, err := r.tmux("copy-mode", "-t", "=int:"); err != nil {
		t.Fatal(err)
	}
	// In sola lettura non si interrompe nulla.
	if code := r.errorCode(r.send(protocol.TypeSessionInterrupt, protocol.ChannelBody{Channel: 2})); code != protocol.CodeMessageInvalid {
		t.Fatalf("interrupt da un canale in sola lettura: %q", code)
	}
	time.Sleep(200 * time.Millisecond)
	if cmdNow() != "sleep" {
		t.Fatal("il canale in sola lettura ha interrotto il processo")
	}
	var res protocol.ChannelBody
	r.result(protocol.TypeSessionInterruptResult, r.send(protocol.TypeSessionInterrupt, protocol.ChannelBody{Channel: 1}), &res)
	eventually(t, "sleep interrotto", func() bool { return cmdNow() != "sleep" })
	if mode, _ := r.tmux("display-message", "-p", "-t", "=int:", "#{pane_in_mode}"); mode != "0" {
		t.Errorf("ancora in copy-mode: %q", mode)
	}
}

func TestRefreshRispondeSulCanaleAncheSubitoDopoLApertura(t *testing.T) {
	r := newRig(t)
	r.newSession("rf")
	// Subito dopo l'apertura: il connettore aspetta che il client compaia in list-clients.
	r.open(1, "rf", false, protocol.ModeView)
	var res protocol.ChannelBody
	r.result(protocol.TypeSessionRefreshResult, r.send(protocol.TypeSessionRefresh, protocol.ChannelBody{Channel: 1}), &res)
	if res.Channel != 1 {
		t.Fatalf("refresh.result %+v", res)
	}
	if code := r.errorCode(r.send(protocol.TypeSessionRefresh, protocol.ChannelBody{Channel: 5})); code != protocol.CodeChannelUnknown {
		t.Errorf("canale sconosciuto: %q", code)
	}
}

// fitToPane: il canale nasce con la finestra tmux (più la barra di stato) e la finestra non cambia.
func TestFitToPaneUsaLaFinestraEDichiaraLeDimensioniVere(t *testing.T) {
	r := newRig(t)
	r.newSession("largo", "-x", "150", "-y", "40")
	id := r.send(protocol.TypeSessionOpen, protocol.SessionOpen{Channel: 1, Session: "largo", Mode: protocol.ModeView, Cols: 80, Rows: 24, FitToPane: true})
	var op protocol.SessionOpened
	r.result(protocol.TypeSessionOpened, id, &op)
	if op.Cols != 150 || op.Rows != 41 {
		t.Fatalf("session.opened %dx%d, atteso 150x41 (40 righe più la barra di stato)", op.Cols, op.Rows)
	}
	time.Sleep(300 * time.Millisecond)
	if size, _ := r.tmux("display-message", "-p", "-t", "=largo:", "#{window_width}x#{window_height}"); size != "150x40" {
		t.Fatalf("la finestra è diventata %s: l'aggancio l'ha ridimensionata", size)
	}
	// Senza fitToPane resta com'era: le dimensioni del client.
	r.newSession("stretto", "-x", "150", "-y", "40")
	id = r.send(protocol.TypeSessionOpen, protocol.SessionOpen{Channel: 2, Session: "stretto", Mode: protocol.ModeView, Cols: 80, Rows: 24})
	r.result(protocol.TypeSessionOpened, id, &op)
	if op.Cols != 80 || op.Rows != 24 {
		t.Fatalf("senza fitToPane: %dx%d", op.Cols, op.Rows)
	}
}

func TestTmuxIrraggiungibileETmuxUnavailable(t *testing.T) {
	m := New(Config{Tmux: "/inesistente/tmux"})
	out := &fakeOut{output: nil}
	m.Attach(out)
	raw, _ := json.Marshal(protocol.SessionBody{Session: "a"})
	m.Handle(protocol.Envelope{Type: protocol.TypeSessionKill, ID: "k", Body: raw})
	msg, ok := out.find(protocol.TypeError, "k")
	var e protocol.ErrorBody
	_ = json.Unmarshal(msg.body, &e)
	if !ok || e.Code != protocol.CodeTmuxUnavailable {
		t.Fatalf("tmux assente: %+v", e)
	}
}
