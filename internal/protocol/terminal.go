package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
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
)

// Codici di errore del terminale (§1.16).
const (
	CodeChannelUnknown     = "CHANNEL_UNKNOWN"
	CodeSessionNotFound    = "SESSION_NOT_FOUND"
	CodeSessionNameInvalid = "SESSION_NAME_INVALID"
	CodeChannelLimit       = "CHANNEL_LIMIT"
)

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
		Channel *uint32 `json:"channel"`
		Session *string `json:"session"`
		Create  *bool   `json:"create"`
		Mode    *string `json:"mode"`
		Cols    *int    `json:"cols"`
		Rows    *int    `json:"rows"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return SessionOpen{}, err
	}
	if m.Channel == nil || m.Session == nil || m.Create == nil || m.Mode == nil || m.Cols == nil || m.Rows == nil {
		return SessionOpen{}, fmt.Errorf("session.open: campi obbligatori mancanti: %w", ErrInvalid)
	}
	o := SessionOpen{Channel: *m.Channel, Session: *m.Session, Create: *m.Create, Mode: *m.Mode, Cols: *m.Cols, Rows: *m.Rows}
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
