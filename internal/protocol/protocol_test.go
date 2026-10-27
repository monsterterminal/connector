// Test del protocollo. Asse A: busta, decodifica rigida, validazioni. Asse B: i campi delle strutture
// Go coincidono con quelli degli schemi 0.1.0 (spec monsterterminal-saas.md §1.5, §1.18).
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestEncodeDecodeEnvelope(t *testing.T) {
	frame, id, err := Encode(TypeHello, "", Hello{Capabilities: []string{}, Agents: []Agent{}}, time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	env, err := DecodeEnvelope(frame)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeHello || env.ID != id || env.Re != "" || env.TS != "2026-10-12T10:00:00.000Z" {
		t.Fatalf("busta inattesa: %+v", env)
	}
	if !IsUUID(id) {
		t.Fatalf("id non UUID v4: %s", id)
	}
}

func TestDecodeEnvelopeRifiuta(t *testing.T) {
	good := `{"type":"welcome","id":"` + NewUUID() + `","ts":"2026-10-12T10:00:00Z","body":{}}`
	if _, err := DecodeEnvelope([]byte(good)); err != nil {
		t.Fatalf("busta valida rifiutata: %v", err)
	}
	cases := map[string]string{
		"campo sconosciuto": `{"type":"x","id":"` + NewUUID() + `","ts":"2026-10-12T10:00:00Z","body":{},"extra":1}`,
		"id non uuid":       `{"type":"x","id":"abc","ts":"2026-10-12T10:00:00Z","body":{}}`,
		"ts non UTC":        `{"type":"x","id":"` + NewUUID() + `","ts":"2026-10-12T10:00:00+02:00","body":{}}`,
		"body mancante":     `{"type":"x","id":"` + NewUUID() + `","ts":"2026-10-12T10:00:00Z"}`,
		"type vuoto":        `{"type":"","id":"` + NewUUID() + `","ts":"2026-10-12T10:00:00Z","body":{}}`,
		"dati in coda":      good + `{}`,
	}
	for name, c := range cases {
		if _, err := DecodeEnvelope([]byte(c)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: atteso ErrInvalid, ottenuto %v", name, err)
		}
	}
}

func TestDecodeWelcome(t *testing.T) {
	ok := `{"hostId":"` + NewUUID() + `","protocol":1,"serverTime":"2026-10-12T10:00:00Z","config":{"defaultIdleThresholdSeconds":300,"sessionIdleThresholds":{},"recording":{"enabled":false},"maxChannels":12}}`
	if _, err := DecodeWelcome([]byte(ok)); err != nil {
		t.Fatalf("welcome valido rifiutato: %v", err)
	}
	bad := map[string]string{
		"soglia sotto 60":               strings.Replace(ok, `:300`, `:30`, 1),
		"registrazione senza giorni":    strings.Replace(ok, `{"enabled":false}`, `{"enabled":true}`, 1),
		"giorni a registrazione spenta": strings.Replace(ok, `{"enabled":false}`, `{"enabled":false,"retentionDays":3}`, 1),
		"campo sconosciuto in config":   strings.Replace(ok, `"maxChannels":12`, `"maxChannels":12,"x":1`, 1),
		"hostId non uuid":               strings.Replace(ok, `"hostId":"`, `"hostId":"z`, 1),
	}
	for name, c := range bad {
		if _, err := DecodeWelcome([]byte(c)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: atteso ErrInvalid, ottenuto %v", name, err)
		}
	}
}

// Asse B: le chiavi JSON delle strutture coincidono con le proprietà degli schemi.
func TestCorrispondenzaConGliSchemi(t *testing.T) {
	cases := []struct {
		schema string
		value  any
	}{
		{"messages/connector-to-backend/hello.json", Hello{}},
		{"messages/backend-to-connector/welcome.json", Welcome{}},
		{"messages/backend-to-connector/config.update.json", ConfigUpdate{}},
		{"messages/connector-to-backend/error.json", nil}, // corpo = errorBody, verificato sotto
		{"objects/host-config.json", HostConfig{}},
		{"messages/connector-to-backend/event.json", Event{Seq: 1, Kind: "x", At: "x", Data: json.RawMessage(`{}`)}},
		{"messages/backend-to-connector/events.ack.json", EventsAck{}},
		{"events/connector.restarted.json", ConnectorRestarted{}},
		{"events/journal.truncated.json", JournalTruncated{}},
		{"events/agent.state.json", AgentState{WaitingFor: "x", RequestID: "x"}},
		{"messages/backend-to-connector/session.open.json", SessionOpen{FitToPane: true}},
		{"messages/backend-to-connector/session.resize.json", SessionResize{}},
		{"messages/backend-to-connector/session.close.json", SessionClose{}},
		{"messages/backend-to-connector/sessions.list.json", struct{}{}},
		{"messages/connector-to-backend/session.opened.json", SessionOpened{}},
		{"messages/connector-to-backend/session.closed.json", SessionClosed{}},
		{"messages/connector-to-backend/sessions.list.result.json", SessionsListResult{}},
		// Schemi 0.6.0 (ADR 0409 A1.2): i comandi del terminale unico e le risposte.
		{"messages/backend-to-connector/session.refresh.json", ChannelBody{}},
		{"messages/backend-to-connector/session.interrupt.json", ChannelBody{}},
		{"messages/backend-to-connector/session.buffer.json", ChannelBody{}},
		{"messages/backend-to-connector/session.kill.json", SessionBody{}},
		{"messages/backend-to-connector/session.rename.json", SessionRename{}},
		{"messages/backend-to-connector/session.zoom.json", SessionZoom{On: new(bool)}},
		{"messages/backend-to-connector/session.capture.json", SessionCapture{}},
		{"messages/connector-to-backend/session.refresh.result.json", ChannelBody{}},
		{"messages/connector-to-backend/session.interrupt.result.json", ChannelBody{}},
		{"messages/connector-to-backend/session.buffer.result.json", SessionBufferResult{}},
		{"messages/connector-to-backend/session.kill.result.json", SessionBody{}},
		{"messages/connector-to-backend/session.rename.result.json", SessionRename{}},
		{"messages/connector-to-backend/session.zoom.result.json", SessionZoomResult{Panes: 1}},
		{"messages/connector-to-backend/session.capture.result.json", SessionCaptureResult{}},
	}
	for _, c := range cases {
		if c.value == nil {
			continue
		}
		want := schemaProps(t, c.schema)
		got := jsonKeys(t, c.value)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: campi Go %v, campi dello schema %v", c.schema, got, want)
		}
	}
	common := readSchema(t, "common.json")
	eb := common["$defs"].(map[string]any)["errorBody"].(map[string]any)
	if got, want := jsonKeys(t, ErrorBody{}), keys(eb["properties"].(map[string]any)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("errorBody: campi Go %v, schema %v", got, want)
	}
	// I motivi del riavvio sono l'enum dello schema, nello stesso ordine.
	rr := readSchema(t, "events/connector.restarted.json")["properties"].(map[string]any)["reason"].(map[string]any)["enum"].([]any)
	if fmt.Sprint(rr) != fmt.Sprint(RestartReasons) {
		t.Errorf("motivi del riavvio: Go %v, schema %v", RestartReasons, rr)
	}
	// Voce di sessions.list.result: tutti i campi, opzionali compresi.
	items := readSchema(t, "messages/connector-to-backend/sessions.list.result.json")["properties"].(map[string]any)["body"].(map[string]any)["properties"].(map[string]any)["sessions"].(map[string]any)["items"].(map[string]any)
	full := SessionInfo{Name: "a", CreatedAt: "x", Windows: 1, Agent: "claude-code", State: "lavora"}
	if got, want := jsonKeys(t, full), keys(items["properties"].(map[string]any)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("voce di sessions.list.result: Go %v, schema %v", got, want)
	}
	if mx := int(readSchema(t, "messages/connector-to-backend/sessions.list.result.json")["properties"].(map[string]any)["body"].(map[string]any)["properties"].(map[string]any)["sessions"].(map[string]any)["maxItems"].(float64)); mx != MaxSessionsListed {
		t.Errorf("maxItems %d, Go %d", mx, MaxSessionsListed)
	}
	// Motivi di chiusura, modi e regola dei nomi: quelli dello schema.
	cr := readSchema(t, "messages/connector-to-backend/session.closed.json")["properties"].(map[string]any)["body"].(map[string]any)["properties"].(map[string]any)["reason"].(map[string]any)["enum"].([]any)
	if fmt.Sprint(cr) != fmt.Sprint(ClosedReasons) {
		t.Errorf("motivi di chiusura: Go %v, schema %v", ClosedReasons, cr)
	}
	op := readSchema(t, "messages/backend-to-connector/session.open.json")["properties"].(map[string]any)["body"].(map[string]any)["properties"].(map[string]any)
	if fmt.Sprint(op["mode"].(map[string]any)["enum"]) != fmt.Sprint([]string{ModeView, ModeInteract}) {
		t.Errorf("modi: schema %v", op["mode"])
	}
	if int(op["cols"].(map[string]any)["maximum"].(float64)) != MaxTerminalSize {
		t.Errorf("dimensione massima: schema %v, Go %d", op["cols"], MaxTerminalSize)
	}
	defs := common["$defs"].(map[string]any)
	if defs["sessionName"].(map[string]any)["pattern"].(string) != sessionNameRE.String() {
		t.Errorf("regola dei nomi: schema %v, Go %s", defs["sessionName"], sessionNameRE)
	}
	if int64(defs["channel"].(map[string]any)["maximum"].(float64)) != 1<<32-1 {
		t.Errorf("canale: schema %v, Go uint32", defs["channel"])
	}
	// Stati, origini e motivi d'attesa di agent.state: gli enum dello schema (ADR 0410 D1, §2.9).
	asp := readSchema(t, "events/agent.state.json")["properties"].(map[string]any)
	if fmt.Sprint(asp["source"].(map[string]any)["enum"]) != fmt.Sprint(AgentSources) {
		t.Errorf("origini: schema %v, Go %v", asp["source"], AgentSources)
	}
	if fmt.Sprint(asp["waitingFor"].(map[string]any)["enum"]) != fmt.Sprint(WaitingForAll) {
		t.Errorf("waitingFor: schema %v, Go %v", asp["waitingFor"], WaitingForAll)
	}
	if fmt.Sprint(defs["agentState"].(map[string]any)["enum"]) != fmt.Sprint(AgentStates) {
		t.Errorf("stati: schema %v, Go %v", defs["agentState"], AgentStates)
	}
	// L'enum dei range di session.capture e i tetti dei testi (ADR 0409 A1.2).
	cp := readSchema(t, "messages/backend-to-connector/session.capture.json")["properties"].(map[string]any)["body"].(map[string]any)["properties"].(map[string]any)
	if fmt.Sprint(cp["range"].(map[string]any)["enum"]) != fmt.Sprint(CaptureRanges) {
		t.Errorf("range di session.capture: schema %v, Go %v", cp["range"], CaptureRanges)
	}
	textMax := func(rel string) int {
		p := readSchema(t, rel)["properties"].(map[string]any)["body"].(map[string]any)["properties"].(map[string]any)
		return int(p["text"].(map[string]any)["maxLength"].(float64))
	}
	if textMax("messages/connector-to-backend/session.capture.result.json") != MaxCaptureBytes ||
		textMax("messages/connector-to-backend/session.buffer.result.json") != MaxBufferBytes {
		t.Error("tetti dei testi: schema e Go divergono")
	}
	if Version != 1 || SchemaVersion != "0.6.0" {
		t.Errorf("versioni inattese: protocollo %d, schemi %s", Version, SchemaVersion)
	}
}

func readSchema(t *testing.T, rel string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", SchemaVersion, rel))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// schemaProps restituisce le proprietà del body per un messaggio, o dell'oggetto per gli altri schemi.
func schemaProps(t *testing.T, rel string) []string {
	s := readSchema(t, rel)
	props := s["properties"].(map[string]any)
	if body, ok := props["body"].(map[string]any); ok && strings.HasPrefix(rel, "messages/") {
		props = body["properties"].(map[string]any)
	}
	return keys(props)
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func jsonKeys(t *testing.T, v any) []string {
	// Tutti i campi valorizzati per non perderne con omitempty.
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	out := keys(m)
	// i campi omitempty della struttura vanno aggiunti a mano
	if _, ok := v.(Recording); ok {
		out = append(out, "retentionDays")
	}
	sort.Strings(out)
	return out
}

// §1.7: frame binari con intestazione di 5 byte, canale big-endian.
func TestFrameBinari(t *testing.T) {
	f := EncodeFrame(FrameOutput, 0x01020304, []byte("ciao"))
	if fmt.Sprintf("% x", f[:5]) != "01 01 02 03 04" || string(f[5:]) != "ciao" {
		t.Fatalf("frame % x", f)
	}
	typ, ch, data, err := DecodeFrame(EncodeFrame(FrameInput, 4294967295, nil))
	if err != nil || typ != FrameInput || ch != 4294967295 || len(data) != 0 {
		t.Fatalf("decodifica: %x %d %q %v", typ, ch, data, err)
	}
	for _, bad := range [][]byte{nil, {1, 0, 0, 0}, {3, 0, 0, 0, 1}} {
		if _, _, _, err := DecodeFrame(bad); err == nil {
			t.Errorf("frame % x accettato", bad)
		}
	}
}

func TestDecodeSessionOpen(t *testing.T) {
	ok := `{"channel":7,"session":"lavoro.1","create":false,"mode":"view","cols":80,"rows":24}`
	o, err := DecodeSessionOpen([]byte(ok))
	if err != nil || o.Channel != 7 || o.Session != "lavoro.1" || o.Mode != ModeView {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := DecodeSessionOpen([]byte(`{"channel":7,"session":"-x","create":false,"mode":"view","cols":80,"rows":24}`)); !errors.Is(err, ErrSessionName) {
		t.Errorf("nome fuori regola: %v", err)
	}
	for _, bad := range []string{
		`{"channel":7,"session":"a","create":false,"mode":"edit","cols":80,"rows":24}`,
		`{"channel":7,"session":"a","create":false,"mode":"view","cols":0,"rows":24}`,
		`{"channel":7,"session":"a","create":false,"mode":"view","cols":80,"rows":1001}`,
		`{"channel":7,"session":"a","mode":"view","cols":80,"rows":24}`,
		`{"channel":-1,"session":"a","create":false,"mode":"view","cols":80,"rows":24}`,
		`{"channel":7,"session":"a","create":false,"mode":"view","cols":80,"rows":24,"x":1}`,
	} {
		if _, err := DecodeSessionOpen([]byte(bad)); err == nil || errors.Is(err, ErrSessionName) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := DecodeSessionResize([]byte(`{"channel":1,"cols":10}`)); err == nil {
		t.Error("resize senza rows accettato")
	}
	if _, err := DecodeSessionClose([]byte(`{}`)); err == nil {
		t.Error("close senza canale accettato")
	}
	if err := DecodeSessionsList([]byte(`{"x":1}`)); err == nil {
		t.Error("sessions.list con campi accettato")
	}
}

// Decodifica dei comandi 0.6.0 (ADR 0409 A1.2): oggetti chiusi, nomi con la regola, range nell'enum.
func TestDecodeComandiZeroSei(t *testing.T) {
	if o, err := DecodeSessionOpen([]byte(`{"channel":1,"session":"a","create":false,"mode":"view","cols":80,"rows":24,"fitToPane":true}`)); err != nil || !o.FitToPane {
		t.Fatalf("fitToPane: %+v %v", o, err)
	}
	if o, _ := DecodeSessionOpen([]byte(`{"channel":1,"session":"a","create":false,"mode":"view","cols":80,"rows":24}`)); o.FitToPane {
		t.Error("fitToPane assente deve valere false (backend 0.5.0)")
	}
	if b, err := DecodeChannelBody(TypeSessionRefresh, []byte(`{"channel":3}`)); err != nil || b.Channel != 3 {
		t.Fatalf("canale: %+v %v", b, err)
	}
	for _, bad := range []string{`{}`, `{"channel":-1}`, `{"channel":1,"x":1}`} {
		if _, err := DecodeChannelBody(TypeSessionBuffer, []byte(bad)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := DecodeSessionKill([]byte(`{"session":"-x"}`)); !errors.Is(err, ErrSessionName) {
		t.Errorf("kill fuori regola: %v", err)
	}
	if _, err := DecodeSessionRename([]byte(`{"session":"a","name":"b c"}`)); !errors.Is(err, ErrSessionName) {
		t.Errorf("rename verso un nome fuori regola: %v", err)
	}
	if _, err := DecodeSessionRename([]byte(`{"session":"a"}`)); !errors.Is(err, ErrInvalid) {
		t.Errorf("rename senza name: %v", err)
	}
	if z, err := DecodeSessionZoom([]byte(`{"session":"a"}`)); err != nil || z.On != nil {
		t.Errorf("zoom senza on (inverti): %+v %v", z, err)
	}
	if z, err := DecodeSessionZoom([]byte(`{"session":"a","on":false}`)); err != nil || z.On == nil || *z.On {
		t.Errorf("zoom on=false: %+v %v", z, err)
	}
	for _, r := range CaptureRanges {
		if c, err := DecodeSessionCapture([]byte(`{"session":"a","range":"` + r + `"}`)); err != nil || c.Range != r {
			t.Errorf("range %s: %+v %v", r, c, err)
		}
	}
	for _, bad := range []string{`{"session":"a","range":"all"}`, `{"session":"a"}`, `{"session":"a","range":"screen","x":1}`} {
		if _, err := DecodeSessionCapture([]byte(bad)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	// Buffer assente: text è null, non una stringa vuota.
	raw, _ := json.Marshal(SessionBufferResult{Channel: 1})
	if string(raw) != `{"channel":1,"text":null}` {
		t.Errorf("buffer assente: %s", raw)
	}
}
