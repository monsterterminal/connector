package catalog

// Test dell'installatore (spec §1.9, ADR 0410 A4). Asse A: un'installazione intera con progresso,
// esito, file di stato, evento catalog.installed e rilevamento; busy; esiti rimandati dopo una
// caduta; messaggio non valido. Asse B: ogni messaggio e ogni evento prodotti dal codice rispettano
// gli schemi 0.11.0, e l'enum dei motivi del codice è quello dello schema.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

type sent struct {
	typ, re string
	body    []byte
}

type fakeOut struct {
	mu   sync.Mutex
	msgs []sent
	fail bool
}

func (o *fakeOut) Control(typ, re string, body any) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail {
		return errors.New("connessione caduta")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	o.msgs = append(o.msgs, sent{typ, re, raw})
	return nil
}

func (o *fakeOut) all() []sent {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]sent(nil), o.msgs...)
}

type fakeJournal struct {
	mu     sync.Mutex
	events []protocol.Event
}

func (j *fakeJournal) Append(kind string, data any) (protocol.Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return protocol.Event{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	ev := protocol.Event{Seq: int64(len(j.events) + 1), Kind: kind, At: protocol.Timestamp(time.Now()), Data: raw}
	j.events = append(j.events, ev)
	return ev, nil
}

type installHarness struct {
	k       keyPair
	in      *Installer
	out     *fakeOut
	journal *fakeJournal
	state   string
	detects int
	mu      sync.Mutex
}

func newInstallHarness(t *testing.T) *installHarness {
	t.Helper()
	h := &installHarness{k: newKey(t), out: &fakeOut{}, journal: &fakeJournal{}, state: t.TempDir()}
	h.in = New(Config{
		StateDir: h.state, Home: t.TempDir(), CatalogInstall: true, Key: h.k.pub,
		Platform: HostPlatform{OS: "linux", Arch: "amd64", Distro: "ubuntu"},
		Journal:  h.journal,
		Detect: func() []protocol.Agent {
			h.mu.Lock()
			h.detects++
			h.mu.Unlock()
			return []protocol.Agent{{Agent: "claude-code", Version: "2.1.332"}, {Agent: "opencode", Version: "2.3.0"}}
		},
		LookupVersion: func(string) (string, bool) { return "22.3.0", true },
	})
	h.in.Attach(h.out)
	return h
}

func (h *installHarness) request(t *testing.T, r Recipe) protocol.CatalogInstall {
	t.Helper()
	raw := canon(t, r)
	return protocol.CatalogInstall{InstallID: protocol.NewUUID(), Recipe: raw, Signature: h.k.sign(raw),
		ApprovedBy: protocol.NewUUID(), ApprovedAt: "2026-11-26T10:00:00.000Z"}
}

func okRecipe(steps ...Step) Recipe {
	r := withNode(testRecipe(), "22.0.0")
	r.Steps = steps
	return r
}

func TestInstallRiuscitaAggiornaStatoEventoERilevamento(t *testing.T) {
	h := newInstallHarness(t)
	ci := h.request(t, okRecipe(runStep(5, "/bin/true"), runStep(5, "/bin/true"), runStep(5, "/bin/true")))
	res := h.in.Install(context.Background(), "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b", ci)
	if res.Outcome != protocol.CatalogOK || res.Reason != "" || res.StepIndex != nil {
		t.Fatalf("esito: %+v", res)
	}
	msgs := h.out.all()
	if len(msgs) != 4 {
		t.Fatalf("attesi 3 progressi e un esito, ricevuti %d", len(msgs))
	}
	for i, m := range msgs[:3] {
		var p protocol.CatalogInstallProgress
		_ = json.Unmarshal(m.body, &p)
		if m.typ != protocol.TypeCatalogInstallProgress || p.Step != i+1 || p.Of != 3 || p.InstallID != ci.InstallID || m.re != "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b" {
			t.Fatalf("progresso %d: %s %+v", i, m.typ, p)
		}
	}
	if msgs[3].typ != protocol.TypeCatalogInstallResult {
		t.Fatalf("ultimo messaggio %s", msgs[3].typ)
	}
	s, ok, err := ReadState(h.state, "opencode")
	if err != nil || !ok || s.RecipeVersion != 2 || s.InstallID != ci.InstallID {
		t.Fatalf("file di stato: %+v %v %v", s, ok, err)
	}
	if len(h.journal.events) != 1 || h.journal.events[0].Kind != protocol.KindCatalogInstalled {
		t.Fatalf("giornale: %+v", h.journal.events)
	}
	var ev protocol.CatalogInstalled
	_ = json.Unmarshal(h.journal.events[0].Data, &ev)
	if ev != (protocol.CatalogInstalled{RecipeID: "opencode", RecipeVersion: 2, Agent: "opencode", Version: "2.3.0"}) {
		t.Fatalf("catalog.installed: %+v", ev)
	}
	if h.detects != 1 {
		t.Fatalf("rilevamento rifatto %d volte, atteso 1", h.detects)
	}
	// La stessa ricetta di nuovo: downgrade dal file di stato, nessun passo, nessun evento.
	again := h.in.Install(context.Background(), "", h.request(t, okRecipe(runStep(5, "/bin/true"))))
	if again.Outcome != protocol.CatalogRefused || again.Reason != protocol.ReasonDowngrade {
		t.Fatalf("seconda installazione: %+v", again)
	}
	if len(h.journal.events) != 1 || h.detects != 1 {
		t.Fatal("un rifiuto ha scritto nel giornale o rifatto il rilevamento")
	}
}

func TestInstallFallitaNonScriveStato(t *testing.T) {
	h := newInstallHarness(t)
	res := h.in.Install(context.Background(), "", h.request(t, okRecipe(runStep(5, "/bin/true"), runStep(5, "/bin/sh", "-c", "echo male; exit 2"))))
	if res.Outcome != protocol.CatalogFailed || res.Reason != protocol.ReasonStepFailed || res.StepIndex == nil || *res.StepIndex != 1 || !strings.Contains(res.LogTail, "male") {
		t.Fatalf("esito: %+v", res)
	}
	if _, ok, _ := ReadState(h.state, "opencode"); ok {
		t.Fatal("file di stato scritto dopo un fallimento")
	}
	if len(h.journal.events) != 0 || h.detects != 0 {
		t.Fatal("evento o rilevamento dopo un fallimento")
	}
	// Il turno è stato rilasciato: un'altra installazione parte.
	if r := h.in.Install(context.Background(), "", h.request(t, okRecipe(runStep(5, "/bin/true")))); r.Outcome != protocol.CatalogOK {
		t.Fatalf("dopo un fallimento: %+v", r)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.state, Subdir, "work")); len(entries) != 0 {
		t.Fatalf("cartelle di lavoro rimaste: %v", entries)
	}
}

// Un'installazione alla volta per host: la seconda, mentre la prima gira, è busy.
func TestInstallUnaAllaVolta(t *testing.T) {
	h := newInstallHarness(t)
	release := filepath.Join(t.TempDir(), "via")
	first := h.request(t, okRecipe(runStep(30, "/bin/sh", "-c", `while [ ! -e "$0" ]; do sleep 0.02; done`, release)))
	done := make(chan protocol.CatalogInstallResult, 1)
	go func() { done <- h.in.Install(context.Background(), "", first) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.in.mu.Lock()
		busy := h.in.busy
		h.in.mu.Unlock()
		if busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("la prima installazione non è partita")
		}
		time.Sleep(5 * time.Millisecond)
	}
	second := h.in.Install(context.Background(), "", h.request(t, okRecipe(runStep(5, "/bin/true"))))
	if second.Outcome != protocol.CatalogRefused || second.Reason != protocol.ReasonBusy {
		t.Fatalf("seconda installazione: %+v", second)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Outcome != protocol.CatalogOK {
		t.Fatalf("prima installazione: %+v", r)
	}
}

// Un esito che non parte (connessione caduta) si rimanda al collegamento successivo.
func TestInstallEsitoRimandatoDopoLaCaduta(t *testing.T) {
	h := newInstallHarness(t)
	h.in.Detach()
	res := h.in.Install(context.Background(), "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b", h.request(t, okRecipe(runStep(5, "/bin/true"))))
	if res.Outcome != protocol.CatalogOK || len(h.out.all()) != 0 {
		t.Fatalf("esito %+v, messaggi %d", res, len(h.out.all()))
	}
	h.in.Attach(h.out)
	msgs := h.out.all()
	if len(msgs) != 1 || msgs[0].typ != protocol.TypeCatalogInstallResult || msgs[0].re != "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b" {
		t.Fatalf("dopo il ricollegamento: %+v", msgs)
	}
	h.in.Attach(h.out)
	if len(h.out.all()) != 1 {
		t.Fatal("esito rimandato due volte")
	}
}

func TestHandleCatalogInstall(t *testing.T) {
	h := newInstallHarness(t)
	if h.in.Handle(protocol.Envelope{Type: protocol.TypeUpdateNone, Body: []byte(`{}`)}) {
		t.Fatal("ha preso un tipo non suo")
	}
	// Corpo non valido: MESSAGE_INVALID con re, nessuna installazione.
	if !h.in.Handle(protocol.Envelope{Type: protocol.TypeCatalogInstall, ID: "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b", Body: []byte(`{"installId":"x"}`)}) {
		t.Fatal("catalog.install non preso")
	}
	msgs := h.out.all()
	if len(msgs) != 1 || msgs[0].typ != protocol.TypeError || !strings.Contains(string(msgs[0].body), protocol.CodeMessageInvalid) {
		t.Fatalf("risposta al corpo non valido: %+v", msgs)
	}
	// Corpo valido: l'installazione gira in sfondo e risponde con re.
	ci := h.request(t, okRecipe(runStep(5, "/bin/true")))
	body, _ := json.Marshal(ci)
	id := protocol.NewUUID()
	h.in.Handle(protocol.Envelope{Type: protocol.TypeCatalogInstall, ID: id, Body: body})
	h.in.Wait()
	msgs = h.out.all()
	if last := msgs[len(msgs)-1]; last.typ != protocol.TypeCatalogInstallResult || last.re != id {
		t.Fatalf("esito: %+v", last)
	}
}

func TestInstallInterruttoreSpentoNonLeggeNemmenoLaRicetta(t *testing.T) {
	h := newInstallHarness(t)
	h.in.cfg.CatalogInstall = false
	ci := h.request(t, okRecipe(runStep(5, "/bin/true")))
	ci.Recipe = []byte(`{"qualunque":"cosa"}`)
	res := h.in.Install(context.Background(), "", ci)
	if res.Outcome != protocol.CatalogRefused || res.Reason != protocol.ReasonDisabledOnHost {
		t.Fatalf("esito: %+v", res)
	}
}

// --- Asse B ---------------------------------------------------------------------------------------

// TestMotiviUgualiAlloSchema — [#26 asse B] spec §1.9, ADR 0410 A4: l'enum dei motivi, gli esiti e i
// tetti del codice sono quelli di catalog.install.result.json 0.11.0; i motivi di failed sono i tre
// della descrizione dello schema.
func TestMotiviUgualiAlloSchema(t *testing.T) {
	v := newValidator(t)
	props := v.load("messages/connector-to-backend/catalog.install.result.json")["properties"].(map[string]any)["body"].(map[string]any)["properties"].(map[string]any)
	if fmt.Sprint(props["reason"].(map[string]any)["enum"]) != fmt.Sprint(protocol.CatalogReasons) {
		t.Errorf("motivi: schema %v, codice %v", props["reason"].(map[string]any)["enum"], protocol.CatalogReasons)
	}
	if fmt.Sprint(props["outcome"].(map[string]any)["enum"]) != fmt.Sprint(protocol.CatalogOutcomes) {
		t.Errorf("esiti: schema %v, codice %v", props["outcome"], protocol.CatalogOutcomes)
	}
	if !strings.Contains(props["reason"].(map[string]any)["description"].(string), "Con failed: "+strings.Join(protocol.FailedReasons, ", ")+".") {
		t.Errorf("motivi di failed diversi dalla descrizione dello schema: %v", protocol.FailedReasons)
	}
	if int(props["detail"].(map[string]any)["maxLength"].(float64)) != protocol.MaxCatalogDetail ||
		int(props["logTail"].(map[string]any)["maxLength"].(float64)) != protocol.MaxCatalogLogTail ||
		int(props["stepIndex"].(map[string]any)["maximum"].(float64)) != protocol.MaxCatalogSteps-1 {
		t.Error("tetti di catalog.install.result: schema e codice divergono")
	}
	pp := v.load("messages/connector-to-backend/catalog.install.progress.json")["properties"].(map[string]any)["body"].(map[string]any)["properties"].(map[string]any)
	if int(pp["message"].(map[string]any)["maxLength"].(float64)) != protocol.MaxCatalogProgressMessage {
		t.Error("tetto di progress.message: schema e codice divergono")
	}
	// Le strutture Go hanno i campi dello schema, opzionali compresi.
	for rel, val := range map[string]any{
		"messages/connector-to-backend/catalog.install.result.json":   protocol.CatalogInstallResult{Reason: "x", Detail: "x", StepIndex: new(int), LogTail: "x"},
		"messages/connector-to-backend/catalog.install.progress.json": protocol.CatalogInstallProgress{},
		"messages/backend-to-connector/catalog.install.json":          protocol.CatalogInstall{Recipe: []byte(`{}`)},
	} {
		body := v.load(rel)["properties"].(map[string]any)["body"].(map[string]any)["properties"].(map[string]any)
		if got, want := jsonKeys(t, val), mapKeys(body); got != want {
			t.Errorf("%s: codice %s, schema %s", rel, got, want)
		}
	}
	if got, want := jsonKeys(t, protocol.CatalogInstalled{}), mapKeys(v.load("events/catalog.installed.json")["properties"].(map[string]any)); got != want {
		t.Errorf("catalog.installed: codice %s, schema %s", got, want)
	}
}

// TestRicettaUgualeAlloSchema — [#26 asse B] objects/recipe.json 0.11.0: i campi della ricetta, del
// passo e di fetch, gli enum e la regola di dest sono quelli dello schema.
func TestRicettaUgualeAlloSchema(t *testing.T) {
	v := newValidator(t)
	rs := v.load("objects/recipe.json")
	props := rs["properties"].(map[string]any)
	full := testRecipe()
	full.Requires = []Require{{MinVersion: "1.0.0"}}
	if got, want := jsonKeys(t, full), mapKeys(props); got != want {
		t.Errorf("ricetta: codice %s, schema %s", got, want)
	}
	branches := props["steps"].(map[string]any)["items"].(map[string]any)["oneOf"].([]any)
	runProps := branches[0].(map[string]any)["properties"].(map[string]any)
	fetchProps := branches[1].(map[string]any)["properties"].(map[string]any)
	if got, want := jsonKeys(t, Step{Run: []string{"x"}}), mapKeys(runProps); got != want {
		t.Errorf("passo run: codice %s, schema %s", got, want)
	}
	if got, want := jsonKeys(t, Step{Fetch: &Fetch{}}), mapKeys(fetchProps); got != want {
		t.Errorf("passo fetch: codice %s, schema %s", got, want)
	}
	fp := fetchProps["fetch"].(map[string]any)["properties"].(map[string]any)
	if got, want := jsonKeys(t, Fetch{Executable: true}), mapKeys(fp); got != want {
		t.Errorf("fetch: codice %s, schema %s", got, want)
	}
	if fp["dest"].(map[string]any)["pattern"] != DestRE.String() {
		t.Errorf("regola di dest: schema %v, codice %s", fp["dest"], DestRE)
	}
	if !strings.Contains(branches[1].(map[string]any)["description"].(string), "256 MiB") || MaxFetchBytes != 256<<20 {
		t.Error("tetto di scarico: schema e codice divergono")
	}
	if int(props["steps"].(map[string]any)["maxItems"].(float64)) != protocol.MaxCatalogSteps {
		t.Error("numero massimo di passi: schema e codice divergono")
	}
	common := v.load("common.json")["$defs"].(map[string]any)
	enums := map[string][2]any{
		"source.channel": {props["source"].(map[string]any)["properties"].(map[string]any)["channel"].(map[string]any)["enum"], SourceChannels},
		"adapter.kind":   {props["adapter"].(map[string]any)["properties"].(map[string]any)["kind"].(map[string]any)["enum"], AdapterKinds},
		"os":             {common["os"].(map[string]any)["enum"], OSes},
		"arch":           {common["arch"].(map[string]any)["enum"], Arches},
		"distro":         {common["distro"].(map[string]any)["enum"], Distros},
		"requires.check": {props["requires"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["check"].(map[string]any)["enum"], RequireChecks},
	}
	for name, e := range enums {
		if fmt.Sprint(e[0]) != fmt.Sprint(e[1]) {
			t.Errorf("%s: schema %v, codice %v", name, e[0], e[1])
		}
	}
}

// TestMessaggiProdottiRispettanoGliSchemi — [#26 asse B] spec §1.9, schemi 0.11.0: progresso, esiti
// (ok, refused, failed con ogni motivo) ed evento catalog.installed prodotti dall'installatore, nella
// loro busta, passano il validatore degli schemi.
func TestMessaggiProdottiRispettanoGliSchemi(t *testing.T) {
	v := newValidator(t)
	h := newInstallHarness(t)
	srv := fetchServer(t, []byte("artefatto"))
	h.in.cfg.HTTPClient = fetchClient(srv)
	h.in.cfg.TimeoutUnit = 100 * time.Millisecond
	// ok con progresso
	h.in.Install(context.Background(), "", h.request(t, okRecipe(runStep(5, "/bin/true"), runStep(5, "/bin/true"))))
	// failed: step-failed con logTail lungo, timeout, checksum-mismatch
	r := okRecipe(runStep(50, "/bin/sh", "-c", "head -c 20000 /dev/zero | tr '\\0' é; exit 1"))
	r.RecipeVersion = 3
	h.in.Install(context.Background(), "", h.request(t, r))
	r = okRecipe(runStep(2, "/bin/sleep", "30"))
	r.RecipeVersion = 3
	h.in.Install(context.Background(), "", h.request(t, r))
	r = okRecipe(Step{Fetch: &Fetch{URL: srv.URL + "/v1/tool", SHA256: sum([]byte("altro")), Dest: "tool"}, TimeoutSeconds: 50})
	r.RecipeVersion = 3
	h.in.Install(context.Background(), "", h.request(t, r))
	// refused: downgrade e requires-failed con detail
	h.in.Install(context.Background(), "", h.request(t, okRecipe(runStep(5, "/bin/true"))))
	h.in.cfg.LookupVersion = func(string) (string, bool) { return "18.19.0", true }
	h.in.Install(context.Background(), "", h.request(t, okRecipe(runStep(5, "/bin/true"))))

	reasons := map[string]bool{}
	for _, m := range h.out.all() {
		frame, _, err := protocol.Encode(m.typ, m.re, json.RawMessage(m.body), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		rel := "messages/connector-to-backend/" + m.typ + ".json"
		if err := v.validateJSON(rel, frame); err != nil {
			t.Errorf("%s fuori schema: %v\n%s", m.typ, err, m.body)
		}
		if m.typ == protocol.TypeCatalogInstallResult {
			var res protocol.CatalogInstallResult
			_ = json.Unmarshal(m.body, &res)
			reasons[res.Outcome+"/"+res.Reason] = true
			if IsFailedReason(res.Reason) != (res.Outcome == protocol.CatalogFailed) || (res.StepIndex != nil) != (res.Outcome == protocol.CatalogFailed) {
				t.Errorf("motivo %s con esito %s e stepIndex %v", res.Reason, res.Outcome, res.StepIndex)
			}
		}
	}
	for _, want := range []string{"ok/", "failed/step-failed", "failed/timeout", "failed/checksum-mismatch", "refused/downgrade", "refused/requires-failed"} {
		if !reasons[want] {
			t.Errorf("esito %s non prodotto (prodotti: %v)", want, reasons)
		}
	}
	for _, ev := range h.journal.events {
		if err := v.validateJSON("events/catalog.installed.json", ev.Data); err != nil {
			t.Errorf("catalog.installed fuori schema: %v", err)
		}
	}
	if len(h.journal.events) != 1 {
		t.Fatalf("eventi: %d", len(h.journal.events))
	}
}

func jsonKeys(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return mapKeys(m)
}

func mapKeys(m map[string]any) string {
	ks := reflect.ValueOf(m).MapKeys()
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, k.String())
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
