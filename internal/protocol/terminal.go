package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// Messaggi del terminale (spec monsterterminal-saas.md §1.6, §1.7).
const (
	TypeSessionsList       = "sessions.list"
	TypeSessionsListResult = "sessions.list.result"
	TypeSessionOpen        = "session.open"
	TypeSessionOpened      = "session.opened"
	TypeSessionResize      = "session.resize"
	TypeSessionClose       = "session.close"
	TypeSessionClosed      = "session.closed"

	// Comandi del terminale unico, schemi 0.6.0 (ADR 0409 A1.2): ognuno ha la risposta <tipo>.result
	// per re. Per canale: refresh, interrupt, buffer; per sessione: kill, rename, zoom, capture.
	TypeSessionRefresh         = "session.refresh"
	TypeSessionRefreshResult   = "session.refresh.result"
	TypeSessionInterrupt       = "session.interrupt"
	TypeSessionInterruptResult = "session.interrupt.result"
	TypeSessionBuffer          = "session.buffer"
	TypeSessionBufferResult    = "session.buffer.result"
	TypeSessionKill            = "session.kill"
	TypeSessionKillResult      = "session.kill.result"
	TypeSessionRename          = "session.rename"
	TypeSessionRenameResult    = "session.rename.result"
	TypeSessionZoom            = "session.zoom"
	TypeSessionZoomResult      = "session.zoom.result"
	TypeSessionCapture         = "session.capture"
	TypeSessionCaptureResult   = "session.capture.result"
)

// Codici di errore del terminale (§1.16).
const (
	CodeChannelUnknown     = "CHANNEL_UNKNOWN"
	CodeSessionNotFound    = "SESSION_NOT_FOUND"
	CodeSessionNameInvalid = "SESSION_NAME_INVALID"
	CodeChannelLimit       = "CHANNEL_LIMIT"
	// Dagli schemi 0.6.0 (ADR 0409 A1.2): i codici di error non sono un enum negli schemi.
	CodeCaptureTooLarge = "CAPTURE_TOO_LARGE"
	CodeSessionExists   = "SESSION_EXISTS"
	CodeTmuxUnavailable = "TMUX_UNAVAILABLE"
)

// Tetti dei comandi 0.6.0 (ADR 0409 A1.2), in byte UTF-8 del testo.
const (
	MaxBufferBytes  = 64 << 10
	MaxCaptureBytes = 256 << 10
)

// CaptureRanges è l'enum di session.capture: la forma di parseCaptureRange del backend (spec 65 §9).
var CaptureRanges = []string{"screen", "view", "last200", "last1000"}

// Modalità di un canale (§1.7).
const (
	ModeView     = "view"
	ModeInteract = "interact"
)

// Motivi di session.closed (§1.6).
const (
	ClosedSessionEnded      = "session-ended"
	ClosedTmuxGone          = "tmux-gone"
	ClosedConnectorShutdown = "connector-shutdown"
	ClosedError             = "error"
)

// ClosedReasons è l'enum di session.closed, verificato contro lo schema.
var ClosedReasons = []string{ClosedSessionEnded, ClosedTmuxGone, ClosedConnectorShutdown, ClosedError}

// Tipi dei frame binari (§1.7).
const (
	FrameOutput byte = 0x01 // output del terminale, connettore → backend
	FrameInput  byte = 0x02 // input dell'utente, backend → connettore
)

// FrameHeaderBytes è l'intestazione di un frame binario: tipo + canale big-endian.
const FrameHeaderBytes = 5

// MaxOutputChunk è il massimo di byte del terminale in un frame di output (§1.7).
const MaxOutputChunk = 64 << 10

// MaxTerminalSize è il massimo di colonne e righe (schemi session.*).
const MaxTerminalSize = 1000

var sessionNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// IsSessionName applica la regola dei nomi tmux (common.json#sessionName, spec 65).
func IsSessionName(s string) bool { return sessionNameRE.MatchString(s) }

// SessionOpen è il corpo di session.open.
type SessionOpen struct {
	Channel uint32 `json:"channel"`
	Session string `json:"session"`
	Create  bool   `json:"create"`
	Mode    string `json:"mode"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
	// FitToPane (schemi 0.6.0): il canale nasce con le dimensioni della finestra tmux, non con cols/rows.
	FitToPane bool `json:"fitToPane,omitempty"`
}

// ErrSessionName distingue un nome di sessione fuori regola (risposta SESSION_NAME_INVALID).
var ErrSessionName = errors.New("nome di sessione fuori regola")

func validSize(cols, rows int) error {
	if cols < 1 || cols > MaxTerminalSize || rows < 1 || rows > MaxTerminalSize {
		return fmt.Errorf("dimensioni %dx%d fuori da 1..%d: %w", cols, rows, MaxTerminalSize, ErrInvalid)
	}
	return nil
}

// DecodeSessionOpen decodifica e valida session.open.
func DecodeSessionOpen(body []byte) (SessionOpen, error) {
	var m struct {
		Channel   *uint32 `json:"channel"`
		Session   *string `json:"session"`
		Create    *bool   `json:"create"`
		Mode      *string `json:"mode"`
		Cols      *int    `json:"cols"`
		Rows      *int    `json:"rows"`
		FitToPane *bool   `json:"fitToPane"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return SessionOpen{}, err
	}
	if m.Channel == nil || m.Session == nil || m.Create == nil || m.Mode == nil || m.Cols == nil || m.Rows == nil {
		return SessionOpen{}, fmt.Errorf("session.open: campi obbligatori mancanti: %w", ErrInvalid)
	}
	o := SessionOpen{Channel: *m.Channel, Session: *m.Session, Create: *m.Create, Mode: *m.Mode, Cols: *m.Cols, Rows: *m.Rows}
	if m.FitToPane != nil {
		o.FitToPane = *m.FitToPane
	}
	if o.Mode != ModeView && o.Mode != ModeInteract {
		return o, fmt.Errorf("modo %q: %w", o.Mode, ErrInvalid)
	}
	if err := validSize(o.Cols, o.Rows); err != nil {
		return o, err
	}
	if !IsSessionName(o.Session) {
		return o, ErrSessionName
	}
	return o, nil
}

// SessionResize è il corpo di session.resize.
type SessionResize struct {
	Channel uint32 `json:"channel"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// DecodeSessionResize decodifica e valida session.resize.
func DecodeSessionResize(body []byte) (SessionResize, error) {
	var m struct {
		Channel *uint32 `json:"channel"`
		Cols    *int    `json:"cols"`
		Rows    *int    `json:"rows"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return SessionResize{}, err
	}
	if m.Channel == nil || m.Cols == nil || m.Rows == nil {
		return SessionResize{}, fmt.Errorf("session.resize: campi obbligatori mancanti: %w", ErrInvalid)
	}
	r := SessionResize{Channel: *m.Channel, Cols: *m.Cols, Rows: *m.Rows}
	return r, validSize(r.Cols, r.Rows)
}

// SessionClose è il corpo di session.close.
type SessionClose struct {
	Channel uint32 `json:"channel"`
}

// DecodeSessionClose decodifica session.close.
func DecodeSessionClose(body []byte) (SessionClose, error) {
	var m struct {
		Channel *uint32 `json:"channel"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return SessionClose{}, err
	}
	if m.Channel == nil {
		return SessionClose{}, fmt.Errorf("session.close: channel mancante: %w", ErrInvalid)
	}
	return SessionClose{Channel: *m.Channel}, nil
}

// DecodeSessionsList valida il corpo vuoto di sessions.list.
func DecodeSessionsList(body []byte) error {
	var m struct{}
	return DecodeStrict(body, &m)
}

// ChannelBody è il corpo dei comandi per canale (session.refresh, session.interrupt, session.buffer) e
// delle risposte di refresh e interrupt.
type ChannelBody struct {
	Channel uint32 `json:"channel"`
}

// DecodeChannelBody decodifica il corpo di un comando per canale.
func DecodeChannelBody(typ string, body []byte) (ChannelBody, error) {
	var m struct {
		Channel *uint32 `json:"channel"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return ChannelBody{}, err
	}
	if m.Channel == nil {
		return ChannelBody{}, fmt.Errorf("%s: channel mancante: %w", typ, ErrInvalid)
	}
	return ChannelBody{Channel: *m.Channel}, nil
}

// SessionBufferResult è il corpo di session.buffer.result: Text nil = nessun buffer recente.
type SessionBufferResult struct {
	Channel uint32  `json:"channel"`
	Text    *string `json:"text"`
}

// SessionBody è il corpo di session.kill e di session.kill.result.
type SessionBody struct {
	Session string `json:"session"`
}

// SessionRename è il corpo di session.rename e di session.rename.result.
type SessionRename struct {
	Session string `json:"session"`
	Name    string `json:"name"`
}

// SessionZoom è il corpo di session.zoom: On nil = inverti lo stato (il toggle della rotta SSH), altrimenti
// imposta quello chiesto.
type SessionZoom struct {
	Session string `json:"session"`
	On      *bool  `json:"on,omitempty"`
}

// SessionZoomResult è il corpo di session.zoom.result: On è lo stato dopo la richiesta, Panes il numero
// di pane della finestra quando si è letto (0 = ignoto, omesso).
type SessionZoomResult struct {
	Session string `json:"session"`
	On      bool   `json:"on"`
	Panes   int    `json:"panes,omitempty"`
}

// SessionCapture è il corpo di session.capture.
type SessionCapture struct {
	Session string `json:"session"`
	Range   string `json:"range"`
}

// SessionCaptureResult è il corpo di session.capture.result.
type SessionCaptureResult struct {
	Session string `json:"session"`
	Text    string `json:"text"`
}

func sessionField(typ string, s *string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("%s: session mancante: %w", typ, ErrInvalid)
	}
	if !IsSessionName(*s) {
		return *s, ErrSessionName
	}
	return *s, nil
}

// DecodeSessionKill decodifica session.kill.
func DecodeSessionKill(body []byte) (SessionBody, error) {
	var m struct {
		Session *string `json:"session"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return SessionBody{}, err
	}
	s, err := sessionField(TypeSessionKill, m.Session)
	return SessionBody{Session: s}, err
}

// DecodeSessionRename decodifica session.rename: tutti e due i nomi seguono la regola.
func DecodeSessionRename(body []byte) (SessionRename, error) {
	var m struct {
		Session *string `json:"session"`
		Name    *string `json:"name"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return SessionRename{}, err
	}
	if m.Name == nil {
		return SessionRename{}, fmt.Errorf("session.rename: name mancante: %w", ErrInvalid)
	}
	s, err := sessionField(TypeSessionRename, m.Session)
	if err != nil {
		return SessionRename{Session: s, Name: *m.Name}, err
	}
	if !IsSessionName(*m.Name) {
		return SessionRename{Session: s, Name: *m.Name}, ErrSessionName
	}
	return SessionRename{Session: s, Name: *m.Name}, nil
}

// DecodeSessionZoom decodifica session.zoom.
func DecodeSessionZoom(body []byte) (SessionZoom, error) {
	var m struct {
		Session *string `json:"session"`
		On      *bool   `json:"on"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return SessionZoom{}, err
	}
	s, err := sessionField(TypeSessionZoom, m.Session)
	return SessionZoom{Session: s, On: m.On}, err
}

// DecodeSessionCapture decodifica session.capture; range fuori dall'enum è un messaggio non valido (lo
// schema è chiuso: il default prudente di parseCaptureRange lo applica il backend, non il connettore).
func DecodeSessionCapture(body []byte) (SessionCapture, error) {
	var m struct {
		Session *string `json:"session"`
		Range   *string `json:"range"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return SessionCapture{}, err
	}
	if m.Range == nil || !slices.Contains(CaptureRanges, *m.Range) {
		return SessionCapture{}, fmt.Errorf("session.capture: range fuori da %v: %w", CaptureRanges, ErrInvalid)
	}
	s, err := sessionField(TypeSessionCapture, m.Session)
	return SessionCapture{Session: s, Range: *m.Range}, err
}

// SessionOpened è il corpo di session.opened.
type SessionOpened struct {
	Channel uint32 `json:"channel"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// SessionClosed è il corpo di session.closed.
type SessionClosed struct {
	Channel uint32 `json:"channel"`
	Reason  string `json:"reason"`
}

// SessionInfo è una voce di sessions.list.result.
type SessionInfo struct {
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
	Attached  bool   `json:"attached"`
	Windows   int    `json:"windows"`
	Agent     string `json:"agent,omitempty"`
	State     string `json:"state,omitempty"`
}

// SessionsListResult è il corpo di sessions.list.result.
type SessionsListResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

// MaxSessionsListed è il massimo di voci in sessions.list.result (schema).
const MaxSessionsListed = 1000

// EncodeFrame costruisce un frame binario.
func EncodeFrame(typ byte, channel uint32, data []byte) []byte {
	out := make([]byte, FrameHeaderBytes+len(data))
	out[0] = typ
	binary.BigEndian.PutUint32(out[1:5], channel)
	copy(out[FrameHeaderBytes:], data)
	return out
}

// DecodeFrame legge un frame binario. I dati restano quelli del buffer ricevuto.
func DecodeFrame(b []byte) (typ byte, channel uint32, data []byte, err error) {
	if len(b) < FrameHeaderBytes {
		return 0, 0, nil, fmt.Errorf("frame binario di %d byte, intestazione di %d: %w", len(b), FrameHeaderBytes, ErrInvalid)
	}
	typ = b[0]
	if typ != FrameOutput && typ != FrameInput {
		return 0, 0, nil, fmt.Errorf("tipo di frame 0x%02x: %w", typ, ErrInvalid)
	}
	return typ, binary.BigEndian.Uint32(b[1:5]), b[FrameHeaderBytes:], nil
}
