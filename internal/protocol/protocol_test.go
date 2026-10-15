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
	if Version != 1 || SchemaVersion != "0.1.0" {
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
