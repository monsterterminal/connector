package terminal

// Comandi del terminale unico, schemi 0.6.0 (ADR 0409 A1.2). Ognuno è un verbo tmux fisso su un
// bersaglio esatto («=nome»), col nome già validato dalla regola dei nomi: nessun comando arbitrario
// (§1.6). Sono gli omologhi dei comandi che il backend lancia via SSH (tmux-attach.ts nel monorepo).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/monsterterminal/connector/internal/protocol"
)

// commandTimeout è il tetto di un comando tmux: oltre, il server tmux è da considerare irraggiungibile.
const commandTimeout = 5 * time.Second

// recentBufferMaxAge è l'età massima del paste-buffer che session.buffer restituisce: la stessa del
// percorso SSH (RECENT_BUFFER_MAX_AGE_S): la selezione appena fatta nel terminale, non una vecchia.
const recentBufferMaxAge = 15 * time.Second

// errTmuxUnavailable: l'eseguibile di tmux non si avvia o non risponde in tempo (§1.16).
var errTmuxUnavailable = errors.New("tmux non disponibile")

// run esegue tmux e restituisce lo stdout, al più limit byte (0 = senza tetto); over dice se ce n'era di
// più. Un tmux che non parte o non risponde in tempo è errTmuxUnavailable; un'uscita con errore torna
// come *exec.ExitError con lo stderr nel messaggio.
func (m *Manager) run(limit int, args ...string) (out []byte, over bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	full := args
	if m.cfg.Socket != "" {
		full = append([]string{"-S", m.cfg.Socket}, args...)
	}
	cmd := exec.CommandContext(ctx, m.cfg.Tmux, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	var ee *exec.Error
	var pe *fs.PathError
	switch {
	case ctx.Err() != nil, errors.As(err, &ee), errors.As(err, &pe):
		return nil, false, errTmuxUnavailable
	case err != nil:
		return nil, false, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	out = stdout.Bytes()
	if limit > 0 && len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// sessionGone: la sessione non c'è (o non c'è proprio il server tmux) al momento del comando.
func (m *Manager) sessionGone(re, name string) {
	m.reply(re, protocol.CodeSessionNotFound, "sessione tmux inesistente: "+name)
}

// present verifica che la sessione esista prima di un comando per sessione; se no risponde
// SESSION_NOT_FOUND, o TMUX_UNAVAILABLE se tmux stesso non risponde.
func (m *Manager) present(re, name string) bool {
	_, _, err := m.run(0, "has-session", "-t", "="+name)
	switch {
	case err == nil:
		return true
	case errors.Is(err, errTmuxUnavailable):
		m.reply(re, protocol.CodeTmuxUnavailable, "tmux non risponde su questo host")
	default:
		m.sessionGone(re, name)
	}
	return false
}

// failed risponde all'errore di un comando tmux: TMUX_UNAVAILABLE o, se la sessione è sparita,
// SESSION_NOT_FOUND; altrimenti l'errore di tmux così com'è, sotto MESSAGE_INVALID.
func (m *Manager) failed(re, name string, err error) {
	switch {
	case errors.Is(err, errTmuxUnavailable):
		m.reply(re, protocol.CodeTmuxUnavailable, "tmux non risponde su questo host")
	case !m.hasSession(name):
		m.sessionGone(re, name)
	default:
		m.reply(re, protocol.CodeMessageInvalid, "comando tmux non riuscito: "+err.Error())
	}
}

// paneTarget è la pane attiva della finestra corrente della sessione, col nome esatto.
func paneTarget(name string) string { return "=" + name + ":" }

// display legge un formato tmux sulla pane attiva della sessione.
func (m *Manager) display(name, format string) (string, error) {
	out, _, err := m.run(4096, "display-message", "-p", "-t", paneTarget(name), format)
	return strings.TrimSpace(string(out)), err
}

// channelFor trova il canale di un comando per canale, o risponde CHANNEL_UNKNOWN.
func (m *Manager) channelFor(env protocol.Envelope) (*channel, bool) {
	b, err := protocol.DecodeChannelBody(env.Type, env.Body)
	if err != nil {
		m.reply(env.ID, protocol.CodeMessageInvalid, err.Error())
		return nil, false
	}
	m.mu.Lock()
	c := m.channels[b.Channel]
	m.mu.Unlock()
	if c == nil {
		m.reply(env.ID, protocol.CodeChannelUnknown, fmt.Sprintf("canale %d non aperto", b.Channel))
		return nil, false
	}
	return c, true
}

// sessionOf legge il nome corrente della sessione di un canale (session.rename lo cambia).
func (m *Manager) sessionOf(c *channel) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return c.session
}

// handleRefresh ridisegna il solo client tmux del canale (refresh-client sul suo tty): gli altri client
// della sessione, compresi i terminali dell'utente sull'host, non si toccano. L'output del ridisegno
// arriva sul frame 0x01 come ogni altro output.
func (m *Manager) handleRefresh(env protocol.Envelope) {
	c, ok := m.channelFor(env)
	if !ok {
		return
	}
	name := m.sessionOf(c)
	// Il client tmux appena agganciato compare in list-clients dopo qualche istante: si riprova per un
	// secondo prima di dire che il canale non ha un client.
	tty := ""
	for attempt := 0; attempt < 10 && tty == ""; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		out, _, err := m.run(64<<10, "list-clients", "-t", "="+name, "-F", "#{client_pid}\t#{client_tty}")
		if err != nil {
			m.failed(env.ID, name, err)
			return
		}
		if c.cmd.Process == nil {
			break
		}
		pid := strconv.Itoa(c.cmd.Process.Pid)
		for _, line := range strings.Split(string(out), "\n") {
			if p, t, ok := strings.Cut(line, "\t"); ok && p == pid {
				tty = t
			}
		}
	}
	if tty == "" {
		m.reply(env.ID, protocol.CodeChannelUnknown, fmt.Sprintf("canale %d senza client tmux agganciato", c.id))
		return
	}
	if _, _, err := m.run(0, "refresh-client", "-t", tty); err != nil {
		m.failed(env.ID, name, err)
		return
	}
	m.send(protocol.TypeSessionRefreshResult, env.ID, protocol.ChannelBody{Channel: c.id})
}

// handleInterrupt è il Ctrl+C dedicato: esce dalla copy-mode (fuori dalla copy-mode fallisce innocuo) e
// poi manda C-c, che così raggiunge sempre il processo (stessa sequenza del percorso SSH). Un canale in
// sola lettura non interrompe nulla: seconda barriera dopo quella del backend, come per l'input.
func (m *Manager) handleInterrupt(env protocol.Envelope) {
	c, ok := m.channelFor(env)
	if !ok {
		return
	}
	if c.view {
		m.reply(env.ID, protocol.CodeMessageInvalid, fmt.Sprintf("canale %d in sola lettura", c.id))
		return
	}
	name := m.sessionOf(c)
	_, _, _ = m.run(0, "send-keys", "-t", paneTarget(name), "-X", "cancel")
	if _, _, err := m.run(0, "send-keys", "-t", paneTarget(name), "C-c"); err != nil {
		m.failed(env.ID, name, err)
		return
	}
	m.send(protocol.TypeSessionInterruptResult, env.ID, protocol.ChannelBody{Channel: c.id})
}

// handleBuffer restituisce l'ultimo paste-buffer di tmux se è stato creato da al più 15 secondi (la
// selezione appena fatta nel terminale), altrimenti null. Il testo si tronca a 64 KiB di UTF-8 valido.
func (m *Manager) handleBuffer(env protocol.Envelope) {
	c, ok := m.channelFor(env)
	if !ok {
		return
	}
	res := protocol.SessionBufferResult{Channel: c.id}
	out, _, err := m.run(4096, "list-buffers", "-F", "#{buffer_created}")
	if errors.Is(err, errTmuxUnavailable) {
		m.failed(env.ID, m.sessionOf(c), err)
		return
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	created, perr := strconv.ParseInt(first, 10, 64)
	if err == nil && perr == nil && time.Since(time.Unix(created, 0)) <= recentBufferMaxAge {
		raw, _, berr := m.run(protocol.MaxBufferBytes, "show-buffer")
		if berr == nil {
			text := truncateUTF8(strings.ToValidUTF8(string(raw), ""), protocol.MaxBufferBytes)
			res.Text = &text
		}
	}
	m.send(protocol.TypeSessionBufferResult, env.ID, res)
}

// truncateUTF8 taglia s ad al più n byte senza spezzare un carattere.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// decodeFailed risponde agli errori di decodifica comuni dei comandi per sessione.
func (m *Manager) decodeFailed(re string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, protocol.ErrSessionName):
		m.reply(re, protocol.CodeSessionNameInvalid, "nome di sessione fuori dalla regola dei nomi tmux")
	default:
		m.reply(re, protocol.CodeMessageInvalid, err.Error())
	}
	return true
}

// handleKill chiude la sessione tmux. I canali agganciati se ne accorgono da soli: il loro client tmux
// esce e pump manda session.closed «session-ended» (closedReason).
func (m *Manager) handleKill(env protocol.Envelope) {
	k, err := protocol.DecodeSessionKill(env.Body)
	if m.decodeFailed(env.ID, err) {
		return
	}
	if !m.present(env.ID, k.Session) {
		return
	}
	if _, _, err := m.run(0, "kill-session", "-t", "="+k.Session); err != nil {
		m.failed(env.ID, k.Session, err)
		return
	}
	m.send(protocol.TypeSessionKillResult, env.ID, protocol.SessionBody{Session: k.Session})
}

// handleRename rinomina la sessione; un nome già in uso è SESSION_EXISTS. I canali agganciati restano
// aperti e prendono il nome nuovo.
func (m *Manager) handleRename(env protocol.Envelope) {
	r, err := protocol.DecodeSessionRename(env.Body)
	if m.decodeFailed(env.ID, err) {
		return
	}
	if !m.present(env.ID, r.Session) {
		return
	}
	if r.Name != r.Session {
		if m.hasSession(r.Name) {
			m.reply(env.ID, protocol.CodeSessionExists, "esiste già una sessione tmux con il nome "+r.Name)
			return
		}
		if _, _, err := m.run(0, "rename-session", "-t", "="+r.Session, r.Name); err != nil {
			m.failed(env.ID, r.Session, err)
			return
		}
		m.mu.Lock()
		for _, c := range m.channels {
			if c.session == r.Session {
				c.session = r.Name
			}
		}
		m.mu.Unlock()
	}
	m.send(protocol.TypeSessionRenameResult, env.ID, protocol.SessionRename{Session: r.Session, Name: r.Name})
}

// zoomState legge il numero di pane della finestra corrente e se è zoomata.
func (m *Manager) zoomState(name string) (panes int, zoomed bool, err error) {
	s, err := m.display(name, "#{window_panes} #{window_zoomed_flag}")
	if err != nil {
		return 0, false, err
	}
	p, z, _ := strings.Cut(s, " ")
	panes, _ = strconv.Atoi(p)
	return panes, z == "1", nil
}

// handleZoom imposta lo zoom della pane attiva quando on c'è (due richieste uguali danno lo stesso
// stato), lo inverte quando manca (il toggle della rotta SSH omologa). Con un solo pane resize-pane -Z non ha effetto (ADR 0382 D8): niente errore, on false e
// panes 1, e il backend lo dice all'utente.
func (m *Manager) handleZoom(env protocol.Envelope) {
	z, err := protocol.DecodeSessionZoom(env.Body)
	if m.decodeFailed(env.ID, err) {
		return
	}
	if !m.present(env.ID, z.Session) {
		return
	}
	panes, zoomed, err := m.zoomState(z.Session)
	if err != nil {
		m.failed(env.ID, z.Session, err)
		return
	}
	if panes != 1 && (z.On == nil || zoomed != *z.On) {
		if _, _, err := m.run(0, "resize-pane", "-Z", "-t", paneTarget(z.Session)); err != nil {
			m.failed(env.ID, z.Session, err)
			return
		}
		if panes, zoomed, err = m.zoomState(z.Session); err != nil {
			m.failed(env.ID, z.Session, err)
			return
		}
	}
	m.send(protocol.TypeSessionZoomResult, env.ID, protocol.SessionZoomResult{Session: z.Session, On: zoomed, Panes: panes})
}

// captureArgs costruisce capture-pane per un range (spec 65 §9): screen = schermo vivo, last200 e
// last1000 = con le ultime righe di storia, view = ciò che l'utente guarda, anche scorrendo in copy-mode.
func (m *Manager) captureArgs(name, rng string) ([]string, error) {
	args := []string{"capture-pane", "-p", "-J", "-t", paneTarget(name)}
	switch rng {
	case "last200":
		args = append(args, "-S", "-200")
	case "last1000":
		args = append(args, "-S", "-1000")
	case "view":
		s, err := m.display(name, "#{scroll_position},#{pane_height}")
		if err != nil {
			return nil, err
		}
		ps, hs, _ := strings.Cut(s, ",")
		p, _ := strconv.Atoi(ps) // vuoto fuori dalla copy-mode: 0
		h, herr := strconv.Atoi(hs)
		if herr != nil || h < 1 {
			return nil, fmt.Errorf("altezza della pane illeggibile: %q", s)
		}
		args = append(args, "-S", strconv.Itoa(-p), "-E", strconv.Itoa(h-1-p))
	}
	return args, nil
}

// handleCapture restituisce il testo della pane attiva; oltre 256 KiB di UTF-8 (o un messaggio che non
// starebbe in un frame) è CAPTURE_TOO_LARGE, e il backend propone un range più corto.
func (m *Manager) handleCapture(env protocol.Envelope) {
	c, err := protocol.DecodeSessionCapture(env.Body)
	if m.decodeFailed(env.ID, err) {
		return
	}
	if !m.present(env.ID, c.Session) {
		return
	}
	args, err := m.captureArgs(c.Session, c.Range)
	if err != nil {
		m.failed(env.ID, c.Session, err)
		return
	}
	raw, over, err := m.run(protocol.MaxCaptureBytes, args...)
	if err != nil {
		m.failed(env.ID, c.Session, err)
		return
	}
	text := strings.ToValidUTF8(string(raw), "�")
	tooLarge := over || len(text) > protocol.MaxCaptureBytes
	if !tooLarge {
		enc, _ := json.Marshal(text)
		tooLarge = len(enc) > protocol.MaxFrameBytes-4096
	}
	if tooLarge {
		m.reply(env.ID, protocol.CodeCaptureTooLarge, fmt.Sprintf("il testo del range %s supera %d KiB: scegli un range più corto", c.Range, protocol.MaxCaptureBytes>>10))
		return
	}
	m.send(protocol.TypeSessionCaptureResult, env.ID, protocol.SessionCaptureResult{Session: c.Session, Text: text})
}

// fitSize legge le dimensioni della finestra corrente della sessione per fitToPane: colonne della
// finestra e righe della finestra più la barra di stato, così il client che si aggancia non la
// ridimensiona. Con un solo pane (o zoomato) la finestra è la pane.
func (m *Manager) fitSize(name string) (cols, rows int, err error) {
	s, err := m.display(name, "#{window_width} #{window_height} #{status}")
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(s)
	if len(f) != 3 {
		return 0, 0, fmt.Errorf("dimensioni della finestra illeggibili: %q", s)
	}
	cols, err1 := strconv.Atoi(f[0])
	rows, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("dimensioni della finestra illeggibili: %q", s)
	}
	switch f[2] {
	case "off":
	case "on":
		rows++
	default:
		if n, err := strconv.Atoi(f[2]); err == nil && n > 0 && n <= 5 {
			rows += n
		}
	}
	return min(max(cols, 1), protocol.MaxTerminalSize), min(max(rows, 1), protocol.MaxTerminalSize), nil
}
