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

// Versioni degli schemi nel repository: 0.1.0 (approvata il 2026-10-12), 0.2.0 (aperta il 2026-10-16,
// spec §1.15) e 0.3.0 (aperta il 2026-10-17, spec §2.12: deadline in request.context). Ogni test gira su ognuna.
var versions = []string{"0.1.0", "0.2.0", "0.3.0"}

func baseID(version string) string {
	return "https://monsterterminal.com/schemas/connector/" + version + "/"
}

// Vocabolario chiuso dei messaggi, spec §1.6; 0.2.0 aggiunge i tre messaggi di §2.9.
var wantMessages = map[string]map[string][]string{
	"0.1.0": {
		"connector-to-backend": {"catalog.install.progress", "catalog.install.result", "error", "event", "hello",
			"session.closed", "session.opened", "sessions.list.result", "update.result"},
		"backend-to-connector": {"catalog.install", "config.update", "error", "events.ack", "session.close",
			"session.config", "session.open", "session.resize", "sessions.list", "update.available", "welcome"},
	},
	"0.2.0": {
		"connector-to-backend": {"catalog.install.progress", "catalog.install.result", "error", "event", "hello",
			"request.context", "request.respond.result", "session.closed", "session.opened", "sessions.list.result",
			"update.result"},
		"backend-to-connector": {"catalog.install", "config.update", "error", "events.ack", "request.respond",
			"session.close", "session.config", "session.open", "session.resize", "sessions.list", "update.available",
			"welcome"},
	},
	// 0.3.0 (2026-10-17, spec §2.12): stesso vocabolario della 0.2.0; request.context riceve il campo
	// opzionale deadline.
	"0.3.0": {
		"connector-to-backend": {"catalog.install.progress", "catalog.install.result", "error", "event", "hello",
			"request.context", "request.respond.result", "session.closed", "session.opened", "sessions.list.result",
			"update.result"},
		"backend-to-connector": {"catalog.install", "config.update", "error", "events.ack", "request.respond",
			"session.close", "session.config", "session.open", "session.resize", "sessions.list", "update.available",
			"welcome"},
	},
}

// Stati dell'agente, ADR 0410 D1.
var wantStates = []string{"attende-input", "errore", "ha-finito", "inattivo", "lavora"}

func loadAll(t *testing.T, version string) map[string]map[string]any {
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
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			for rel, doc := range loadAll(t, version) {
				if got, want := doc["$id"], baseID(version)+rel; got != want {
					t.Errorf("%s: $id %v, atteso %s", rel, got, want)
				}
				if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
					t.Errorf("%s: $schema non è il draft 2020-12", rel)
				}
			}
		})
	}
}

// Ogni oggetto con properties è chiuso (spec §1.5).
func TestOggettiChiusi(t *testing.T) {
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
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
						if k == "if" {
							continue // una condizione, non un oggetto del messaggio: non va chiusa
						}
						walk(rel, path+"/"+k, c)
					}
				case []any:
					for i, c := range v {
						walk(rel, path+"/"+string(rune('0'+i%10)), c)
					}
				}
			}
			for rel, doc := range loadAll(t, version) {
				walk(rel, "", doc)
			}
		})
	}
}

// Ogni $ref punta a un file esistente e, se c'è un frammento, a una definizione esistente.
func TestRiferimentiRisolti(t *testing.T) {
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			all := loadAll(t, version)
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
		})
	}
}

// Il vocabolario dei messaggi è esattamente quello della spec, e ogni type coincide col nome del file.
func TestVocabolarioChiuso(t *testing.T) {
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			all := loadAll(t, version)
			for dir, want := range wantMessages[version] {
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
		})
	}
}

// Gli stati dell'agente sono l'insieme chiuso di ADR 0410 D1, e ogni kind di evento ha il suo schema.
func TestStatiEKindDegliEventi(t *testing.T) {
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			all := loadAll(t, version)
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
		})
	}
}

// 0.2.0 solo aggiunge (spec §1.15): ogni schema di 0.1.0 c'è ancora, nessuna proprietà sparisce e
// nessuna diventa obbligatoria. Un messaggio valido per 0.1.0 resta valido.
func TestZeroDueEstendeZeroUno(t *testing.T) {
	old, cur := loadAll(t, "0.1.0"), loadAll(t, "0.2.0")
	var props func(n any) map[string]any
	props = func(n any) map[string]any {
		m, _ := n.(map[string]any)
		p, _ := m["properties"].(map[string]any)
		return p
	}
	req := func(n any) []string {
		m, _ := n.(map[string]any)
		var out []string
		for _, r := range m["required"].([]any) {
			out = append(out, r.(string))
		}
		sort.Strings(out)
		return out
	}
	for rel, o := range old {
		c, ok := cur[rel]
		if !ok {
			t.Errorf("%s: sparito in 0.2.0", rel)
			continue
		}
		check := func(where string, on, cn any) {
			op, cp := props(on), props(cn)
			for k := range op {
				if _, ok := cp[k]; !ok {
					t.Errorf("%s%s: proprietà %s sparita", rel, where, k)
				}
			}
			if on.(map[string]any)["required"] != nil && strings.Join(req(on), ",") != strings.Join(req(cn), ",") {
				t.Errorf("%s%s: required cambiato: %v → %v", rel, where, req(on), req(cn))
			}
		}
		check("", o, c)
		if ob, ok := props(o)["body"]; ok {
			check(" body", ob, props(c)["body"])
		}
	}
	// Le aggiunte di 0.2.0 attese dal contratto.
	as := props(cur["events/agent.state.json"])
	if _, ok := as["waitingFor"]; !ok {
		t.Error("agent.state senza waitingFor")
	}
	if _, ok := as["requestId"]; !ok {
		t.Error("agent.state senza requestId")
	}
	codes := props(cur["http/error.json"])["code"].(map[string]any)["enum"].([]any)
	joined := ""
	for _, c := range codes {
		joined += c.(string) + ","
	}
	if !strings.Contains(joined, "RATE_LIMITED,") || !strings.Contains(joined, "UNAVAILABLE,") {
		t.Errorf("http/error.json senza RATE_LIMITED o UNAVAILABLE: %v", codes)
	}
	if cur["common.json"]["$defs"].(map[string]any)["seq"].(map[string]any)["maximum"] != float64(9007199254740991) {
		t.Error("seq senza il tetto 2^53-1")
	}
}

// TestZeroTreEstendeZeroDue: la 0.3.0 non toglie nulla alla 0.2.0 e aggiunge il solo campo opzionale
// deadline a request.context (spec §2.12, §1.15: tipi e campi opzionali nuovi non cambiano il protocollo).
func TestZeroTreEstendeZeroDue(t *testing.T) {
	old, cur := loadAll(t, "0.2.0"), loadAll(t, "0.3.0")
	props := func(n any) map[string]any {
		m, _ := n.(map[string]any)
		p, _ := m["properties"].(map[string]any)
		return p
	}
	for rel, o := range old {
		c, ok := cur[rel]
		if !ok {
			t.Errorf("%s: sparito in 0.3.0", rel)
			continue
		}
		for k := range props(o) {
			if _, ok := props(c)[k]; !ok {
				t.Errorf("%s: proprietà %s sparita", rel, k)
			}
		}
		if ob, ok := props(o)["body"]; ok {
			for k := range props(ob) {
				if _, ok := props(props(c)["body"])[k]; !ok {
					t.Errorf("%s body: proprietà %s sparita", rel, k)
				}
			}
		}
	}
	body := props(props(cur["messages/connector-to-backend/request.context.json"])["body"])
	if _, ok := body["deadline"]; !ok {
		t.Error("request.context 0.3.0 senza deadline")
	}
	rc := props(cur["messages/connector-to-backend/request.context.json"])["body"].(map[string]any)
	for _, r := range rc["required"].([]any) {
		if r == "deadline" {
			t.Error("deadline deve restare opzionale: un connettore 0.2.0 non lo manda")
		}
	}
	if _, ok := props(props(old["messages/connector-to-backend/request.context.json"])["body"])["deadline"]; ok {
		t.Error("deadline non deve esistere in 0.2.0")
	}
}
