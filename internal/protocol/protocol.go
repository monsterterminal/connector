// Package protocol contiene la busta e i messaggi del protocollo tra connettore e backend.
//
// Riferimento: spec monsterterminal-saas.md, capitolo 1 (protocollo 1), e gli schemi JSON in
// schemas/0.3.0. Ogni decodifica è rigida: un campo sconosciuto rende il messaggio non valido (§1.5).
package protocol

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// Version è il numero intero del protocollo (§1.15).
const Version = 1

// SchemaVersion è la versione degli schemi JSON che questo codice implementa (§1.19).
const SchemaVersion = "0.4.0"

// HeaderSchema dichiara al backend la versione degli schemi, su POST /v1/register e sull'upgrade
// di GET /v1 (§1.15, ADR 0418 D5). Il backend decide da QUESTO header, chiamata per chiamata, se la
// barriera del billing si presenta come BILLING_REQUIRED (da 0.4.0) o come HOST_DISABLED (prima).
const HeaderSchema = "X-MonsterTerminal-Schema"

// MaxFrameBytes è la dimensione massima di un frame (§1.2).
const MaxFrameBytes = 1 << 20

// Tipi di messaggio implementati in questa fetta (§1.6).
const (
	TypeHello        = "hello"
	TypeWelcome      = "welcome"
	TypeConfigUpdate = "config.update"
	TypeError        = "error"
)

// Codici di errore (§1.16).
const (
	CodeMessageInvalid = "MESSAGE_INVALID"
	CodeTypeUnknown    = "TYPE_UNKNOWN"
)

var (
	uuidRE      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	errorCodeRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
)

// ErrInvalid segnala un messaggio che non rispetta lo schema.
var ErrInvalid = errors.New("messaggio non valido")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Envelope è la busta comune dei frame di testo (§1.5, schemas/0.3.0/envelope.json).
type Envelope struct {
	Type string          `json:"type"`
	ID   string          `json:"id"`
	Re   string          `json:"re,omitempty"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

// NewUUID genera un UUID v4 in minuscolo.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("protocol: crypto/rand non disponibile: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Timestamp formatta un istante in RFC 3339 UTC con suffisso Z (§1.20).
func Timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// Encode costruisce un frame di testo con busta nuova. re è facoltativo.
func Encode(typ, re string, body any, now time.Time) ([]byte, string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	env := Envelope{Type: typ, ID: NewUUID(), Re: re, TS: Timestamp(now), Body: raw}
	out, err := json.Marshal(env)
	return out, env.ID, err
}

// DecodeStrict decodifica JSON rifiutando campi sconosciuti e dati in coda.
func DecodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("%v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return invalid("dati in coda dopo l'oggetto JSON")
	}
	return nil
}

// DecodeEnvelope decodifica e valida la busta (§1.5).
func DecodeEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := DecodeStrict(data, &env); err != nil {
		return env, err
	}
	switch {
	case env.Type == "":
		return env, invalid("type mancante")
	case !uuidRE.MatchString(env.ID):
		return env, invalid("id non è un UUID v4")
	case env.Re != "" && !uuidRE.MatchString(env.Re):
		return env, invalid("re non è un UUID v4")
	case len(env.Body) == 0 || env.Body[0] != '{':
		return env, invalid("body mancante o non oggetto")
	}
	if err := validTimestamp(env.TS); err != nil {
		return env, err
	}
	return env, nil
}

func validTimestamp(s string) error {
	if len(s) == 0 || s[len(s)-1] != 'Z' {
		return invalid("ts non è RFC 3339 in UTC")
	}
	if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		return invalid("ts non è RFC 3339: %v", err)
	}
	return nil
}

// IsUUID dice se s è un UUID v4 in minuscolo.
func IsUUID(s string) bool { return uuidRE.MatchString(s) }

// Agent è un agente rilevato sull'host (§1.20).
type Agent struct {
	Agent   string `json:"agent"`
	Version string `json:"version"`
}

// Hello è il corpo del primo messaggio del connettore (§1.4, §1.6).
type Hello struct {
	ConnectorVersion      string   `json:"connectorVersion"`
	Protocol              int      `json:"protocol"`
	OS                    string   `json:"os"`
	Arch                  string   `json:"arch"`
	RunAsUser             string   `json:"runAsUser"`
	RunAsRoot             bool     `json:"runAsRoot"`
	Capabilities          []string `json:"capabilities"`
	AutoUpdateEnabled     bool     `json:"autoUpdateEnabled"`
	CatalogInstallEnabled bool     `json:"catalogInstallEnabled"`
	LastAckedSeq          int64    `json:"lastAckedSeq"`
	Agents                []Agent  `json:"agents"`
}

// Recording è la configurazione della registrazione per il replay (§1.11).
type Recording struct {
	Enabled       bool `json:"enabled"`
	RetentionDays *int `json:"retentionDays,omitempty"`
}

// HostConfig è la configurazione decisa dal backend (§1.11).
type HostConfig struct {
	DefaultIdleThresholdSeconds int            `json:"defaultIdleThresholdSeconds"`
	SessionIdleThresholds       map[string]int `json:"sessionIdleThresholds"`
	Recording                   Recording      `json:"recording"`
	MaxChannels                 int            `json:"maxChannels"`
}

// Validate controlla i vincoli di objects/host-config.json.
func (c HostConfig) Validate() error {
	inRange := func(v int) bool { return v >= 60 && v <= 86400 }
	if !inRange(c.DefaultIdleThresholdSeconds) {
		return invalid("defaultIdleThresholdSeconds fuori intervallo")
	}
	for _, v := range c.SessionIdleThresholds {
		if !inRange(v) {
			return invalid("soglia di sessione fuori intervallo")
		}
	}
	if c.SessionIdleThresholds == nil {
		return invalid("sessionIdleThresholds mancante")
	}
	if c.Recording.Enabled {
		if r := c.Recording.RetentionDays; r == nil || *r < 1 || *r > 365 {
			return invalid("recording.retentionDays mancante o fuori intervallo")
		}
	} else if c.Recording.RetentionDays != nil {
		return invalid("recording.retentionDays presente con registrazione spenta")
	}
	if c.MaxChannels < 1 {
		return invalid("maxChannels deve essere almeno 1")
	}
	return nil
}

// Welcome è la risposta del backend a hello (§1.4).
type Welcome struct {
	HostID     string     `json:"hostId"`
	Protocol   int        `json:"protocol"`
	ServerTime string     `json:"serverTime"`
	Config     HostConfig `json:"config"`
}

// DecodeWelcome decodifica e valida il corpo di welcome.
func DecodeWelcome(body []byte) (Welcome, error) {
	var w Welcome
	if err := DecodeStrict(body, &w); err != nil {
		return w, err
	}
	if !uuidRE.MatchString(w.HostID) {
		return w, invalid("hostId non è un UUID v4")
	}
	if w.Protocol < 1 {
		return w, invalid("protocol mancante")
	}
	if err := validTimestamp(w.ServerTime); err != nil {
		return w, err
	}
	return w, w.Config.Validate()
}

// ConfigUpdate è il corpo di config.update (§1.11).
type ConfigUpdate struct {
	Config HostConfig `json:"config"`
}

// ErrorBody è il corpo di error (§1.5).
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DecodeError decodifica e valida il corpo di error.
func DecodeError(body []byte) (ErrorBody, error) {
	var e ErrorBody
	if err := DecodeStrict(body, &e); err != nil {
		return e, err
	}
	if !errorCodeRE.MatchString(e.Code) || len(e.Message) > 2000 {
		return e, invalid("error.code o error.message non validi")
	}
	return e, nil
}
