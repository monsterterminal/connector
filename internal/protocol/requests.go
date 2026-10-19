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
	// TypeRequestClosed (0.5.0, §2.14): il connettore ha chiuso da sé una richiesta ancora aperta.
	TypeRequestClosed = "request.closed"
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

	// ReasonCancelled è l'unico motivo di request.closed (0.5.0, §2.14): l'utente ha annullato il
	// prompt nel terminale e Claude Code ha terminato l'hook in attesa.
	ReasonCancelled = "annullata"

	// MaxRequestDetail e MaxRequestAction sono i tetti di request.context (§2.9 punto 2).
	MaxRequestDetail = 16384
	MaxRequestAction = 200

	// Tetti delle domande e delle risposte (schemi 0.5.0, §2.14), in caratteri come i maxLength
	// dello schema: al più 4 domande da 200 caratteri (intestazione compresa), al più 10 opzioni
	// per domanda con etichetta da 200 e descrizione da 500; una risposta da 2000.
	MaxQuestions         = 4
	MaxQuestionText      = 200
	MaxOptions           = 10
	MaxOptionLabel       = 200
	MaxOptionDescription = 500
	MaxAnswerText        = 2000
)

// ClosedReasonsOfRequest sono i motivi di request.closed, l'enum dello schema nello stesso ordine.
var ClosedReasonsOfRequest = []string{ReasonCancelled}

// Question è una domanda dell'agente con le sue opzioni (request.context.questions, §2.14): la forma
// di AskUserQuestion di Claude Code, chiusa sui campi dello schema.
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	Options     []Option `json:"options"`
	MultiSelect bool     `json:"multiSelect"`
}

// Option è una risposta rapida proposta dall'agente.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Answer è la risposta a una domanda (request.respond.answers, §2.14), nello stesso ordine di
// request.context.questions.
type Answer struct {
	Question string `json:"question"`
	Text     string `json:"text"`
}

// RequestClosed è il corpo di request.closed (0.5.0, §2.14).
type RequestClosed struct {
	RequestID string `json:"requestId"`
	Reason    string `json:"reason"`
}

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
	// Questions (0.5.0, §2.14) solo con kind «domanda»: le domande con le opzioni, già nei tetti
	// dello schema (NormalizeQuestions). Assente, la card resta al testo libero.
	Questions []Question `json:"questions,omitempty"`
}

// RequestRespond è il corpo di request.respond: Text e Answers solo con answer «testo»; Answers
// (0.5.0, §2.14) porta una risposta per domanda, nello stesso ordine di request.context.questions.
type RequestRespond struct {
	RequestID string   `json:"requestId"`
	Answer    string   `json:"answer"`
	Text      string   `json:"text,omitempty"`
	Answers   []Answer `json:"answers,omitempty"`
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
		Answers   *[]struct {
			Question *string `json:"question"`
			Text     *string `json:"text"`
		} `json:"answers"`
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
		if m.Answers != nil {
			return r, invalid("answers ammesso solo con answer «testo»")
		}
	case AnswerText:
		if m.Text == nil || *m.Text == "" {
			return r, invalid("answer «testo» senza text")
		}
		if len(*m.Text) > MaxRequestDetail || !utf8.ValidString(*m.Text) {
			return r, invalid("text oltre %d byte o non UTF-8", MaxRequestDetail)
		}
		r.Text = *m.Text
		if m.Answers != nil {
			// Una risposta per domanda (§2.14): al più MaxQuestions, mai vuota, nei tetti dello schema.
			if n := len(*m.Answers); n == 0 || n > MaxQuestions {
				return r, invalid("answers con %d voci (da 1 a %d)", n, MaxQuestions)
			}
			for i, a := range *m.Answers {
				if a.Question == nil || a.Text == nil {
					return r, invalid("answers[%d]: campi obbligatori mancanti", i)
				}
				if !validText(*a.Question, 1, MaxQuestionText) {
					return r, invalid("answers[%d].question vuota, oltre %d caratteri o non UTF-8", i, MaxQuestionText)
				}
				if !validText(*a.Text, 1, MaxAnswerText) {
					return r, invalid("answers[%d].text vuoto, oltre %d caratteri o non UTF-8", i, MaxAnswerText)
				}
				r.Answers = append(r.Answers, Answer{Question: *a.Question, Text: *a.Text})
			}
		}
	default:
		return r, invalid("answer %q fuori dall'insieme", r.Answer)
	}
	return r, nil
}

// validText dice se s è UTF-8 valido e lungo fra min e max caratteri (i maxLength dello schema
// contano i caratteri, non i byte).
func validText(s string, min, max int) bool {
	if !utf8.ValidString(s) {
		return false
	}
	n := utf8.RuneCountInString(s)
	return n >= min && n <= max
}

// cutRunes taglia s a max caratteri senza spezzare una runa.
func cutRunes(s string, max int) (string, bool) {
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i], true
		}
		n++
	}
	return s, false
}

// NormalizeQuestions porta le domande di AskUserQuestion nei tetti dello schema 0.5.0 (§2.14).
// Troncamento, con avviso, di ciò che è solo da mostrare: testo e intestazione a 200 caratteri,
// etichetta a 200 e descrizione a 500, opzioni oltre la decima scartate. Rifiuto (nil, con ok=false)
// di ciò che cambierebbe la forma della richiesta: nessuna domanda o più di quattro, una domanda
// senza testo, senza opzioni o con un'etichetta vuota, un testo non UTF-8. Con il rifiuto la card
// resta al testo libero (richiesta «domanda» senza questions) e la risposta vale solo con una domanda
// sola; le risposte si abbinano per posizione, così il troncamento non le tocca.
func NormalizeQuestions(qs []Question) (out []Question, truncated, ok bool) {
	if len(qs) == 0 || len(qs) > MaxQuestions {
		return nil, false, false
	}
	out = make([]Question, 0, len(qs))
	for _, q := range qs {
		if q.Question == "" || len(q.Options) == 0 || !utf8.ValidString(q.Question) || !utf8.ValidString(q.Header) {
			return nil, false, false
		}
		n := Question{MultiSelect: q.MultiSelect}
		var cut bool
		n.Question, cut = cutRunes(q.Question, MaxQuestionText)
		truncated = truncated || cut
		n.Header, cut = cutRunes(q.Header, MaxQuestionText)
		truncated = truncated || cut
		opts := q.Options
		if len(opts) > MaxOptions {
			opts, truncated = opts[:MaxOptions], true
		}
		n.Options = make([]Option, 0, len(opts))
		for _, o := range opts {
			if o.Label == "" || !utf8.ValidString(o.Label) || !utf8.ValidString(o.Description) {
				return nil, false, false
			}
			var no Option
			no.Label, cut = cutRunes(o.Label, MaxOptionLabel)
			truncated = truncated || cut
			no.Description, cut = cutRunes(o.Description, MaxOptionDescription)
			truncated = truncated || cut
			n.Options = append(n.Options, no)
		}
		out = append(out, n)
	}
	return out, truncated, true
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
