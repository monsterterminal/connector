// Le domande dell'agente nel comando hook (spec monsterterminal-saas.md §2.14 e §1.21, fetta 3b):
// l'adattatore riconosce AskUserQuestion nelle forme vere di Claude Code (banco del 2026-10-19,
// E4-E7), porta le domande fino al backend, e la risposta dalla card torna a Claude Code come
// `decision: {behavior: allow, updatedInput: tool_input + answers}`. Corrispondenza (regola #26): la
// forma di updatedInput è quella misurata sul banco; l'Esc (SIGTERM) diventa request.closed.
package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/agents"
	"github.com/monsterterminal/connector/internal/protocol"
)

// toolInputE4 è il tool_input di AskUserQuestion come lo manda Claude Code 2.1.303 (banco E4).
const toolInputE4 = `{"questions":[{"question":"Quale frutto?","header":"Frutto","options":[{"label":"Mela","description":"rossa"},{"label":"Pera","description":"verde"}],"multiSelect":false}]}`

// La riga esatta che l'hook scrive per AskUserQuestion dopo la risposta «Mela» dalla card (§2.14 fatto 3).
const expectedLineE4 = `{"hookSpecificOutput":{"decision":{"behavior":"allow","updatedInput":{"answers":{"Quale frutto?":"Mela"},"questions":[{"header":"Frutto","multiSelect":false,"options":[{"description":"rossa","label":"Mela"},{"description":"verde","label":"Pera"}],"question":"Quale frutto?"}]}},"hookEventName":"PermissionRequest"}}` + "\n"

func (h *harness) hookCtx(ctx context.Context, event, pane, stdin string) (int, string) {
	h.env["TMUX_PANE"] = pane
	a := h.app()
	a.stdin = strings.NewReader(stdin)
	out := &syncBuf{}
	a.stdout = out
	code := a.run(ctx, []string{"hook", "-state-dir", h.stateDir, event})
	return code, out.String()
}

// askQuestions nelle forme del banco: header mancante, multiSelect, quattro domande, campi in più
// dello strumento; input non decodificabile → nessuna domanda.
func TestDecodificaDelleDomandeDiClaudeCode(t *testing.T) {
	qs := askQuestions(json.RawMessage(toolInputE4))
	if len(qs) != 1 || qs[0].Question != "Quale frutto?" || qs[0].Header != "Frutto" || len(qs[0].Options) != 2 ||
		qs[0].Options[0] != (protocol.Option{Label: "Mela", Description: "rossa"}) || qs[0].MultiSelect {
		t.Fatalf("E4: %+v", qs)
	}
	e7 := `{"questions":[{"question":"Quale frutto?","options":[{"label":"Mela"},{"label":"Pera"}],"multiSelect":false},{"question":"Quali colori?","header":"Colori","options":[{"label":"Rosso","description":"come la mela"},{"label":"Verde"}],"multiSelect":true}],"extra":"ignorato"}`
	qs = askQuestions(json.RawMessage(e7))
	if len(qs) != 2 || qs[0].Header != "" || !qs[1].MultiSelect || qs[1].Options[0].Description != "come la mela" {
		t.Fatalf("E7: %+v", qs)
	}
	four := `{"questions":[` + strings.Repeat(`{"question":"q","options":[{"label":"a"}],"multiSelect":false},`, 4)
	if qs = askQuestions(json.RawMessage(four[:len(four)-1] + `]}`)); len(qs) != 4 {
		t.Fatalf("quattro domande: %d", len(qs))
	}
	for _, bad := range []string{``, `non json`, `{"command":"ls"}`, `{"questions":"x"}`} {
		if qs := askQuestions(json.RawMessage(bad)); qs != nil {
			t.Errorf("%q: %+v", bad, qs)
		}
	}
	// detail: la prima domanda, più «(+N domande)».
	if d := questionDetail(askQuestions(json.RawMessage(e7))); d != "Quale frutto? (+1 domande)" {
		t.Fatalf("detail %q", d)
	}
	if d := questionDetail(nil); d != "" {
		t.Fatalf("detail senza domande %q", d)
	}
	if d := questionDetail(askQuestions(json.RawMessage(toolInputE4))); d != "Quale frutto?" {
		t.Fatalf("detail di una domanda %q", d)
	}
}

// questionDecision: updatedInput = tool_input con tutti i campi + answers; chiave = testo originale
// della domanda anche se nel contesto era troncata; senza risposte o senza allow nessuna decisione.
func TestDecisionePerLaDomanda(t *testing.T) {
	qs := askQuestions(json.RawMessage(toolInputE4))
	d := questionDecision(agents.Decision{Behavior: "allow", Answers: []protocol.Answer{{Question: "Quale frutto?", Text: "Mela"}}}, json.RawMessage(toolInputE4), qs)
	raw, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": d}})
	if string(raw)+"\n" != expectedLineE4 {
		t.Fatalf("riga:\n%s\nattesa:\n%s", raw, expectedLineE4)
	}
	// Domanda lunga: il connettore risponde col testo troncato, la chiave resta l'originale.
	long := strings.Repeat("d", 300)
	in := json.RawMessage(`{"questions":[{"question":"` + long + `","options":[{"label":"sì"}],"multiSelect":false}],"nota":1}`)
	d = questionDecision(agents.Decision{Behavior: "allow", Answers: []protocol.Answer{{Question: long[:200], Text: "sì"}}}, in, askQuestions(in))
	up := d["updatedInput"].(map[string]any)
	if up["answers"].(map[string]string)[long] != "sì" || up["nota"] != float64(1) || up["questions"] == nil {
		t.Fatalf("updatedInput %+v", up)
	}
	for name, dec := range map[string]agents.Decision{
		"senza risposte": {Behavior: "allow"},
		"deny":           {Behavior: "deny", Message: "no", Answers: []protocol.Answer{{Question: "q", Text: "t"}}},
		"vuota":          {},
	} {
		if d := questionDecision(dec, json.RawMessage(toolInputE4), qs); d != nil {
			t.Errorf("%s: decisione %+v", name, d)
		}
	}
	if d := questionDecision(agents.Decision{Behavior: "allow", Answers: []protocol.Answer{{Question: "q", Text: "t"}}}, json.RawMessage(`non json`), nil); d != nil {
		t.Fatal("tool_input non decodificabile accettato")
	}
	// Per i permessi la decisione resta quella della fetta 2.
	if d := permissionDecision(agents.Decision{Behavior: "deny", Message: "no"}); d["message"] != "no" || d["updatedInput"] != nil {
		t.Fatalf("permesso: %+v", d)
	}
}

func startWithHooks(t *testing.T) (*harness, string, func()) {
	t.Helper()
	h := newHarness(t)
	conClaude(t, h, true)
	h.panes = map[string]string{"%3": "lavoro"}
	h.hooks(t, "install", "-yes")
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	id := hostIDFromStdout(t, h.stdout.String())
	done, stop := h.start(t)
	waitHello(t, h.s)
	eventually(t, "socket degli hook", func() bool { _, err := os.Stat(hookSocket(h.stateDir)); return err == nil })
	return h, id, func() { stop(); waitExit(t, done) }
}

// Dal comando hook alla card e ritorno (§2.14): request.context con kind domanda e questions,
// attende-input/domanda col requestId nel giornale (mai il testo), request.respond con answers →
// consegnata e la riga esatta dell'hook con updatedInput.
func TestHookDiDomandaFinoAllaCard(t *testing.T) {
	h, id, stop := startWithHooks(t)
	defer stop()
	type res struct {
		code int
		out  string
	}
	got := make(chan res, 1)
	go func() {
		code, out := h.hook("PermissionRequest", "%3", `{"session_id":"abc","hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion","tool_input":`+toolInputE4+`}`)
		got <- res{code, out}
	}()
	eventually(t, "request.context al backend", func() bool { return len(h.s.Contexts(id)) == 1 })
	ctx := h.s.Contexts(id)[0]
	if ctx.Kind != protocol.KindQuestion || ctx.Action != "AskUserQuestion" || ctx.Detail != "Quale frutto?" || ctx.Deadline == "" ||
		len(ctx.Questions) != 1 || ctx.Questions[0].Header != "Frutto" || len(ctx.Questions[0].Options) != 2 {
		t.Fatalf("contesto %+v", ctx)
	}
	eventually(t, "agent.state attende-input/domanda col requestId", func() bool {
		st := stati(h, id)
		return len(st) == 1 && st[0].RequestID == ctx.RequestID && st[0].WaitingFor == protocol.WaitQuestion
	})
	for _, ev := range h.s.Events(id) {
		if strings.Contains(strings.ToLower(string(ev.Data)), "frutto") {
			t.Fatalf("testo della domanda nel giornale: %s", ev.Data)
		}
	}
	// La Notification di 6 s dopo non chiude la domanda.
	h.hook("Notification", "%3", `{"session_id":"abc","notification_type":"permission_prompt"}`)
	select {
	case r := <-got:
		t.Fatalf("l'hook è uscito prima della risposta: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if n := len(stati(h, id)); n != 1 {
		t.Fatalf("agent.state dopo la notifica: %d", n)
	}
	rid, err := h.s.SendControl(id, protocol.TypeRequestRespond, protocol.RequestRespond{RequestID: ctx.RequestID, Answer: protocol.AnswerText, Text: "Mela", Answers: []protocol.Answer{{Question: "Quale frutto?", Text: "Mela"}}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "request.respond.result", func() bool { _, ok := h.s.Reply(id, rid); return ok })
	reply, _ := h.s.Reply(id, rid)
	var result protocol.RequestRespondResult
	_ = json.Unmarshal(reply.Body, &result)
	if reply.Type != protocol.TypeRequestRespondResult || result.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("esito %s %+v", reply.Type, result)
	}
	select {
	case r := <-got:
		if r.code != exitOK || r.out != expectedLineE4 {
			t.Fatalf("hook: uscita %d, stdout\n%s\natteso\n%s", r.code, r.out, expectedLineE4)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("l'hook non è uscito dopo la risposta")
	}
	if n := len(h.s.RequestsClosed(id)); n != 0 {
		t.Fatalf("request.closed dopo una risposta consegnata: %d", n)
	}
}

// §2.14 «Annullata nel terminale»: il SIGTERM (qui la fine del contesto del comando) fa uscire l'hook
// subito, senza riga, e il connettore manda request.closed {requestId, annullata}; una risposta
// arrivata dopo è superata.
func TestHookDiDomandaAnnullata(t *testing.T) {
	h, id, stop := startWithHooks(t)
	defer stop()
	hctx, sigterm := context.WithCancel(context.Background())
	defer sigterm()
	type res struct {
		code int
		out  string
	}
	got := make(chan res, 1)
	go func() {
		code, out := h.hookCtx(hctx, "PermissionRequest", "%3", `{"tool_name":"AskUserQuestion","tool_input":`+toolInputE4+`}`)
		got <- res{code, out}
	}()
	eventually(t, "request.context al backend", func() bool { return len(h.s.Contexts(id)) == 1 })
	ctx := h.s.Contexts(id)[0]
	began := time.Now()
	sigterm()
	select {
	case r := <-got:
		if r.code != exitOK || r.out != "" || time.Since(began) > time.Second {
			t.Fatalf("hook annullato: uscita %d, stdout %q, in %v", r.code, r.out, time.Since(began))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("l'hook non è uscito al SIGTERM")
	}
	eventually(t, "request.closed al backend", func() bool { return len(h.s.RequestsClosed(id)) == 1 })
	if c := h.s.RequestsClosed(id)[0]; c.RequestID != ctx.RequestID || c.Reason != protocol.ReasonCancelled {
		t.Fatalf("request.closed %+v", c)
	}
	rid, _ := h.s.SendControl(id, protocol.TypeRequestRespond, protocol.RequestRespond{RequestID: ctx.RequestID, Answer: protocol.AnswerText, Text: "Mela", Answers: []protocol.Answer{{Question: "Quale frutto?", Text: "Mela"}}})
	eventually(t, "esito", func() bool { _, ok := h.s.Reply(id, rid); return ok })
	reply, _ := h.s.Reply(id, rid)
	var result protocol.RequestRespondResult
	_ = json.Unmarshal(reply.Body, &result)
	if result.Outcome != protocol.OutcomeSuperseded {
		t.Fatalf("risposta dopo l'annullata: %+v", result)
	}
	// approva su una domanda: non-supportata; il permesso di fetta 2 resta invariato.
	go func() {
		code, out := h.hook("PermissionRequest", "%3", `{"tool_name":"AskUserQuestion","tool_input":`+toolInputE4+`}`)
		got <- res{code, out}
	}()
	eventually(t, "seconda domanda", func() bool { return len(h.s.Contexts(id)) == 2 })
	ctx2 := h.s.Contexts(id)[1]
	rid2, _ := h.s.SendControl(id, protocol.TypeRequestRespond, protocol.RequestRespond{RequestID: ctx2.RequestID, Answer: protocol.AnswerApprove})
	eventually(t, "esito approva", func() bool { _, ok := h.s.Reply(id, rid2); return ok })
	reply2, _ := h.s.Reply(id, rid2)
	_ = json.Unmarshal(reply2.Body, &result)
	if result.Outcome != protocol.OutcomeUnsupported {
		t.Fatalf("approva su una domanda: %+v", result)
	}
	rid3, _ := h.s.SendControl(id, protocol.TypeRequestRespond, protocol.RequestRespond{RequestID: ctx2.RequestID, Answer: protocol.AnswerText, Text: "Pera"})
	eventually(t, "esito testo", func() bool { _, ok := h.s.Reply(id, rid3); return ok })
	select {
	case r := <-got:
		if r.code != exitOK || !strings.Contains(r.out, `"answers":{"Quale frutto?":"Pera"}`) {
			t.Fatalf("testo sulla domanda sola: %d %q", r.code, r.out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("l'hook non è uscito dopo il testo")
	}
}
