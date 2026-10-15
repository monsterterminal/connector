package protocol

import (
	"encoding/json"
	"regexp"
)

// Tipi di messaggio degli eventi (§1.6, §1.8, §1.12).
const (
	TypeEvent     = "event"
	TypeEventsAck = "events.ack"
)

// Kind degli eventi implementati (§1.8; schemi events/*.json).
const (
	KindConnectorRestarted = "connector.restarted"
	KindJournalTruncated   = "journal.truncated"
)

// Codici di errore del giornale lato backend (§1.12, §1.16, emendamento 2026-10-14).
const (
	// CodeAckAhead: il lastAckedSeq di hello è oltre il segnalibro del backend.
	CodeAckAhead = "ACK_AHEAD"
	// CodeSeqGap: un evento oltre il segnalibro del backend senza un troncamento coerente.
	CodeSeqGap = "SEQ_GAP"
	// CodeTruncationIncoherent: journal.truncated non coerente con il segnalibro del backend.
	CodeTruncationIncoherent = "TRUNCATION_INCOHERENT"
)

// MaxCounter è il tetto dei contatori (seq, lastAckedSeq, firstKeptSeq, droppedCount): 2^53 − 1 (§1.20).
const MaxCounter = 1<<53 - 1

// MaxEventDataBytes è il limite del campo data di un evento serializzato (§1.8).
const MaxEventDataBytes = 4096

// Event è il corpo del messaggio event (§1.8).
type Event struct {
	Seq  int64           `json:"seq"`
	Kind string          `json:"kind"`
	At   string          `json:"at"`
	Data json.RawMessage `json:"data"`
}

// EventsAck è il corpo di events.ack (§1.12): il backend ha ricevuto tutto fino a UpToSeq.
type EventsAck struct {
	UpToSeq int64 `json:"upToSeq"`
}

// DecodeEventsAck decodifica e valida events.ack.
func DecodeEventsAck(body []byte) (EventsAck, error) {
	var a EventsAck
	if err := DecodeStrict(body, &a); err != nil {
		return a, err
	}
	if a.UpToSeq < 1 || a.UpToSeq > MaxCounter {
		return a, invalid("upToSeq fuori intervallo")
	}
	return a, nil
}

// ConnectorRestarted è il data di connector.restarted (§1.8, §1.20).
type ConnectorRestarted struct {
	PreviousVersion string `json:"previousVersion"`
	Version         string `json:"version"`
	Reason          string `json:"reason"`
}

// Motivi del riavvio (§1.20).
var RestartReasons = []string{"update", "rollback", "host-reboot", "service-restart", "crash"}

// JournalTruncated è il data di journal.truncated (§1.12, forma coerente dell'emendamento 2026-10-14):
// il troncamento prende il seq dell'ULTIMO evento scartato; DroppedCount = seq − segnalibro del
// backend; FirstKeptSeq = seq + 1; OldestDroppedAt <= NewestDroppedAt.
type JournalTruncated struct {
	DroppedCount    int64  `json:"droppedCount"`
	FirstKeptSeq    int64  `json:"firstKeptSeq"`
	OldestDroppedAt string `json:"oldestDroppedAt"`
	NewestDroppedAt string `json:"newestDroppedAt"`
}

var semverRE = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?$`)

// IsSemver dice se s è una versione semver (§1.20).
func IsSemver(s string) bool { return semverRE.MatchString(s) }
