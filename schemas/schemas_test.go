// Package schemas verifica la coerenza degli schemi JSON del protocollo del connettore.
// Spec monsterterminal-saas.md §1.5 (schemi chiusi), §1.6 (vocabolario chiuso), §1.18 (schemi come
// fonte unica del contratto, versionati).
package schemas

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const version = "0.1.0"
const baseID = "https://monsterterminal.com/schemas/connector/" + version + "/"

// Vocabolario chiuso dei messaggi, spec §1.6.
var wantMessages = map[string][]string{
	"connector-to-backend": {"catalog.install.progress", "catalog.install.result", "error", "event", "hello",
		"session.closed", "session.opened", "sessions.list.result", "update.result"},
	"backend-to-connector": {"catalog.install", "config.update", "error", "events.ack", "session.close",
		"session.config", "session.open", "session.resize", "sessions.list", "update.available", "welcome"},
}

// Stati dell'agente, ADR 0410 D1.
var wantStates = []string{"attende-input", "errore", "ha-finito", "inattivo", "lavora"}

func loadAll(t *testing.T) map[string]map[string]any {
	t.Helper()
	root := version
	out := map[string]map[string]any{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("%s: JSON non valido: %v", p, err)
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = doc
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("nessuno schema trovato")
	}
	return out
}

func TestIDCoerenteColPercorso(t *testing.T) {
	for rel, doc := range loadAll(t) {
		if got, want := doc["$id"], baseID+rel; got != want {
			t.Errorf("%s: $id %v, atteso %s", rel, got, want)
		}
		if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s: $schema non è il draft 2020-12", rel)
		}
	}
}

// Ogni oggetto con properties è chiuso (spec §1.5).
func TestOggettiChiusi(t *testing.T) {
	var walk func(rel, path string, n any)
	walk = func(rel, path string, n any) {
		switch v := n.(type) {
		case map[string]any:
			if _, ok := v["properties"]; ok {
				if v["additionalProperties"] != false {
					t.Errorf("%s %s: oggetto con properties senza additionalProperties:false", rel, path)
				}
			}
			for k, c := range v {
				walk(rel, path+"/"+k, c)
			}
		case []any:
			for i, c := range v {
				walk(rel, path+"/"+string(rune('0'+i%10)), c)
			}
		}
	}
	for rel, doc := range loadAll(t) {
		walk(rel, "", doc)
	}
}

// Ogni $ref punta a un file esistente e, se c'è un frammento, a una definizione esistente.
func TestRiferimentiRisolti(t *testing.T) {
	all := loadAll(t)
	var walk func(rel string, n any)
	walk = func(rel string, n any) {
		switch v := n.(type) {
		case map[string]any:
			if r, ok := v["$ref"].(string); ok {
				file, frag, _ := strings.Cut(r, "#")
				target := rel
				if file != "" {
					target = filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(rel), file)))
				}
				doc, ok := all[target]
				if !ok {
					t.Errorf("%s: $ref %q verso un file inesistente (%s)", rel, r, target)
				} else if frag != "" {
					name := strings.TrimPrefix(frag, "/$defs/")
					defs, _ := doc["$defs"].(map[string]any)
					if _, ok := defs[name]; !ok {
						t.Errorf("%s: $ref %q verso una definizione inesistente", rel, r)
					}
				}
			}
			for _, c := range v {
				walk(rel, c)
			}
		case []any:
			for _, c := range v {
				walk(rel, c)
			}
		}
	}
	for rel, doc := range all {
		walk(rel, doc)
	}
}

// Il vocabolario dei messaggi è esattamente quello della spec, e ogni type coincide col nome del file.
func TestVocabolarioChiuso(t *testing.T) {
	all := loadAll(t)
	for dir, want := range wantMessages {
		var got []string
		for rel, doc := range all {
			prefix := "messages/" + dir + "/"
			if !strings.HasPrefix(rel, prefix) {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(rel, prefix), ".json")
			got = append(got, name)
			props := doc["properties"].(map[string]any)
			typ := props["type"].(map[string]any)
			if typ["const"] != name {
				t.Errorf("%s: type const %v, atteso %s", rel, typ["const"], name)
			}
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: messaggi %v, attesi %v", dir, got, want)
		}
	}
}

// Gli stati dell'agente sono l'insieme chiuso di ADR 0410 D1, e ogni kind di evento ha il suo schema.
func TestStatiEKindDegliEventi(t *testing.T) {
	all := loadAll(t)
	defs := all["common.json"]["$defs"].(map[string]any)
	var states []string
	for _, s := range defs["agentState"].(map[string]any)["enum"].([]any) {
		states = append(states, s.(string))
	}
	sort.Strings(states)
	if strings.Join(states, ",") != strings.Join(wantStates, ",") {
		t.Errorf("stati %v, attesi %v", states, wantStates)
	}
	event := all["messages/connector-to-backend/event.json"]["properties"].(map[string]any)["body"].(map[string]any)
	kinds := event["properties"].(map[string]any)["kind"].(map[string]any)["enum"].([]any)
	for _, k := range kinds {
		if _, ok := all["events/"+k.(string)+".json"]; !ok {
			t.Errorf("kind %s senza schema in events/", k)
		}
	}
}
