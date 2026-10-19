// Test di CORRISPONDENZA delle domande dell'agente (spec monsterterminal-saas.md §2.14 e §1.21;
// schemi 0.5.0): la forma di `questions` in request.context, di `answers` in request.respond e il
// vocabolario di request.closed sono letti dagli schemi JSON, non riscritti qui (regola #26). Poi la
// validazione dei tetti (troncamento o rifiuto documentato) e la decodifica di request.respond con
// answers.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func bodyProps(t *testing.T, rel string) map[string]any {
	t.Helper()
	return readSchema(t, rel)["properties"].(map[string]any)["body"].(map[string]any)["properties"].(map[string]any)
}

func maxOf(m map[string]any, field, key string) int {
	return int(m[field].(map[string]any)[key].(float64))
}

// §2.14 «Schemi 0.5.0»: questions (4 domande, 200 caratteri, 10 opzioni da 200 con descrizione da
// 500), answers (question, text ≤ 2000), request.closed (requestId, reason ∈ annullata).
func TestCorrispondenzaDomandeConGliSchemi(t *testing.T) {
	ctx := bodyProps(t, "messages/connector-to-backend/request.context.json")
	qs := ctx["questions"].(map[string]any)
	if int(qs["maxItems"].(float64)) != MaxQuestions || int(qs["minItems"].(float64)) != 1 {
		t.Errorf("questions: maxItems %v, Go %d", qs["maxItems"], MaxQuestions)
	}
	qp := qs["items"].(map[string]any)["properties"].(map[string]any)
	full := Question{Question: "q", Header: "h", Options: []Option{{Label: "l", Description: "d"}}, MultiSelect: true}
	if got, want := jsonKeys(t, full), keys(qp); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("domanda: campi Go %v, schema %v", got, want)
	}
	if fmt.Sprint(qs["items"].(map[string]any)["required"]) != fmt.Sprint([]string{"question", "options", "multiSelect"}) {
		t.Errorf("domanda: required %v (header è facoltativo)", qs["items"].(map[string]any)["required"])
	}
	if maxOf(qp, "question", "maxLength") != MaxQuestionText || maxOf(qp, "header", "maxLength") != MaxQuestionText {
		t.Errorf("tetto della domanda: schema %v/%v, Go %d", qp["question"], qp["header"], MaxQuestionText)
	}
	ops := qp["options"].(map[string]any)
	if int(ops["maxItems"].(float64)) != MaxOptions || int(ops["minItems"].(float64)) != 1 {
		t.Errorf("opzioni: schema %v, Go %d", ops["maxItems"], MaxOptions)
	}
	op := ops["items"].(map[string]any)["properties"].(map[string]any)
	if got, want := jsonKeys(t, Option{Label: "l", Description: "d"}), keys(op); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("opzione: campi Go %v, schema %v", got, want)
	}
	if maxOf(op, "label", "maxLength") != MaxOptionLabel || maxOf(op, "description", "maxLength") != MaxOptionDescription {
		t.Errorf("tetti dell'opzione: schema %v, Go %d/%d", op, MaxOptionLabel, MaxOptionDescription)
	}
	// request.context: tutti i campi, questions compreso.
	rc := RequestContext{Deadline: "x", Questions: []Question{full}}
	if got, want := jsonKeys(t, rc), keys(ctx); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("request.context: campi Go %v, schema %v", got, want)
	}

	rr := bodyProps(t, "messages/backend-to-connector/request.respond.json")
	as := rr["answers"].(map[string]any)
	if int(as["maxItems"].(float64)) != MaxQuestions {
		t.Errorf("answers: maxItems %v, Go %d", as["maxItems"], MaxQuestions)
	}
	ap := as["items"].(map[string]any)["properties"].(map[string]any)
	if got, want := jsonKeys(t, Answer{Question: "q", Text: "t"}), keys(ap); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("risposta: campi Go %v, schema %v", got, want)
	}
	if maxOf(ap, "question", "maxLength") != MaxQuestionText || maxOf(ap, "text", "maxLength") != MaxAnswerText {
		t.Errorf("tetti della risposta: schema %v, Go %d/%d", ap, MaxQuestionText, MaxAnswerText)
	}
	if got, want := jsonKeys(t, RequestRespond{Text: "t", Answers: []Answer{{"q", "t"}}}), keys(rr); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("request.respond: campi Go %v, schema %v", got, want)
	}

	// request.closed: campi e vocabolario del motivo uguali allo schema. Nota: lo schema elenca `re` fra
	// i campi obbligatori della busta; il messaggio non risponde a nulla e il connettore lo manda senza
	// `re` (dubbio segnalato nel decision-log della fetta).
	cl := readSchema(t, "messages/connector-to-backend/request.closed.json")
	if cl["properties"].(map[string]any)["type"].(map[string]any)["const"] != TypeRequestClosed {
		t.Errorf("tipo: schema %v, Go %s", cl["properties"].(map[string]any)["type"], TypeRequestClosed)
	}
	cp := bodyProps(t, "messages/connector-to-backend/request.closed.json")
	if got, want := jsonKeys(t, RequestClosed{RequestID: "x", Reason: "x"}), keys(cp); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("request.closed: campi Go %v, schema %v", got, want)
	}
	if fmt.Sprint(cp["reason"].(map[string]any)["enum"]) != fmt.Sprint(ClosedReasonsOfRequest) {
		t.Errorf("motivi di request.closed: schema %v, Go %v", cp["reason"], ClosedReasonsOfRequest)
	}
	if fmt.Sprint(ctx["kind"].(map[string]any)["enum"]) != fmt.Sprint([]string{KindPermission, KindQuestion}) {
		t.Errorf("kind: schema %v", ctx["kind"])
	}
}

func opts(n int) []Option {
	out := make([]Option, n)
	for i := range out {
		out[i] = Option{Label: fmt.Sprintf("opzione %d", i+1)}
	}
	return out
}

// §2.14: le domande oltre i tetti si troncano (testi, opzioni oltre la decima) con avviso; ciò che
// cambierebbe la forma (nessuna domanda, più di quattro, domanda senza testo o senza opzioni,
// etichetta vuota, testo non UTF-8) si rifiuta e la card resta al testo libero.
func TestNormalizeQuestions(t *testing.T) {
	q := func(n int) []Question {
		out := make([]Question, n)
		for i := range out {
			out[i] = Question{Question: fmt.Sprintf("domanda %d", i+1), Options: opts(2)}
		}
		return out
	}
	// Forma vera di Claude Code (banco E4): header e descrizioni presenti; nulla da troncare.
	in := []Question{{Question: "Quale frutto?", Header: "Frutto", Options: []Option{{Label: "Mela", Description: "rossa"}, {Label: "Pera"}}}}
	out, cut, ok := NormalizeQuestions(in)
	if !ok || cut || fmt.Sprint(out) != fmt.Sprint(in) {
		t.Fatalf("domanda regolare: %+v cut=%v ok=%v", out, cut, ok)
	}
	// Quattro domande, header mancante, multiSelect: ok senza tagli.
	four := q(4)
	four[3].MultiSelect = true
	if out, cut, ok := NormalizeQuestions(four); !ok || cut || len(out) != 4 || !out[3].MultiSelect {
		t.Fatalf("quattro domande: %+v cut=%v ok=%v", out, cut, ok)
	}
	// Rifiuti.
	for name, bad := range map[string][]Question{
		"nessuna":          nil,
		"cinque":           q(5),
		"senza testo":      {{Question: "", Options: opts(1)}},
		"senza opzioni":    {{Question: "q"}},
		"etichetta vuota":  {{Question: "q", Options: []Option{{Label: ""}}}},
		"testo non UTF-8":  {{Question: "q\xff", Options: opts(1)}},
		"label non UTF-8":  {{Question: "q", Options: []Option{{Label: "\xff"}}}},
		"header non UTF-8": {{Question: "q", Header: "\xff", Options: opts(1)}},
	} {
		if out, _, ok := NormalizeQuestions(bad); ok || out != nil {
			t.Errorf("%s: accettata (%+v)", name, out)
		}
	}
	// Troncamenti: testi ai tetti senza spezzare una runa, opzioni alle prime dieci.
	long := strings.Repeat("à", 300)
	in = []Question{{Question: long, Header: long, Options: append(opts(12), Option{Label: long, Description: strings.Repeat("é", 600)})}}
	out, cut, ok = NormalizeQuestions(in)
	if !ok || !cut || len(out[0].Options) != MaxOptions {
		t.Fatalf("troncamento: cut=%v ok=%v opzioni=%d", cut, ok, len(out[0].Options))
	}
	if n := len([]rune(out[0].Question)); n != MaxQuestionText || len([]rune(out[0].Header)) != MaxQuestionText || !strings.HasSuffix(out[0].Question, "à") {
		t.Fatalf("domanda troncata a %d caratteri: %q", n, out[0].Question[:20])
	}
	if out[0].Options[0].Label != "opzione 1" || out[0].Options[9].Label != "opzione 10" {
		t.Fatalf("opzioni tenute: %+v", out[0].Options)
	}
	in = []Question{{Question: "q", Options: []Option{{Label: long, Description: strings.Repeat("é", 600)}}}}
	out, cut, ok = NormalizeQuestions(in)
	if !ok || !cut || len([]rune(out[0].Options[0].Label)) != MaxOptionLabel || len([]rune(out[0].Options[0].Description)) != MaxOptionDescription {
		t.Fatalf("opzione troncata: %+v cut=%v ok=%v", out, cut, ok)
	}
	// L'input non si tocca.
	if len([]rune(in[0].Options[0].Label)) != 300 {
		t.Fatal("NormalizeQuestions ha modificato l'input")
	}
}

// §2.14: answers solo con «testo», da 1 a 4 voci, ciascuna con question (≤ 200) e text (≤ 2000) non
// vuoti; campi in più rifiutati.
func TestDecodeRequestRespondConAnswers(t *testing.T) {
	id := NewUUID()
	good := `{"requestId":"` + id + `","answer":"testo","text":"Mela","answers":[{"question":"Quale frutto?","text":"Mela"},{"question":"Colore?","text":"Rosso, Verde"}]}`
	r, err := DecodeRequestRespond([]byte(good))
	if err != nil || len(r.Answers) != 2 || r.Answers[0] != (Answer{"Quale frutto?", "Mela"}) || r.Answers[1].Text != "Rosso, Verde" || r.Text != "Mela" {
		t.Fatalf("decodifica %+v %v", r, err)
	}
	// Senza answers resta come nella 0.4.0.
	if r, err := DecodeRequestRespond([]byte(`{"requestId":"` + id + `","answer":"testo","text":"Mela"}`)); err != nil || r.Answers != nil {
		t.Fatalf("senza answers: %+v %v", r, err)
	}
	five := strings.Repeat(`{"question":"q","text":"t"},`, 5)
	for name, body := range map[string]string{
		"answers con approva":    `{"requestId":"` + id + `","answer":"approva","answers":[{"question":"q","text":"t"}]}`,
		"answers con nega":       `{"requestId":"` + id + `","answer":"nega","answers":[{"question":"q","text":"t"}]}`,
		"answers vuoto":          `{"requestId":"` + id + `","answer":"testo","text":"t","answers":[]}`,
		"cinque risposte":        `{"requestId":"` + id + `","answer":"testo","text":"t","answers":[` + five[:len(five)-1] + `]}`,
		"risposta senza testo":   `{"requestId":"` + id + `","answer":"testo","text":"t","answers":[{"question":"q","text":""}]}`,
		"risposta senza domanda": `{"requestId":"` + id + `","answer":"testo","text":"t","answers":[{"text":"t"}]}`,
		"campo in più":           `{"requestId":"` + id + `","answer":"testo","text":"t","answers":[{"question":"q","text":"t","quickReply":true}]}`,
		"domanda troppo lunga":   `{"requestId":"` + id + `","answer":"testo","text":"t","answers":[{"question":"` + strings.Repeat("a", 201) + `","text":"t"}]}`,
		"testo troppo lungo":     `{"requestId":"` + id + `","answer":"testo","text":"t","answers":[{"question":"q","text":"` + strings.Repeat("a", 2001) + `"}]}`,
	} {
		if _, err := DecodeRequestRespond([]byte(body)); err == nil {
			t.Errorf("%s: accettato", name)
		}
	}
	// I tetti contano i caratteri: 2000 «à» (4000 byte) passano.
	ok := `{"requestId":"` + id + `","answer":"testo","text":"t","answers":[{"question":"q","text":"` + strings.Repeat("à", 2000) + `"}]}`
	if _, err := DecodeRequestRespond([]byte(ok)); err != nil {
		t.Fatalf("2000 caratteri multibyte rifiutati: %v", err)
	}
	// Il corpo di request.closed si codifica con i soli due campi dello schema.
	raw, _ := json.Marshal(RequestClosed{RequestID: id, Reason: ReasonCancelled})
	if string(raw) != `{"requestId":"`+id+`","reason":"annullata"}` {
		t.Fatalf("request.closed: %s", raw)
	}
}
