// fitRows "client" su un tmux vero (schemi 0.9.0, ADR 0409 A1.6; rilievo BOSS 2026-10-31, decisione
// BOSS 2026-11-01): un «computer» e un «telefono» sulla stessa sessione. Il telefono ha le sue righe e
// le colonne della finestra; la finestra non cambia per il computer, né all'aggancio né quando il
// telefono scrive, ruota o chiede colonne diverse. Rimasto solo, il telefono fa seguire la finestra.
package terminal

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

// client restituisce «LxA flag» del client tmux del canale.
func (r *rig) client(ch uint32) string {
	r.t.Helper()
	r.m.mu.Lock()
	c := r.m.channels[ch]
	r.m.mu.Unlock()
	if c == nil || c.cmd.Process == nil {
		r.t.Fatalf("canale %d senza client", ch)
	}
	pid := strconv.Itoa(c.cmd.Process.Pid)
	var got string
	eventually(r.t, "client tmux del canale", func() bool {
		out, _ := r.tmux("list-clients", "-F", "#{client_pid} #{client_width}x#{client_height} #{client_flags}")
		for _, l := range strings.Split(out, "\n") {
			if p, rest, ok := strings.Cut(l, " "); ok && p == pid {
				got = rest
				return true
			}
		}
		return false
	})
	return got
}

func (r *rig) window(name string) string {
	r.t.Helper()
	s, _ := r.tmux("display-message", "-p", "-t", "="+name+":", "#{window_width}x#{window_height}")
	return s
}

func (r *rig) phone(ch uint32, session string, rows int) protocol.SessionOpened {
	r.t.Helper()
	id := r.send(protocol.TypeSessionOpen, protocol.SessionOpen{Channel: ch, Session: session, Mode: protocol.ModeInteract,
		Cols: 40, Rows: rows, FitToPane: true, FitRows: protocol.FitRowsClient})
	var op protocol.SessionOpened
	r.result(protocol.TypeSessionOpened, id, &op)
	return op
}

func TestFitRowsRigheDelTelefonoFinestraDelComputerInvariata(t *testing.T) {
	r := newRig(t)
	if !r.m.ignoreSize() {
		t.Skip("tmux senza ignore-size (< 3.2)")
	}
	r.newSession("ssh2", "-x", "150", "-y", "40")
	// Il computer: un client normale grande quanto la finestra più la barra.
	pc := r.send(protocol.TypeSessionOpen, protocol.SessionOpen{Channel: 1, Session: "ssh2", Mode: protocol.ModeInteract, Cols: 150, Rows: 41})
	r.result(protocol.TypeSessionOpened, pc, &protocol.SessionOpened{})

	op := r.phone(2, "ssh2", 20)
	if op.Cols != 150 || op.Rows != 20 || op.FitRows != protocol.FitRowsClient {
		t.Fatalf("session.opened %+v, atteso 150x20 con fitRows client", op)
	}
	if c := r.client(2); !strings.HasPrefix(c, "150x20 ") || !strings.Contains(c, "ignore-size") {
		t.Fatalf("client del telefono %q: atteso 150x20 in ignore-size", c)
	}
	// Il telefono scrive (diventa il client più recente): la finestra resta del computer.
	r.m.Input(2, []byte("echo dal-telefono\r"))
	time.Sleep(400 * time.Millisecond)
	if w := r.window("ssh2"); w != "150x40" {
		t.Fatalf("finestra %s dopo l'input del telefono, attesa 150x40", w)
	}
	// Rotazione a 12 righe, con colonne diverse: le colonne non contano.
	r.send(protocol.TypeSessionResize, protocol.SessionResize{Channel: 2, Cols: 60, Rows: 12})
	eventually(t, "telefono a 12 righe", func() bool { return strings.HasPrefix(r.client(2), "150x12 ") })
	r.m.Input(2, []byte("echo ruotato\r"))
	time.Sleep(400 * time.Millisecond)
	if w := r.window("ssh2"); w != "150x40" {
		t.Fatalf("finestra %s dopo la rotazione, attesa 150x40", w)
	}
	// Il computer stringe la finestra: al suo prossimo resize il telefono riprende la larghezza nuova.
	r.send(protocol.TypeSessionResize, protocol.SessionResize{Channel: 1, Cols: 120, Rows: 41})
	eventually(t, "finestra del computer a 120", func() bool { return r.window("ssh2") == "120x40" })
	r.send(protocol.TypeSessionResize, protocol.SessionResize{Channel: 2, Cols: 60, Rows: 15})
	eventually(t, "telefono a 120x15", func() bool { return strings.HasPrefix(r.client(2), "120x15 ") })
	// Il ridisegno riprende anche la larghezza: il computer allarga, il telefono fa refresh.
	r.send(protocol.TypeSessionResize, protocol.SessionResize{Channel: 1, Cols: 140, Rows: 41})
	eventually(t, "finestra del computer a 140", func() bool { return r.window("ssh2") == "140x40" })
	r.result(protocol.TypeSessionRefreshResult, r.send(protocol.TypeSessionRefresh, protocol.ChannelBody{Channel: 2}), &protocol.ChannelBody{})
	eventually(t, "telefono a 140x15 dopo il ridisegno", func() bool { return strings.HasPrefix(r.client(2), "140x15 ") })

	// Il computer si stacca: la finestra segue il telefono, rimasto solo.
	r.send(protocol.TypeSessionClose, protocol.SessionClose{Channel: 1})
	eventually(t, "finestra che segue il telefono", func() bool { return r.window("ssh2") == "140x14" })
}

// Telefono più alto della finestra (decisione BOSS 2026-11-01): il computer non cede, la finestra resta.
func TestFitRowsTelefonoPiuAltoNonCambiaLaFinestra(t *testing.T) {
	r := newRig(t)
	if !r.m.ignoreSize() {
		t.Skip("tmux senza ignore-size (< 3.2)")
	}
	r.newSession("basso", "-x", "120", "-y", "20")
	pc := r.send(protocol.TypeSessionOpen, protocol.SessionOpen{Channel: 1, Session: "basso", Mode: protocol.ModeInteract, Cols: 120, Rows: 21})
	r.result(protocol.TypeSessionOpened, pc, &protocol.SessionOpened{})
	if op := r.phone(2, "basso", 40); op.Rows != 40 || op.FitRows != protocol.FitRowsClient {
		t.Fatalf("session.opened %+v", op)
	}
	r.m.Input(2, []byte("echo alto\r"))
	time.Sleep(400 * time.Millisecond)
	if w := r.window("basso"); w != "120x20" {
		t.Fatalf("finestra %s, attesa 120x20: il computer non cede", w)
	}
}

// tmux < 3.2: ripiego sullo specchio largo, dichiarato (session.opened senza fitRows).
func TestFitRowsRipiegoSenzaIgnoreSize(t *testing.T) {
	r := newRig(t)
	r.m.cfg.NoIgnoreSize = true
	r.newSession("vecchio", "-x", "150", "-y", "40")
	op := r.phone(1, "vecchio", 20)
	if op.Cols != 150 || op.Rows != 41 || op.FitRows != "" {
		t.Fatalf("ripiego: session.opened %+v, atteso 150x41 senza fitRows", op)
	}
	if c := r.client(1); strings.Contains(c, "ignore-size") {
		t.Fatalf("ripiego con ignore-size: %q", c)
	}
}

// Contratto (schemi 0.9.0): fitRows vale solo "client", con fitToPane e almeno 5 righe.
func TestFitRowsFuoriRegola(t *testing.T) {
	r := newRig(t)
	r.newSession("regole", "-x", "100", "-y", "30")
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"senza fitToPane", map[string]any{"fitRows": "client", "rows": 20}},
		{"valore sconosciuto", map[string]any{"fitToPane": true, "fitRows": "pane", "rows": 20}},
		{"meno di 5 righe", map[string]any{"fitToPane": true, "fitRows": "client", "rows": 4}},
	} {
		body := map[string]any{"channel": 9, "session": "regole", "create": false, "mode": "view", "cols": 80}
		for k, v := range tc.body {
			body[k] = v
		}
		raw, _ := json.Marshal(body)
		id := protocol.NewUUID()
		r.m.Handle(protocol.Envelope{Type: protocol.TypeSessionOpen, ID: id, Body: raw})
		if code := r.errorCode(id); code != protocol.CodeMessageInvalid {
			t.Errorf("%s: codice %q, atteso MESSAGE_INVALID", tc.name, code)
		}
	}
	if r.m.Open() != 0 {
		t.Fatal("un canale fuori regola si è aperto")
	}
}

func TestTmuxHasIgnoreSize(t *testing.T) {
	for v, want := range map[string]bool{
		"tmux 3.4\n": true, "tmux 3.2": true, "tmux 3.2a": true, "tmux 3.10": true, "tmux 4.0": true,
		"tmux master": true, "tmux next-3.5": true,
		"tmux 3.1c": false, "tmux 3.1": false, "tmux 2.9a": false, "": false, "boh 3.4": false, "tmux x.y": false,
	} {
		if got := TmuxHasIgnoreSize(v); got != want {
			t.Errorf("%q: %v, atteso %v", v, got, want)
		}
	}
}
