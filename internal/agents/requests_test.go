// Test delle richieste dell'agente (spec monsterterminal-saas.md §2.9, §2.12; §1.21 «Hook di
// permesso»; ADR 0410). Regola #26: la finestra adattiva, la chiusura per cambio di stato, «una sola
// richiesta aperta per sessione», il rinvio del contesto dopo welcome e gli esiti di request.respond
// sono verificati contro il contratto.
package agents

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

type control struct {
	typ, re string
	body    any
}

type fakeOut struct {
	mu   sync.Mutex
	sent []control
}

func (f *fakeOut) Control(typ, re string, body any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, control{typ, re, body})
	return nil
}

func (f *fakeOut) all() []control {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]control(nil), f.sent...)
}

func (f *fakeOut) ofType(typ string) []control {
	var out []control
	for _, c := range f.all() {
		if c.typ == typ {
			out = append(out, c)
		}
	}
	return out
}

type bench struct {
	j     *fakeJournal
	tr    *Tracker
	rq    *Requests
	out   *fakeOut
	probe func(string) (bool, time.Time, bool)
}

func newBench(t *testing.T, attach bool) *bench {
	t.Helper()
	b := &bench{j: &fakeJournal{}, out: &fakeOut{}}
	b.probe = func(string) (bool, time.Time, bool) { return false, time.Time{}, false }
	b.tr = NewTracker(b.j, nil, nil)
	b.rq = NewRequests(RequestsConfig{
		ShortWindow: 300 * time.Millisecond, LongWindow: 2 * time.Second, ActiveWithin: time.Minute,
		Probe: func(s string) (bool, time.Time, bool) { return b.probe(s) },
	}, b.tr)
	if attach {
		b.rq.Attach(b.out)
	}
	return b
}

// open simula un hook di permesso: restituisce il lato dell'hook e il contesto mandato al backend.
func (b *bench) open(t *testing.T, session, action, detail string) (net.Conn, protocol.RequestContext) {
	t.Helper()
	hook, conn := net.Pipe()
	go b.rq.HandlePermission(HookMessage{Event: "PermissionRequest", Session: session, Action: action, Detail: detail}, conn)
	var ctx protocol.RequestContext
	deadline := time.Now().Add(2 * time.Second)
	for {
		cs := b.out.ofType(protocol.TypeRequestContext)
		if len(cs) > 0 {
			ctx = cs[len(cs)-1].body.(protocol.RequestContext)
			if ctx.Session == session && ctx.Action == action {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("request.context non inviato")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return hook, ctx
}

func readDecision(t *testing.T, hook net.Conn, wait time.Duration) (Decision, bool) {
	t.Helper()
	line, ok := readLine(hook, wait)
	if !ok {
		return Decision{}, false
	}
	var d Decision
	if err := json.Unmarshal([]byte(line), &d); err != nil {
		t.Fatalf("riga dell'hook non valida: %q", line)
	}
	return d, true
}

func respond(b *bench, id, answer, text string) protocol.Envelope {
	body := map[string]any{"requestId": id, "answer": answer}
	if text != "" {
		body["text"] = text
	}
	raw, _ := json.Marshal(body)
	env := protocol.Envelope{Type: protocol.TypeRequestRespond, ID: protocol.NewUUID(), TS: protocol.Timestamp(time.Now()), Body: raw}
	if !b.rq.Handle(env) {
		panic("request.respond non gestito")
	}
	return env
}

func lastResult(t *testing.T, b *bench, re string) protocol.RequestRespondResult {
	t.Helper()
	for _, c := range b.out.ofType(protocol.TypeRequestRespondResult) {
		if c.re == re {
			return c.body.(protocol.RequestRespondResult)
		}
	}
	t.Fatalf("nessun request.respond.result con re=%s", re)
	return protocol.RequestRespondResult{}
}

// §2.12 «Finestra adattiva»: 2 minuti con un client collegato e attivo, 60 minuti altrimenti.
func TestFinestraAdattiva(t *testing.T) {
	b := newBench(t, true)
	now := time.Now()
	for _, c := range []struct {
		name     string
		attached bool
		activity time.Time
		ok       bool
		want     time.Duration
	}{
		{"collegato e attivo", true, now.Add(-10 * time.Second), true, 300 * time.Millisecond},
		{"collegato ma fermo da troppo", true, now.Add(-2 * time.Minute), true, 2 * time.Second},
		{"nessun client", false, now, true, 2 * time.Second},
		{"tmux non risponde", false, time.Time{}, false, 2 * time.Second},
	} {
		b.probe = func(string) (bool, time.Time, bool) { return c.attached, c.activity, c.ok }
		if got := b.rq.Window("s"); got != c.want {
			t.Errorf("%s: finestra %v, attesa %v", c.name, got, c.want)
		}
	}
}

// §2.9 punto 2 e §2.12: l'apertura manda il contesto fuori dal giornale (con deadline, 0.3.0) e scrive
// nel giornale attende-input/permesso con lo stesso requestId; mai il contesto nel giornale.
func TestAperturaDellaRichiesta(t *testing.T) {
	b := newBench(t, true)
	b.probe = func(string) (bool, time.Time, bool) { return true, time.Now(), true }
	hook, ctx := b.open(t, "lavoro", "Bash", "rm -rf /tmp/x")
	defer hook.Close()
	if !protocol.IsUUID(ctx.RequestID) || ctx.Kind != protocol.KindPermission || ctx.Agent != protocol.AgentClaude ||
		ctx.Detail != "rm -rf /tmp/x" || ctx.Deadline == "" {
		t.Fatalf("contesto %+v", ctx)
	}
	dl, err := time.Parse(time.RFC3339Nano, ctx.Deadline)
	if err != nil || dl.Before(time.Now()) || dl.After(time.Now().Add(time.Second)) {
		t.Fatalf("deadline %q non è la finestra breve: %v", ctx.Deadline, err)
	}
	evs := b.j.all()
	if len(evs) != 1 || evs[0].State != protocol.StateWaiting || evs[0].WaitingFor != protocol.WaitPermission ||
		evs[0].RequestID != ctx.RequestID || evs[0].Source != protocol.SourceHook {
		t.Fatalf("giornale %+v", evs)
	}
	raw, _ := json.Marshal(evs[0])
	if strings.Contains(string(raw), "rm -rf") {
		t.Fatal("il contesto è finito nel giornale")
	}
	if b.rq.Open() != 1 {
		t.Fatalf("richieste aperte %d", b.rq.Open())
	}
}

// §2.9 punto 3, §2.12: approva → allow; nega → deny; testo → deny col testo come message; l'esito è
// consegnata con re del request.respond; una seconda risposta è superata.
func TestRispostaDallaCard(t *testing.T) {
	for _, c := range []struct{ answer, text, behavior, message string }{
		{protocol.AnswerApprove, "", "allow", ""},
		{protocol.AnswerDeny, "", "deny", "Negato dalla card di MonsterTerminal."},
		{protocol.AnswerText, "Spiega meglio", "deny", "Spiega meglio"},
	} {
		b := newBench(t, true)
		hook, ctx := b.open(t, "lavoro", "Bash", "ls")
		env := respond(b, ctx.RequestID, c.answer, c.text)
		if r := lastResult(t, b, env.ID); r.Outcome != protocol.OutcomeDelivered || r.RequestID != ctx.RequestID {
			t.Fatalf("%s: esito %+v", c.answer, r)
		}
		d, ok := readDecision(t, hook, time.Second)
		if !ok || d.Behavior != c.behavior || d.Message != c.message {
			t.Fatalf("%s: decisione %+v (%v)", c.answer, d, ok)
		}
		if _, ok := readLine(hook, 200*time.Millisecond); ok {
			t.Fatal("l'hook ha ricevuto più di una riga")
		}
		hook.Close()
		env2 := respond(b, ctx.RequestID, protocol.AnswerApprove, "")
		if r := lastResult(t, b, env2.ID); r.Outcome != protocol.OutcomeSuperseded {
			t.Fatalf("seconda risposta: %+v", r)
		}
		if b.rq.Open() != 0 {
			t.Fatal("richiesta ancora aperta dopo la risposta")
		}
	}
}

// §2.12: scaduta la finestra l'hook esce senza decisione e una risposta tardiva è superata.
func TestFinestraScaduta(t *testing.T) {
	b := newBench(t, true)
	b.probe = func(string) (bool, time.Time, bool) { return true, time.Now(), true } // finestra breve
	hook, ctx := b.open(t, "lavoro", "Bash", "ls")
	defer hook.Close()
	d, ok := readDecision(t, hook, 2*time.Second)
	if !ok || d.Behavior != "" {
		t.Fatalf("alla scadenza: %+v %v", d, ok)
	}
	env := respond(b, ctx.RequestID, protocol.AnswerApprove, "")
	if r := lastResult(t, b, env.ID); r.Outcome != protocol.OutcomeSuperseded {
		t.Fatalf("risposta tardiva: %+v", r)
	}
}

// §1.21 fatto 3 e §2.12: la risposta data nel terminale non uccide l'hook; è il cambio di stato della
// sessione (PostToolUse → lavora, Stop, SessionEnd) a chiudere la richiesta come superata.
func TestSuperataDalCambioDiStato(t *testing.T) {
	for _, ev := range []string{"PostToolUse", "Stop", "SessionEnd"} {
		b := newBench(t, true)
		hook, ctx := b.open(t, "lavoro", "Bash", "ls")
		if err := b.tr.HandleHook(HookMessage{Event: ev, Session: "lavoro"}); err != nil {
			t.Fatal(err)
		}
		d, ok := readDecision(t, hook, time.Second)
		if !ok || d.Behavior != "" {
			t.Fatalf("%s: l'hook doveva uscire senza decisione: %+v %v", ev, d, ok)
		}
		hook.Close()
		env := respond(b, ctx.RequestID, protocol.AnswerApprove, "")
		if r := lastResult(t, b, env.ID); r.Outcome != protocol.OutcomeSuperseded {
			t.Fatalf("%s: %+v", ev, r)
		}
	}
	// Un permesso su un'ALTRA sessione non chiude la richiesta.
	b := newBench(t, true)
	hook, _ := b.open(t, "lavoro", "Bash", "ls")
	defer hook.Close()
	_ = b.tr.HandleHook(HookMessage{Event: "Stop", Session: "altra"})
	if _, ok := readLine(hook, 200*time.Millisecond); ok || b.rq.Open() != 1 {
		t.Fatal("una sessione diversa ha chiuso la richiesta")
	}
}

// §2.12 «Una sola richiesta aperta per sessione»: un permesso nuovo chiude il precedente.
func TestUnaSolaRichiestaPerSessione(t *testing.T) {
	b := newBench(t, true)
	first, ctx1 := b.open(t, "lavoro", "Bash", "ls")
	second, ctx2 := b.open(t, "lavoro", "Write", `{"file_path":"/tmp/a"}`)
	defer second.Close()
	if d, ok := readDecision(t, first, time.Second); !ok || d.Behavior != "" {
		t.Fatalf("il primo hook doveva uscire: %+v %v", d, ok)
	}
	first.Close()
	if ctx1.RequestID == ctx2.RequestID || b.rq.Open() != 1 {
		t.Fatalf("richieste %s %s aperte %d", ctx1.RequestID, ctx2.RequestID, b.rq.Open())
	}
	env := respond(b, ctx1.RequestID, protocol.AnswerApprove, "")
	if r := lastResult(t, b, env.ID); r.Outcome != protocol.OutcomeSuperseded {
		t.Fatalf("la prima doveva essere superata: %+v", r)
	}
	evs := b.j.all()
	if len(evs) != 2 || evs[1].RequestID != ctx2.RequestID {
		t.Fatalf("giornale %+v: il secondo permesso è una transizione a sé", evs)
	}
}

// §2.12 «Il contesto si rimanda»: senza connessione la richiesta si apre lo stesso e il contesto parte
// ad Attach; una richiesta chiusa non si rimanda.
func TestRinvioDelContestoDopoWelcome(t *testing.T) {
	b := newBench(t, false)
	hook, conn := net.Pipe()
	defer hook.Close()
	go b.rq.HandlePermission(HookMessage{Event: "PermissionRequest", Session: "lavoro", Action: "Bash", Detail: "ls"}, conn)
	deadline := time.Now().Add(time.Second)
	for b.rq.Open() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("richiesta non aperta senza connessione")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(b.out.all()) != 0 {
		t.Fatal("inviato senza connessione")
	}
	b.rq.Attach(b.out)
	cs := b.out.ofType(protocol.TypeRequestContext)
	if len(cs) != 1 || cs[0].body.(protocol.RequestContext).Session != "lavoro" {
		t.Fatalf("rinvio %+v", cs)
	}
	b.rq.Detach()
	_ = b.tr.HandleHook(HookMessage{Event: "Stop", Session: "lavoro"})
	readLine(hook, time.Second)
	b.rq.Attach(b.out)
	if len(b.out.ofType(protocol.TypeRequestContext)) != 1 {
		t.Fatal("una richiesta chiusa è stata rimandata")
	}
}

// §1.5/§2.9: request.respond fuori schema (campo in più, answer sconosciuta, testo senza text) →
// error MESSAGE_INVALID con re; altri tipi non sono di questo gestore.
func TestRequestRespondNonValido(t *testing.T) {
	b := newBench(t, true)
	for _, body := range []string{
		`{"requestId":"` + protocol.NewUUID() + `","answer":"approva","extra":1}`,
		`{"requestId":"` + protocol.NewUUID() + `","answer":"forse"}`,
		`{"requestId":"` + protocol.NewUUID() + `","answer":"testo"}`,
		`{"requestId":"` + protocol.NewUUID() + `","answer":"approva","text":"no"}`,
		`{"requestId":"abc","answer":"approva"}`,
	} {
		env := protocol.Envelope{Type: protocol.TypeRequestRespond, ID: protocol.NewUUID(), TS: protocol.Timestamp(time.Now()), Body: json.RawMessage(body)}
		if !b.rq.Handle(env) {
			t.Fatalf("%s non gestito", body)
		}
		found := false
		for _, c := range b.out.ofType(protocol.TypeError) {
			if c.re == env.ID && c.body.(protocol.ErrorBody).Code == protocol.CodeMessageInvalid {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: nessun MESSAGE_INVALID", body)
		}
	}
	if b.rq.Handle(protocol.Envelope{Type: protocol.TypeSessionOpen}) {
		t.Fatal("session.open non è del gestore delle richieste")
	}
}

// §2.12: il contesto oltre 16 KiB è troncato con avviso, mai spezzando una runa.
func TestContestoTroncato(t *testing.T) {
	long := strings.Repeat("à", 9000) // 18 000 byte
	s, cut := protocol.TruncateDetail(long)
	if !cut || len(s) > protocol.MaxRequestDetail || !strings.HasSuffix(s, "à") || len(s)%2 != 0 {
		t.Fatalf("troncamento: %d byte, cut=%v", len(s), cut)
	}
	if s2, cut2 := protocol.TruncateDetail("corto"); cut2 || s2 != "corto" {
		t.Fatal("un testo corto non si tocca")
	}
	b := newBench(t, true)
	hook, ctx := b.open(t, "lavoro", "Bash", long)
	defer hook.Close()
	if len(ctx.Detail) > protocol.MaxRequestDetail {
		t.Fatalf("contesto di %d byte oltre il tetto", len(ctx.Detail))
	}
}

// Il socket vero: Ask aspetta sulla stessa connessione la decisione; Listen continua a servire gli altri
// hook mentre un permesso è in attesa; senza connettore Ask fallisce subito.
func TestAskSulSocket(t *testing.T) {
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
	type res struct {
		d   Decision
		err error
	}
	got := make(chan res, 1)
	go func() {
		d, err := Ask(context.Background(), sock, HookMessage{Event: "PermissionRequest", Session: "lavoro", Action: "Bash", Detail: "ls"}, time.Second, 5*time.Second)
		got <- res{d, err}
	}()
	var rc protocol.RequestContext
	deadline = time.Now().Add(2 * time.Second)
	for len(b.out.ofType(protocol.TypeRequestContext)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("contesto non inviato")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rc = b.out.ofType(protocol.TypeRequestContext)[0].body.(protocol.RequestContext)
	// Un altro hook sulla stessa connessione del socket viene servito mentre il permesso aspetta.
	if err := Send(sock, HookMessage{Event: "UserPromptSubmit", Session: "altra"}, time.Second); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for len(b.j.all()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("il socket è bloccato dal permesso in attesa")
		}
		time.Sleep(5 * time.Millisecond)
	}
	respond(b, rc.RequestID, protocol.AnswerApprove, "")
	select {
	case r := <-got:
		if r.err != nil || r.d.Behavior != "allow" {
			t.Fatalf("Ask: %+v %v", r.d, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask non è tornato")
	}
	cancel()
	<-done
	if _, err := Ask(context.Background(), sock, HookMessage{Event: "PermissionRequest", Session: "x", Action: "Bash"}, 200*time.Millisecond, time.Second); err == nil {
		t.Fatal("Ask senza connettore riuscito")
	}
}

// §1.21: la Notification permission_prompt che segue il PermissionRequest (stessa attesa, senza
// requestId) non chiude la richiesta e non scrive un secondo agent.state (prova dal vivo 2026-10-18:
// la richiesta risultava «superata» sei secondi dopo l'apertura e l'allow della card non arrivava).
func TestLaNotificaDiPermessoNonSuperaLaRichiesta(t *testing.T) {
	b := newBench(t, true)
	hook, ctx := b.open(t, "lavoro", "Bash", "ls")
	defer hook.Close()
	if err := b.tr.HandleHook(HookMessage{Event: "Notification", Session: "lavoro", NotificationType: "permission_prompt"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readLine(hook, 300*time.Millisecond); ok {
		t.Fatal("la notifica di permesso ha chiuso la richiesta")
	}
	if n := len(b.j.all()); n != 1 {
		t.Fatalf("agent.state nel giornale: %d, atteso 1 (nessun doppione)", n)
	}
	env := respond(b, ctx.RequestID, protocol.AnswerApprove, "")
	if r := lastResult(t, b, env.ID); r.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("la risposta dopo la notifica doveva essere consegnata: %+v", r)
	}
	if d, ok := readDecision(t, hook, time.Second); !ok || d.Behavior != "allow" {
		t.Fatalf("decisione %+v %v", d, ok)
	}
	// Un'attesa «sconosciuta» (idle_prompt) è invece un'attesa diversa: la richiesta si chiude.
	b2 := newBench(t, true)
	hook2, _ := b2.open(t, "lavoro", "Bash", "ls")
	defer hook2.Close()
	_ = b2.tr.HandleHook(HookMessage{Event: "Notification", Session: "lavoro", NotificationType: "idle_prompt"})
	if d, ok := readDecision(t, hook2, time.Second); !ok || d.Behavior != "" {
		t.Fatalf("idle_prompt doveva chiudere la richiesta senza decisione: %+v %v", d, ok)
	}
}
