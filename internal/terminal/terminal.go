// Package terminal tiene i canali del terminale sulle sessioni tmux dell'host (spec
// monsterterminal-saas.md §1.6, §1.7; ADR 0409 D4).
//
// Un canale è un `tmux attach` in uno pseudo-terminale: l'output del pty va al backend in frame
// binari, l'input del backend va nel pty. Chiudere un canale stacca il client tmux e basta: la
// sessione resta viva sull'host, con gli agenti che ci lavorano dentro. Con mode "view" l'attach è in
// sola lettura (`tmux attach -r`) e l'input di quel canale si scarta QUI, seconda barriera dopo quella
// del backend.
//
// Il protocollo non esegue comandi sull'host (§1.6): gli unici comandi tmux lanciati sono quelli di
// questo file e di commands.go (schemi 0.6.0), con argomenti fissi e il nome di sessione già validato
// dalla regola dei nomi.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/monsterterminal/connector/internal/protocol"
)

// Out è la connessione vista dai canali: messaggi di controllo e output in frame binari.
type Out interface {
	// Control invia un messaggio di controllo; re è facoltativo.
	Control(typ, re string, body any) error
	// Output accoda output del canale; si blocca quando la coda supera il tetto (contropressione §1.7).
	Output(channel uint32, data []byte) error
	// Discard butta l'output ancora in coda di un canale chiuso.
	Discard(channel uint32)
}

// Config configura il gestore.
type Config struct {
	// Tmux è l'eseguibile di tmux; vuoto = "tmux" dal PATH.
	Tmux string
	// Socket, se non vuoto, è il socket del server tmux (-S). Vuoto = quello di default dell'utente.
	Socket string
	// DefaultMaxChannels vale finché welcome non porta maxChannels (§1.11).
	DefaultMaxChannels int
	Logger             *slog.Logger
	// ReadBuffer è la dimensione della lettura dal pty; zero = 32 KiB.
	ReadBuffer int
}

type channel struct {
	id       uint32
	session  string
	view     bool
	cmd      *exec.Cmd
	pty      *os.File
	closing  bool // chiuso su richiesta del backend o per la caduta: nessun session.closed
	finished chan struct{}
}

// Manager tiene i canali della connessione corrente.
type Manager struct {
	cfg Config

	mu          sync.Mutex
	out         Out
	channels    map[uint32]*channel
	maxChannels int
	unknownLog  map[uint32]time.Time
}

// New crea il gestore.
func New(cfg Config) *Manager {
	if cfg.Tmux == "" {
		cfg.Tmux = "tmux"
	}
	if cfg.DefaultMaxChannels <= 0 {
		cfg.DefaultMaxChannels = 12
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.ReadBuffer <= 0 {
		cfg.ReadBuffer = 32 << 10
	}
	return &Manager{cfg: cfg, channels: map[uint32]*channel{}, maxChannels: cfg.DefaultMaxChannels, unknownLog: map[uint32]time.Time{}}
}

// SetMaxChannels applica maxChannels della configurazione dell'host (§1.11). I canali già aperti
// restano: il tetto vale per le aperture successive.
func (m *Manager) SetMaxChannels(n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	m.maxChannels = n
	m.mu.Unlock()
}

// Attach collega il gestore a una connessione nuova.
func (m *Manager) Attach(out Out) {
	m.mu.Lock()
	m.out = out
	m.mu.Unlock()
}

// Detach chiude tutti i canali alla fine della connessione: le sessioni tmux restano vive (§1.7,
// ADR 0409 D4). Con shutdown il connettore si sta fermando e prova ad avvisare il backend con
// session.closed «connector-shutdown»; se la connessione è caduta non c'è nessuno da avvisare.
func (m *Manager) Detach(shutdown bool) {
	m.mu.Lock()
	out := m.out
	chs := make([]*channel, 0, len(m.channels))
	for _, c := range m.channels {
		c.closing = true
		chs = append(chs, c)
	}
	m.channels = map[uint32]*channel{}
	m.out = nil
	m.mu.Unlock()
	for _, c := range chs {
		if shutdown && out != nil {
			_ = out.Control(protocol.TypeSessionClosed, "", protocol.SessionClosed{Channel: c.id, Reason: protocol.ClosedConnectorShutdown})
		}
		stop(c)
	}
}

// Open restituisce il numero di canali aperti (per i test e i log).
func (m *Manager) Open() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.channels)
}

// Handle elabora un messaggio di controllo del terminale; false se il tipo non è suo.
func (m *Manager) Handle(env protocol.Envelope) bool {
	switch env.Type {
	case protocol.TypeSessionOpen:
		m.handleOpen(env)
	case protocol.TypeSessionResize:
		r, err := protocol.DecodeSessionResize(env.Body)
		if err != nil {
			m.reply(env.ID, protocol.CodeMessageInvalid, err.Error())
			return true
		}
		m.mu.Lock()
		c := m.channels[r.Channel]
		m.mu.Unlock()
		if c == nil {
			m.reply(env.ID, protocol.CodeChannelUnknown, fmt.Sprintf("canale %d non aperto", r.Channel))
			return true
		}
		_ = pty.Setsize(c.pty, &pty.Winsize{Cols: uint16(r.Cols), Rows: uint16(r.Rows)})
	case protocol.TypeSessionClose:
		cl, err := protocol.DecodeSessionClose(env.Body)
		if err != nil {
			m.reply(env.ID, protocol.CodeMessageInvalid, err.Error())
			return true
		}
		// Chiudere un canale già chiuso non è un errore: la chiusura può incrociarsi con session.closed.
		m.closeChannel(cl.Channel)
	case protocol.TypeSessionsList:
		if err := protocol.DecodeSessionsList(env.Body); err != nil {
			m.reply(env.ID, protocol.CodeMessageInvalid, err.Error())
			return true
		}
		m.send(protocol.TypeSessionsListResult, env.ID, protocol.SessionsListResult{Sessions: m.listSessions()})
	case protocol.TypeSessionRefresh:
		m.handleRefresh(env)
	case protocol.TypeSessionInterrupt:
		m.handleInterrupt(env)
	case protocol.TypeSessionBuffer:
		m.handleBuffer(env)
	case protocol.TypeSessionKill:
		m.handleKill(env)
	case protocol.TypeSessionRename:
		m.handleRename(env)
	case protocol.TypeSessionZoom:
		m.handleZoom(env)
	case protocol.TypeSessionCapture:
		m.handleCapture(env)
	default:
		return false
	}
	return true
}

// Input consegna l'input dell'utente al canale (frame 0x02). Un canale sconosciuto si scarta (§1.7);
// in sola lettura l'input si scarta sempre.
func (m *Manager) Input(ch uint32, data []byte) {
	m.mu.Lock()
	c := m.channels[ch]
	if c == nil {
		// §1.7 chiede un error CHANNEL_UNKNOWN al più una volta al secondo, ma l'error del connettore
		// richiede `re` (schema 0.1.0) e un frame binario non ha id: si registra soltanto, con lo
		// stesso ritmo.
		now := time.Now()
		if now.Sub(m.unknownLog[ch]) >= time.Second {
			m.unknownLog[ch] = now
			m.cfg.Logger.Warn("input per un canale non aperto, scartato", "canale", ch)
		}
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	if c.view || len(data) == 0 {
		return
	}
	_, _ = c.pty.Write(data)
}

func (m *Manager) send(typ, re string, body any) {
	m.mu.Lock()
	out := m.out
	m.mu.Unlock()
	if out != nil {
		_ = out.Control(typ, re, body)
	}
}

func (m *Manager) reply(re, code, msg string) {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	m.send(protocol.TypeError, re, protocol.ErrorBody{Code: code, Message: msg})
}

func (m *Manager) tmux(args ...string) *exec.Cmd {
	full := args
	if m.cfg.Socket != "" {
		full = append([]string{"-S", m.cfg.Socket}, args...)
	}
	cmd := exec.Command(m.cfg.Tmux, full...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	return cmd
}

// target è il bersaglio esatto: «=nome» impedisce a tmux di agganciare per prefisso un'altra
// sessione («lav» non deve aprire «lavoro»).
func target(name string) string { return "=" + name }

func (m *Manager) hasSession(name string) bool {
	return m.tmux("has-session", "-t", target(name)).Run() == nil
}

func (m *Manager) handleOpen(env protocol.Envelope) {
	o, err := protocol.DecodeSessionOpen(env.Body)
	switch {
	case errors.Is(err, protocol.ErrSessionName):
		m.reply(env.ID, protocol.CodeSessionNameInvalid, "nome di sessione fuori dalla regola dei nomi tmux")
		return
	case err != nil:
		m.reply(env.ID, protocol.CodeMessageInvalid, err.Error())
		return
	}
	m.mu.Lock()
	_, dup := m.channels[o.Channel]
	full := len(m.channels) >= m.maxChannels
	max := m.maxChannels
	m.mu.Unlock()
	switch {
	case dup:
		m.reply(env.ID, protocol.CodeMessageInvalid, fmt.Sprintf("canale %d già aperto", o.Channel))
		return
	case full:
		m.reply(env.ID, protocol.CodeChannelLimit, fmt.Sprintf("già %d canali aperti su questo host", max))
		return
	}
	if !m.hasSession(o.Session) {
		if !o.Create {
			m.reply(env.ID, protocol.CodeSessionNotFound, "sessione tmux inesistente: "+o.Session)
			return
		}
		cmd := m.tmux("new-session", "-d", "-s", o.Session, "-x", strconv.Itoa(o.Cols), "-y", strconv.Itoa(o.Rows))
		if out, err := cmd.CombinedOutput(); err != nil && !m.hasSession(o.Session) {
			m.cfg.Logger.Warn("sessione tmux non creata", "errore", err.Error(), "tmux", strings.TrimSpace(string(out)))
			m.reply(env.ID, protocol.CodeSessionNotFound, "sessione tmux non creata: "+strings.TrimSpace(string(out)))
			return
		}
	}
	cols, rows := o.Cols, o.Rows
	if o.FitToPane {
		// Specchio largo (schemi 0.6.0): il canale nasce con le dimensioni della finestra tmux, e
		// session.opened le dichiara. Se non si leggono si resta su quelle del client.
		if fc, fr, err := m.fitSize(o.Session); err == nil {
			cols, rows = fc, fr
		} else {
			m.cfg.Logger.Warn("fitToPane: dimensioni della finestra non lette, uso quelle del client", "errore", err.Error())
		}
	}
	args := []string{"attach-session"}
	if o.Mode == protocol.ModeView {
		args = append(args, "-r")
	}
	args = append(args, "-t", target(o.Session))
	cmd := m.tmux(args...)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		m.reply(env.ID, protocol.CodeSessionNotFound, "tmux non avviabile: "+err.Error())
		return
	}
	c := &channel{id: o.Channel, session: o.Session, view: o.Mode == protocol.ModeView, cmd: cmd, pty: f, finished: make(chan struct{})}
	m.mu.Lock()
	out := m.out
	m.channels[o.Channel] = c
	delete(m.unknownLog, o.Channel)
	m.mu.Unlock()
	m.send(protocol.TypeSessionOpened, env.ID, protocol.SessionOpened{Channel: o.Channel, Cols: cols, Rows: rows})
	go m.pump(c, out)
}

// pump porta l'output del pty al backend finché il client tmux non esce.
func (m *Manager) pump(c *channel, out Out) {
	defer close(c.finished)
	buf := make([]byte, m.cfg.ReadBuffer)
	for {
		n, err := c.pty.Read(buf)
		if n > 0 && out != nil {
			chunk := append([]byte(nil), buf[:n]...)
			if oerr := out.Output(c.id, chunk); oerr != nil {
				break // connessione finita: ci pensa Detach
			}
		}
		if err != nil {
			break
		}
	}
	werr := c.cmd.Wait()
	_ = c.pty.Close()
	m.mu.Lock()
	closing := c.closing
	if m.channels[c.id] == c {
		delete(m.channels, c.id)
	}
	cur := m.out
	m.mu.Unlock()
	if closing || cur == nil {
		return
	}
	cur.Discard(c.id)
	m.send(protocol.TypeSessionClosed, "", protocol.SessionClosed{Channel: c.id, Reason: m.closedReason(c, werr)})
}

// closedReason: il client tmux è uscito da solo, senza session.close del backend.
//   - la sessione non c'è più e il client è uscito con un codice: «session-ended» (exit, kill-session);
//   - la sessione non c'è più e il client non è nemmeno uscito con un codice (segnale, server
//     sparito): «tmux-gone»;
//   - la sessione c'è ancora: il client è stato staccato o è uscito con errore, «error».
func (m *Manager) closedReason(c *channel, werr error) string {
	var ee *exec.ExitError
	exited := werr == nil || (errors.As(werr, &ee) && ee.Exited())
	switch {
	case m.hasSession(m.sessionOf(c)):
		return protocol.ClosedError
	case exited:
		return protocol.ClosedSessionEnded
	default:
		return protocol.ClosedTmuxGone
	}
}

func (m *Manager) closeChannel(id uint32) {
	m.mu.Lock()
	c := m.channels[id]
	if c != nil {
		c.closing = true
		delete(m.channels, id)
	}
	out := m.out
	m.mu.Unlock()
	if c == nil {
		return
	}
	if out != nil {
		out.Discard(id)
	}
	stop(c)
}

// stop stacca il client tmux: SIGHUP come alla chiusura di un terminale; la sessione resta viva.
func stop(c *channel) {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGHUP)
	}
	select {
	case <-c.finished:
	case <-time.After(2 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_ = c.pty.Close()
	}
}

// listSessions elenca le sessioni tmux dell'host (sessions.list.result). Senza server tmux l'elenco è
// vuoto; i nomi fuori dalla regola non si elencano (non sarebbero apribili, e lo schema li rifiuta).
func (m *Manager) listSessions() []protocol.SessionInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := []string{"list-sessions", "-F", "#{session_name}\t#{session_created}\t#{session_attached}\t#{session_windows}"}
	if m.cfg.Socket != "" {
		args = append([]string{"-S", m.cfg.Socket}, args...)
	}
	raw, err := exec.CommandContext(ctx, m.cfg.Tmux, args...).Output()
	out := []protocol.SessionInfo{}
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || !protocol.IsSessionName(f[0]) {
			continue
		}
		created, err1 := strconv.ParseInt(f[1], 10, 64)
		attached, err2 := strconv.Atoi(f[2])
		windows, err3 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil || err3 != nil || windows < 1 {
			continue
		}
		out = append(out, protocol.SessionInfo{
			Name: f[0], CreatedAt: protocol.Timestamp(time.Unix(created, 0)), Attached: attached > 0, Windows: windows,
		})
		if len(out) == protocol.MaxSessionsListed {
			break
		}
	}
	return out
}
