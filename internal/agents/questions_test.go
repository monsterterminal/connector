// Test delle domande dell'agente dalla card (spec monsterterminal-saas.md §2.14 e §1.21; ADR 0410
// A2). Corrispondenza (regola #26): la riga nuova della tabella di §1.21 (attende-input/domanda col
// requestId), le domande nel contesto nei tetti dello schema, la risposta per domanda che diventa la
// decisione dell'hook, «testo» sulla domanda sola, non-supportata su più domande senza answers e su
// approva/nega, la Notification di permesso che non chiude la domanda, request.closed «annullata»
// SOLO quando l'hook chiude la connessione prima della decisione.
package agents

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

// fruitQuestions sono le domande del banco del 2026-10-19 (E4, E7): header, descrizioni, multiSelect.
func fruitQuestions() []protocol.Question {
	return []protocol.Question{
		{Question: "Quale frutto?", Header: "Frutto", Options: []protocol.Option{{Label: "Mela", Description: "rossa"}, {Label: "Pera"}}},
		{Question: "Quali colori?", Options: []protocol.Option{{Label: "Rosso"}, {Label: "Verde"}, {Label: "Blu"}}, MultiSelect: true},
	}
}

// openQuestion simula l'hook PermissionRequest di AskUserQuestion: restituisce il lato dell'hook e il
// contesto mandato al backend.
func (b *bench) openQuestion(t *testing.T, session string, qs []protocol.Question) (net.Conn, protocol.RequestContext) {
	t.Helper()
	hook, conn := net.Pipe()
	detail := ""
	if len(qs) > 0 {
		detail = qs[0].Question
	}
	go b.rq.HandlePermission(HookMessage{Event: "PermissionRequest", Session: session, Action: "AskUserQuestion", Detail: detail, Kind: protocol.KindQuestion, Questions: qs}, conn)
	deadline := time.Now().Add(2 * time.Second)
	for {
		cs := b.out.ofType(protocol.TypeRequestContext)
		if len(cs) > 0 {
			ctx := cs[len(cs)-1].body.(protocol.RequestContext)
			if ctx.Session == session && ctx.Kind == protocol.KindQuestion {
				return hook, ctx
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("request.context della domanda non inviato")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func respondAnswers(b *bench, id, text string, answers []protocol.Answer) protocol.Envelope {
	body := map[string]any{"requestId": id, "answer": protocol.AnswerText, "text": text}
	if answers != nil {
		body["answers"] = answers
	}
	raw, _ := json.Marshal(body)
	env := protocol.Envelope{Type: protocol.TypeRequestRespond, ID: protocol.NewUUID(), TS: protocol.Timestamp(time.Now()), Body: raw}
	if !b.rq.Handle(env) {
		panic("request.respond non gestito")
	}
	return env
}

func requestsClosed(b *bench) []protocol.RequestClosed {
	var out []protocol.RequestClosed
	for _, c := range b.out.ofType(protocol.TypeRequestClosed) {
		out = append(out, c.body.(protocol.RequestClosed))
	}
	return out
}

func waitClosed(t *testing.T, b *bench, n int) []protocol.RequestClosed {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(requestsClosed(b)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("request.closed: %d, attesi %d", len(requestsClosed(b)), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return requestsClosed(b)
}

// §2.14 «Origine della domanda» e §1.21 riga nuova: kind domanda, action AskUserQuestion, questions nei
// tetti dello schema, deadline; nel giornale attende-input/domanda con lo stesso requestId; mai il
// testo delle domande nel giornale.
func TestAperturaDellaDomanda(t *testing.T) {
	b := newBench(t, true)
	hook, ctx := b.openQuestion(t, "lavoro", fruitQuestions())
	defer hook.Close()
	if ctx.Kind != protocol.KindQuestion || ctx.Action != "AskUserQuestion" || ctx.Detail != "Quale frutto?" || ctx.Deadline == "" ||
		len(ctx.Questions) != 2 || ctx.Questions[0].Header != "Frutto" || ctx.Questions[0].Options[0].Description != "rossa" || !ctx.Questions[1].MultiSelect {
		t.Fatalf("contesto %+v", ctx)
	}
	evs := b.j.all()
	if len(evs) != 1 || evs[0].State != protocol.StateWaiting || evs[0].WaitingFor != protocol.WaitQuestion || evs[0].RequestID != ctx.RequestID {
		t.Fatalf("giornale %+v", evs)
	}
	if raw, _ := json.Marshal(evs[0]); strings.Contains(string(raw), "frutto") {
		t.Fatal("il testo della domanda è finito nel giornale")
	}
	// Domande oltre i tetti: troncate nel contesto (opzioni alle prime dieci).
	b2 := newBench(t, true)
	many := []protocol.Question{{Question: strings.Repeat("d", 300), Options: make([]protocol.Option, 12)}}
	for i := range many[0].Options {
		many[0].Options[i].Label = "o"
	}
	hook2, ctx2 := b2.openQuestion(t, "lavoro", many)
	defer hook2.Close()
	if len(ctx2.Questions) != 1 || len(ctx2.Questions[0].Options) != protocol.MaxOptions || len(ctx2.Questions[0].Question) != protocol.MaxQuestionText {
		t.Fatalf("contesto non troncato: %d opzioni, domanda di %d", len(ctx2.Questions[0].Options), len(ctx2.Questions[0].Question))
	}
	// Domande rifiutate (cinque): richiesta domanda SENZA questions, la card resta al testo libero.
	b3 := newBench(t, true)
	five := make([]protocol.Question, 5)
	for i := range five {
		five[i] = protocol.Question{Question: "q", Options: []protocol.Option{{Label: "o"}}}
	}
	hook3, ctx3 := b3.openQuestion(t, "lavoro", five)
	defer hook3.Close()
	if ctx3.Kind != protocol.KindQuestion || ctx3.Questions != nil {
		t.Fatalf("cinque domande non rifiutate: %+v", ctx3)
	}
}

// §2.14 «La risposta»: request.respond testo + answers → l'hook riceve allow con una risposta per
// domanda, nell'ordine delle domande (multiSelect: etichette unite da «, »); esito consegnata; una
// seconda risposta è superata; nessun request.closed.
func TestRispostaAllaDomandaConAnswers(t *testing.T) {
	b := newBench(t, true)
	hook, ctx := b.openQuestion(t, "lavoro", fruitQuestions())
	env := respondAnswers(b, ctx.RequestID, "Mela", []protocol.Answer{{Question: "Quale frutto?", Text: "Mela"}, {Question: "Quali colori?", Text: "Rosso, Verde"}})
	if r := lastResult(t, b, env.ID); r.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("esito %+v", r)
	}
	d, ok := readDecision(t, hook, time.Second)
	if !ok || d.Behavior != "allow" || d.Message != "" || len(d.Answers) != 2 ||
		d.Answers[0] != (protocol.Answer{Question: "Quale frutto?", Text: "Mela"}) || d.Answers[1] != (protocol.Answer{Question: "Quali colori?", Text: "Rosso, Verde"}) {
		t.Fatalf("decisione %+v %v", d, ok)
	}
	hook.Close()
	env2 := respondAnswers(b, ctx.RequestID, "Pera", []protocol.Answer{{Question: "Quale frutto?", Text: "Pera"}, {Question: "Quali colori?", Text: "Blu"}})
	if r := lastResult(t, b, env2.ID); r.Outcome != protocol.OutcomeSuperseded {
		t.Fatalf("seconda risposta: %+v", r)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(requestsClosed(b)); n != 0 || b.rq.Open() != 0 {
		t.Fatalf("request.closed dopo una risposta consegnata: %d (aperte %d)", n, b.rq.Open())
	}
	// Le risposte si abbinano per posizione: con una domanda troncata nel contesto la risposta porta il
	// testo troncato (l'hook ricostruisce la chiave originale dal tool_input).
	b2 := newBench(t, true)
	long := strings.Repeat("d", 300)
	hook2, ctx2 := b2.openQuestion(t, "lavoro", []protocol.Question{{Question: long, Options: []protocol.Option{{Label: "sì"}}}})
	defer hook2.Close()
	respondAnswers(b2, ctx2.RequestID, "sì", []protocol.Answer{{Question: ctx2.Questions[0].Question, Text: "sì"}})
	if d, ok := readDecision(t, hook2, time.Second); !ok || len(d.Answers) != 1 || d.Answers[0].Question != long[:protocol.MaxQuestionText] {
		t.Fatalf("decisione %+v %v", d, ok)
	}
}

// §2.14: «testo» senza answers vale come risposta alla sola domanda; con più domande, con domande
// rifiutate (senza questions), con un numero di answers diverso dalle domande, e con approva/nega è
// non-supportata e la richiesta RESTA aperta.
func TestTestoSuDomandaSolaENonSupportata(t *testing.T) {
	b := newBench(t, true)
	hook, ctx := b.openQuestion(t, "lavoro", fruitQuestions()[:1])
	defer hook.Close()
	env := respondAnswers(b, ctx.RequestID, "Un kiwi", nil)
	if r := lastResult(t, b, env.ID); r.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("esito %+v", r)
	}
	if d, ok := readDecision(t, hook, time.Second); !ok || d.Behavior != "allow" || len(d.Answers) != 1 || d.Answers[0] != (protocol.Answer{Question: "Quale frutto?", Text: "Un kiwi"}) {
		t.Fatalf("decisione %+v %v", d, ok)
	}

	b2 := newBench(t, true)
	hook2, ctx2 := b2.openQuestion(t, "lavoro", fruitQuestions())
	defer hook2.Close()
	for name, env := range map[string]protocol.Envelope{
		"testo senza answers su due domande": respondAnswers(b2, ctx2.RequestID, "Mela", nil),
		"una sola answer per due domande":    respondAnswers(b2, ctx2.RequestID, "Mela", []protocol.Answer{{Question: "Quale frutto?", Text: "Mela"}}),
		"approva":                            respond(b2, ctx2.RequestID, protocol.AnswerApprove, ""),
		"nega":                               respond(b2, ctx2.RequestID, protocol.AnswerDeny, ""),
	} {
		if r := lastResult(t, b2, env.ID); r.Outcome != protocol.OutcomeUnsupported || r.RequestID != ctx2.RequestID {
			t.Errorf("%s: esito %+v", name, r)
		}
	}
	if _, ok := readLine(hook2, 200*time.Millisecond); ok || b2.rq.Open() != 1 {
		t.Fatal("una risposta non supportata ha chiuso la richiesta")
	}
	env2 := respondAnswers(b2, ctx2.RequestID, "Mela", []protocol.Answer{{Question: "Quale frutto?", Text: "Mela"}, {Question: "Quali colori?", Text: "Blu"}})
	if r := lastResult(t, b2, env2.ID); r.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("dopo una non-supportata la risposta buona doveva essere consegnata: %+v", r)
	}

	// Domanda senza questions (rifiutate): testo → non-supportata.
	b3 := newBench(t, true)
	five := make([]protocol.Question, 5)
	for i := range five {
		five[i] = protocol.Question{Question: "q", Options: []protocol.Option{{Label: "o"}}}
	}
	hook3, ctx3 := b3.openQuestion(t, "lavoro", five)
	defer hook3.Close()
	if r := lastResult(t, b3, respondAnswers(b3, ctx3.RequestID, "x", nil).ID); r.Outcome != protocol.OutcomeUnsupported {
		t.Fatalf("domanda senza questions: %+v", r)
	}
	// Per i permessi tutto invariato: answers su un permesso è non-supportata, approva passa.
	b4 := newBench(t, true)
	hook4, ctx4 := b4.open(t, "lavoro", "Bash", "ls")
	defer hook4.Close()
	if r := lastResult(t, b4, respond(b4, ctx4.RequestID, protocol.AnswerApprove, "").ID); r.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("permesso: %+v", r)
	}
}

// §2.14 e §1.21: la Notification permission_prompt che segue di 6 s il PermissionRequest di
// AskUserQuestion dice «permesso» ma la richiesta aperta è la domanda: non è una transizione, non la
// chiude e non scrive un secondo agent.state (stampo: TestLaNotificaDiPermessoNonSuperaLaRichiesta).
func TestLaNotificaDiPermessoNonSuperaLaDomanda(t *testing.T) {
	b := newBench(t, true)
	hook, ctx := b.openQuestion(t, "lavoro", fruitQuestions()[:1])
	defer hook.Close()
	if err := b.tr.HandleHook(HookMessage{Event: "Notification", Session: "lavoro", NotificationType: "permission_prompt"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readLine(hook, 300*time.Millisecond); ok {
		t.Fatal("la notifica di permesso ha chiuso la domanda")
	}
	if n := len(b.j.all()); n != 1 {
		t.Fatalf("agent.state nel giornale: %d, atteso 1", n)
	}
	env := respondAnswers(b, ctx.RequestID, "Mela", []protocol.Answer{{Question: "Quale frutto?", Text: "Mela"}})
	if r := lastResult(t, b, env.ID); r.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("la risposta dopo la notifica doveva essere consegnata: %+v", r)
	}
	if d, ok := readDecision(t, hook, time.Second); !ok || d.Behavior != "allow" {
		t.Fatalf("decisione %+v %v", d, ok)
	}
	// Una domanda risolta nel terminale resta superata al cambio di stato (PostToolUse), senza
	// request.closed.
	b2 := newBench(t, true)
	hook2, ctx2 := b2.openQuestion(t, "lavoro", fruitQuestions()[:1])
	_ = b2.tr.HandleHook(HookMessage{Event: "PostToolUse", Session: "lavoro"})
	if d, ok := readDecision(t, hook2, time.Second); !ok || d.Behavior != "" {
		t.Fatalf("PostToolUse doveva chiudere senza decisione: %+v %v", d, ok)
	}
	hook2.Close()
	if r := lastResult(t, b2, respondAnswers(b2, ctx2.RequestID, "x", nil).ID); r.Outcome != protocol.OutcomeSuperseded {
		t.Fatalf("dopo PostToolUse: %+v", r)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(requestsClosed(b2)); n != 0 {
		t.Fatalf("request.closed per una richiesta superata: %d", n)
	}
}

// §2.14 «Annullata nel terminale»: l'hook chiude la connessione prima della decisione (SIGTERM
// all'Esc) → la richiesta si chiude e parte request.closed {requestId, annullata}; vale anche per un
// permesso. Una risposta arrivata dopo è superata.
func TestAnnullataAllaChiusuraDellHook(t *testing.T) {
	for _, kind := range []string{protocol.KindQuestion, protocol.KindPermission} {
		b := newBench(t, true)
		var hook net.Conn
		var ctx protocol.RequestContext
		if kind == protocol.KindQuestion {
			hook, ctx = b.openQuestion(t, "lavoro", fruitQuestions())
		} else {
			hook, ctx = b.open(t, "lavoro", "Bash", "ls")
		}
		hook.Close()
		closed := waitClosed(t, b, 1)
		if closed[0] != (protocol.RequestClosed{RequestID: ctx.RequestID, Reason: protocol.ReasonCancelled}) {
			t.Fatalf("%s: request.closed %+v", kind, closed[0])
		}
		if b.rq.Open() != 0 {
			t.Fatalf("%s: richiesta ancora aperta", kind)
		}
		if r := lastResult(t, b, respond(b, ctx.RequestID, protocol.AnswerApprove, "").ID); r.Outcome != protocol.OutcomeSuperseded {
			t.Fatalf("%s: risposta dopo l'annullata: %+v", kind, r)
		}
		time.Sleep(50 * time.Millisecond)
		if n := len(requestsClosed(b)); n != 1 {
			t.Fatalf("%s: request.closed %d, atteso 1", kind, n)
		}
	}
	// Senza connessione al backend al momento dell'Esc, request.closed non si rimanda (non ha giornale
	// né conferma): la richiesta è chiusa e basta.
	b := newBench(t, false)
	hook, conn := net.Pipe()
	go b.rq.HandlePermission(HookMessage{Event: "PermissionRequest", Session: "lavoro", Action: "Bash", Detail: "ls"}, conn)
	deadline := time.Now().Add(time.Second)
	for b.rq.Open() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	hook.Close()
	deadline = time.Now().Add(time.Second)
	for b.rq.Open() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.rq.Open() != 0 {
		t.Fatal("richiesta non chiusa all'EOF senza backend")
	}
	b.rq.Attach(b.out)
	if len(b.out.all()) != 0 {
		t.Fatalf("rimandato qualcosa per una richiesta annullata: %+v", b.out.all())
	}
}

// §2.14: nessun request.closed quando è il connettore a chiudere: alla decisione, allo scadere della
// finestra, al cambio di stato, a una richiesta nuova sulla stessa sessione.
func TestNessunRequestClosedQuandoChiudeIlConnettore(t *testing.T) {
	// Decisione consegnata, poi l'hook chiude.
	b := newBench(t, true)
	hook, ctx := b.openQuestion(t, "lavoro", fruitQuestions()[:1])
	respondAnswers(b, ctx.RequestID, "Mela", nil)
	if d, ok := readDecision(t, hook, time.Second); !ok || d.Behavior != "allow" {
		t.Fatalf("decisione %+v %v", d, ok)
	}
	hook.Close()
	// Finestra scaduta, poi l'hook chiude.
	b2 := newBench(t, true)
	b2.probe = func(string) (bool, time.Time, bool) { return true, time.Now(), true }
	hook2, _ := b2.openQuestion(t, "lavoro", fruitQuestions()[:1])
	if d, ok := readDecision(t, hook2, 2*time.Second); !ok || d.Behavior != "" {
		t.Fatalf("alla scadenza: %+v %v", d, ok)
	}
	hook2.Close()
	// Cambio di stato (Stop), poi l'hook chiude.
	b3 := newBench(t, true)
	hook3, _ := b3.open(t, "lavoro", "Bash", "ls")
	_ = b3.tr.HandleHook(HookMessage{Event: "Stop", Session: "lavoro"})
	readLine(hook3, time.Second)
	hook3.Close()
	// Richiesta nuova sulla stessa sessione: la prima si chiude senza request.closed.
	b4 := newBench(t, true)
	first, _ := b4.open(t, "lavoro", "Bash", "ls")
	second, _ := b4.openQuestion(t, "lavoro", fruitQuestions()[:1])
	defer second.Close()
	readLine(first, time.Second)
	first.Close()
	time.Sleep(100 * time.Millisecond)
	for i, bb := range []*bench{b, b2, b3, b4} {
		if n := len(requestsClosed(bb)); n != 0 {
			t.Errorf("caso %d: request.closed %d, atteso 0", i, n)
		}
	}
}

// Il socket vero: Ask con il contesto annullato (il SIGTERM dell'Esc) chiude la connessione ed esce
// senza decisione; il connettore manda request.closed.
func TestAskAnnullatoSulSocket(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "hook.sock")
	b := newBench(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Listen(ctx, sock, b.tr) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket non creato")
		}
		time.Sleep(10 * time.Millisecond)
	}
	hctx, sigterm := context.WithCancel(context.Background())
	type res struct {
		d   Decision
		err error
	}
	got := make(chan res, 1)
	go func() {
		d, err := Ask(hctx, sock, HookMessage{Event: "PermissionRequest", Session: "lavoro", Action: "AskUserQuestion", Detail: "Quale frutto?", Kind: protocol.KindQuestion, Questions: fruitQuestions()[:1]}, time.Second, 5*time.Second)
		got <- res{d, err}
	}()
	deadline = time.Now().Add(2 * time.Second)
	for len(b.out.ofType(protocol.TypeRequestContext)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("contesto non inviato")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rc := b.out.ofType(protocol.TypeRequestContext)[0].body.(protocol.RequestContext)
	if rc.Kind != protocol.KindQuestion || len(rc.Questions) != 1 {
		t.Fatalf("contesto %+v", rc)
	}
	began := time.Now()
	sigterm()
	select {
	case r := <-got:
		if r.d.Behavior != "" || time.Since(began) > time.Second {
			t.Fatalf("Ask dopo l'annullamento: %+v %v in %v", r.d, r.err, time.Since(began))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask non è uscito all'annullamento")
	}
	closed := waitClosed(t, b, 1)
	if closed[0] != (protocol.RequestClosed{RequestID: rc.RequestID, Reason: protocol.ReasonCancelled}) {
		t.Fatalf("request.closed %+v", closed[0])
	}
	cancel()
	<-done
}
