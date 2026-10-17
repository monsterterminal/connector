package protocol

import (
	"fmt"
	"time"
	"unicode/utf8"
)

// Richieste dell'agente (spec monsterterminal-saas.md §2.9, §2.12; schemi 0.3.0). Il contesto di una
// richiesta viaggia FUORI dal giornale (request.context), la risposta arriva dal backend
// (request.respond) e il connettore ne dichiara l'esito (request.respond.result).
const (
	TypeRequestContext       = "request.context"
	TypeRequestRespond       = "request.respond"
	TypeRequestRespondResult = "request.respond.result"
)

// Tipi di richiesta, risposte ed esiti: insiemi chiusi degli schemi 0.3.0.
const (
	KindPermission = "permesso"
	KindQuestion   = "domanda"

	AnswerApprove = "approva"
	AnswerDeny    = "nega"
	AnswerText    = "testo"

	OutcomeDelivered   = "consegnata"
	OutcomeSuperseded  = "superata"
	OutcomeUnsupported = "non-supportata"

	// MaxRequestDetail e MaxRequestAction sono i tetti di request.context (§2.9 punto 2).
	MaxRequestDetail = 16384
	MaxRequestAction = 200
)

// RequestContext è il corpo di request.context. Deadline (0.3.0, §2.12) è l'istante in cui scade la
// finestra in cui l'hook aspetta la risposta dalla card; un connettore 0.2.0 non lo manda.
type RequestContext struct {
	RequestID string `json:"requestId"`
	Session   string `json:"session"`
	Agent     string `json:"agent"`
	Kind      string `json:"kind"`
	Action    string `json:"action"`
	Detail    string `json:"detail"`
	Deadline  string `json:"deadline,omitempty"`
}

// RequestRespond è il corpo di request.respond: Text solo con answer «testo».
type RequestRespond struct {
	RequestID string `json:"requestId"`
	Answer    string `json:"answer"`
	Text      string `json:"text,omitempty"`
}

// RequestRespondResult è il corpo di request.respond.result.
type RequestRespondResult struct {
	RequestID string `json:"requestId"`
	Outcome   string `json:"outcome"`
}

// DecodeRequestRespond decodifica e valida request.respond (schema chiuso, text ⇔ testo).
func DecodeRequestRespond(body []byte) (RequestRespond, error) {
	var m struct {
		RequestID *string `json:"requestId"`
		Answer    *string `json:"answer"`
		Text      *string `json:"text"`
	}
	if err := DecodeStrict(body, &m); err != nil {
		return RequestRespond{}, err
	}
	if m.RequestID == nil || m.Answer == nil {
		return RequestRespond{}, fmt.Errorf("request.respond: campi obbligatori mancanti: %w", ErrInvalid)
	}
	r := RequestRespond{RequestID: *m.RequestID, Answer: *m.Answer}
	if !IsUUID(r.RequestID) {
		return r, invalid("requestId non è un UUID v4")
	}
	switch r.Answer {
	case AnswerApprove, AnswerDeny:
		if m.Text != nil {
			return r, invalid("text ammesso solo con answer «testo»")
		}
	case AnswerText:
		if m.Text == nil || *m.Text == "" {
			return r, invalid("answer «testo» senza text")
		}
		if len(*m.Text) > MaxRequestDetail || !utf8.ValidString(*m.Text) {
			return r, invalid("text oltre %d byte o non UTF-8", MaxRequestDetail)
		}
		r.Text = *m.Text
	default:
		return r, invalid("answer %q fuori dall'insieme", r.Answer)
	}
	return r, nil
}

// TruncateDetail taglia il contesto al tetto di request.context senza spezzare una runa (§2.12:
// «troncato con avviso»). Restituisce il testo e se è stato tagliato.
func TruncateDetail(s string) (string, bool) {
	if len(s) <= MaxRequestDetail {
		return s, false
	}
	cut := MaxRequestDetail
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// Deadline formatta la scadenza della finestra come gli altri istanti del protocollo.
func Deadline(t time.Time) string { return Timestamp(t) }
