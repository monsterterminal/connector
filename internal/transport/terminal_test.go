// Test del terminale dal capo all'altro: server finto ↔ trasporto ↔ canali su tmux vero, con un server
// tmux privato. Spec monsterterminal-saas.md §1.6, §1.7, §1.17; ADR 0409 D4.
package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/terminal"
	"github.com/monsterterminal/connector/internal/transport"
)

type termRig struct {
	s    *fakeserver.Server
	id   string
	sock string
	mgr  *terminal.Manager
	r    *run
}

func newTermRig(t *testing.T, mod func(*fakeserver.Server)) *termRig {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux non installato")
	}
	dir, err := os.MkdirTemp("", "mt")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s")
	s := fakeserver.New()
	if mod != nil {
		mod(s)
	}
	id, tok := s.AddHost()
	mgr := terminal.New(terminal.Config{Socket: sock})
	tr := &termRig{s: s, id: id, sock: sock, mgr: mgr}
	tr.r = start(t, s, tok, func(c *transport.Config) {
		c.Terminal = mgr
		c.Sleep = realSleep
		c.Backoff = &transport.Backoff{Min: 20 * time.Millisecond, Max: 100 * time.Millisecond}
	})
	t.Cleanup(func() {
		tr.r.cancel()
		s.Close()
		_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
	waitHello(t, s)
	waitWelcome(t, tr.r)
	return tr
}

func (tr *termRig) tmux(args ...string) (string, error) {
	out, err := exec.Command("tmux", append([]string{"-S", tr.sock}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (tr *termRig) send(t *testing.T, typ string, body any) string {
	t.Helper()
	id, err := tr.s.SendControl(tr.id, typ, body)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (tr *termRig) reply(t *testing.T, id string) protocol.Envelope {
	t.Helper()
	var env protocol.Envelope
	eventually(t, "risposta a "+id, func() bool { var ok bool; env, ok = tr.s.Reply(tr.id, id); return ok })
	return env
}

func (tr *termRig) open(t *testing.T, ch uint32, session string, create bool, mode string) protocol.Envelope {
	t.Helper()
	id := tr.send(t, protocol.TypeSessionOpen, protocol.SessionOpen{Channel: ch, Session: session, Create: create, Mode: mode, Cols: 100, Rows: 30})
	return tr.reply(t, id)
}

func errCode(env protocol.Envelope) string {
	if env.Type != protocol.TypeError {
		return ""
	}
	var e protocol.ErrorBody
	_ = json.Unmarshal(env.Body, &e)
	return e.Code
}

func TestTerminaleApreScriveLeggeEChiude(t *testing.T) {
	tr := newTermRig(t, nil)
	if env := tr.open(t, 1, "lavoro", true, protocol.ModeInteract); env.Type != protocol.TypeSessionOpened {
		t.Fatalf("risposta %s %s", env.Type, env.Body)
	}
	if err := tr.s.SendInput(tr.id, 1, []byte("echo mt-$((40+2))-ok\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "output del comando al backend", func() bool { return bytes.Contains(tr.s.Output(tr.id, 1), []byte("mt-42-ok")) })
	tr.send(t, protocol.TypeSessionResize, protocol.SessionResize{Channel: 1, Cols: 120, Rows: 40})
	eventually(t, "ridimensionato", func() bool {
		out, _ := tr.tmux("list-clients", "-F", "#{client_width}x#{client_height}")
		return out == "120x40"
	})
	tr.send(t, protocol.TypeSessionClose, protocol.SessionClose{Channel: 1})
	eventually(t, "client staccato", func() bool { out, _ := tr.tmux("list-clients"); return out == "" })
	if _, err := tr.tmux("has-session", "-t", "=lavoro"); err != nil {
		t.Fatal("la sessione tmux non è sopravvissuta a session.close")
	}
	id := tr.send(t, protocol.TypeSessionsList, struct{}{})
	var res protocol.SessionsListResult
	_ = json.Unmarshal(tr.reply(t, id).Body, &res)
	if len(res.Sessions) != 1 || res.Sessions[0].Name != "lavoro" {
		t.Fatalf("sessions.list.result %+v", res)
	}
}

// Sola lettura: anche se il backend mandasse input, sull'host non arriva.
func TestTerminaleSolaLettura(t *testing.T) {
	tr := newTermRig(t, nil)
	if _, err := tr.tmux("new-session", "-d", "-s", "vista"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "non-deve-esistere")
	tr.open(t, 2, "vista", false, protocol.ModeView)
	_ = tr.s.SendInput(tr.id, 2, []byte("touch "+marker+"\r"))
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("input eseguito su un canale in sola lettura")
	}
}

// maxChannels arriva con welcome (§1.11).
func TestTerminaleTettoDaWelcome(t *testing.T) {
	tr := newTermRig(t, func(s *fakeserver.Server) { s.MaxChannels = 1 })
	tr.open(t, 1, "uno", true, protocol.ModeInteract)
	if code := errCode(tr.open(t, 2, "due", true, protocol.ModeInteract)); code != protocol.CodeChannelLimit {
		t.Fatalf("codice %q, atteso CHANNEL_LIMIT", code)
	}
}

// Caduta della connessione: il canale si chiude, la sessione tmux continua (ADR 0409 D4); dopo il
// ricollegamento la si riapre e si ritrova quello che è successo nel frattempo.
func TestTerminaleSopravviveAllaCaduta(t *testing.T) {
	tr := newTermRig(t, nil)
	tr.open(t, 1, "lunga", true, protocol.ModeInteract)
	_ = tr.s.SendInput(tr.id, 1, []byte("sleep 1; echo dopo-la-$((6*7))-caduta\r"))
	time.Sleep(100 * time.Millisecond)
	tr.s.Drop(tr.id)
	eventually(t, "canali chiusi dopo la caduta", func() bool { return tr.mgr.Open() == 0 })
	waitHello(t, tr.s)
	waitWelcome(t, tr.r)
	if _, err := tr.tmux("has-session", "-t", "=lunga"); err != nil {
		t.Fatal("la sessione è morta con la connessione")
	}
	if env := tr.open(t, 1, "lunga", false, protocol.ModeInteract); env.Type != protocol.TypeSessionOpened {
		t.Fatalf("riapertura: %s %s", env.Type, env.Body)
	}
	eventually(t, "output prodotto durante la caduta", func() bool { return bytes.Contains(tr.s.Output(tr.id, 1), []byte("dopo-la-42-caduta")) })
}

// Spegnimento del connettore: il backend riceve session.closed «connector-shutdown» per ogni canale.
func TestTerminaleSpegnimento(t *testing.T) {
	tr := newTermRig(t, nil)
	tr.open(t, 3, "a", true, protocol.ModeInteract)
	tr.r.cancel()
	waitDone(t, tr.r)
	cl := tr.s.Closed(tr.id)
	if len(cl) != 1 || cl[0].Channel != 3 || cl[0].Reason != protocol.ClosedConnectorShutdown {
		t.Fatalf("session.closed %+v", cl)
	}
}

// Molto output: arriva tutto, in frame da al massimo 64 KiB, e il backend non chiude per troppi
// messaggi (§1.7, §1.17).
func TestTerminaleMoltoOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("qualche secondo")
	}
	tr := newTermRig(t, nil)
	tr.open(t, 1, "fiume", true, protocol.ModeInteract)
	_ = tr.s.SendInput(tr.id, 1, []byte("seq 1 150000; echo fine-$((1+1))-fiume\r"))
	eventually(t, "tutto l'output", func() bool { return bytes.Contains(tr.s.Output(tr.id, 1), []byte("fine-2-fiume")) })
	for _, n := range tr.s.OutputFrames(tr.id) {
		if n > protocol.FrameHeaderBytes+protocol.MaxOutputChunk {
			t.Fatalf("frame di %d byte, oltre 64 KiB", n)
		}
	}
	if tr.s.Connects() != 1 {
		t.Fatalf("collegamenti %d: il backend ha chiuso per troppi messaggi", tr.s.Connects())
	}
}

// Senza gestore del terminale i messaggi del terminale restano TYPE_UNKNOWN, come prima.
func TestSenzaTerminaleTypeUnknown(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	r := start(t, s, tok, nil)
	waitHello(t, s)
	waitWelcome(t, r)
	mid, err := s.SendControl(id, protocol.TypeSessionsList, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var env protocol.Envelope
	eventually(t, "risposta", func() bool { var ok bool; env, ok = s.Reply(id, mid); return ok })
	if errCode(env) != protocol.CodeTypeUnknown {
		t.Fatalf("risposta %s %s", env.Type, env.Body)
	}
	_ = context.Background()
}

// Schemi 0.6.0 dal capo all'altro (ADR 0409 A1.2): il connettore dichiara 0.6.0 nell'header, e i comandi
// del terminale unico tornano al backend come <tipo>.result per re attraverso la connessione vera.
func TestTerminaleComandiZeroSei(t *testing.T) {
	tr := newTermRig(t, nil)
	if got := tr.s.DeclaredSchemas(); len(got) == 0 || got[len(got)-1] != "0.6.0" {
		t.Fatalf("header X-MonsterTerminal-Schema: %v, atteso 0.6.0", got)
	}
	if _, err := tr.tmux("new-session", "-d", "-s", "uno", "-x", "130", "-y", "35"); err != nil {
		t.Fatal(err)
	}
	id := tr.send(t, protocol.TypeSessionOpen, protocol.SessionOpen{Channel: 1, Session: "uno", Mode: protocol.ModeInteract, Cols: 80, Rows: 24, FitToPane: true})
	env := tr.reply(t, id)
	var op protocol.SessionOpened
	_ = json.Unmarshal(env.Body, &op)
	if env.Type != protocol.TypeSessionOpened || op.Cols != 130 || op.Rows != 36 {
		t.Fatalf("fitToPane: %s %s", env.Type, env.Body)
	}
	expect := func(typ string, body any, want string) protocol.Envelope {
		t.Helper()
		env := tr.reply(t, tr.send(t, typ, body))
		if env.Type != want {
			t.Fatalf("%s: risposta %s %s, attesa %s", typ, env.Type, env.Body, want)
		}
		return env
	}
	expect(protocol.TypeSessionRefresh, protocol.ChannelBody{Channel: 1}, protocol.TypeSessionRefreshResult)
	expect(protocol.TypeSessionInterrupt, protocol.ChannelBody{Channel: 1}, protocol.TypeSessionInterruptResult)
	expect(protocol.TypeSessionBuffer, protocol.ChannelBody{Channel: 1}, protocol.TypeSessionBufferResult)
	expect(protocol.TypeSessionZoom, protocol.SessionZoom{Session: "uno"}, protocol.TypeSessionZoomResult)
	if err := tr.s.SendInput(tr.id, 1, []byte("echo segno-$((6*7))\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "segno nella cattura", func() bool {
		env := tr.reply(t, tr.send(t, protocol.TypeSessionCapture, protocol.SessionCapture{Session: "uno", Range: "screen"}))
		var c protocol.SessionCaptureResult
		_ = json.Unmarshal(env.Body, &c)
		return strings.Contains(c.Text, "segno-42")
	})
	expect(protocol.TypeSessionRename, protocol.SessionRename{Session: "uno", Name: "due"}, protocol.TypeSessionRenameResult)
	expect(protocol.TypeSessionKill, protocol.SessionBody{Session: "due"}, protocol.TypeSessionKillResult)
	eventually(t, "session.closed session-ended al backend", func() bool {
		for _, c := range tr.s.Closed(tr.id) {
			if c.Channel == 1 && c.Reason == protocol.ClosedSessionEnded {
				return true
			}
		}
		return false
	})
	if code := errCode(tr.reply(t, tr.send(t, protocol.TypeSessionKill, protocol.SessionBody{Session: "due"}))); code != protocol.CodeSessionNotFound {
		t.Fatalf("kill ripetuto: %q", code)
	}
}
