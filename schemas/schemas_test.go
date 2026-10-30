// Package schemas verifica la coerenza degli schemi JSON del protocollo del connettore.
// Spec monsterterminal-saas.md §1.5 (schemi chiusi), §1.6 (vocabolario chiuso), §1.18 (schemi come
// fonte unica del contratto, versionati).
package schemas

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Versioni degli schemi nel repository: 0.1.0 (approvata il 2026-10-12), 0.2.0 (aperta il 2026-10-16,
// spec §1.15), 0.3.0 (aperta il 2026-10-17, spec §2.12: deadline in request.context) e 0.4.0 (aperta il
// 2026-10-18, ADR 0418 D5: BILLING_REQUIRED in http/error.json), 0.5.0 (2026-10-19, ADR 0410 A2: le
// domande dell'agente) 0.6.0 (2026-10-27, ADR 0409 A1.2: i comandi del terminale unico) 0.7.0 (2026-10-27, ADR 0409 A1.5:
// DragMeHere sugli host) e 0.8.0 (2026-10-30, ADR 0409 A2: un server non passa a un altro spazio di
// lavoro; HOST_OTHER_TENANT e POST /v1/register/check). Ogni test gira
// su ognuna.
var versions = []string{"0.1.0", "0.2.0", "0.3.0", "0.4.0", "0.5.0", "0.6.0", "0.7.0", "0.8.0"}

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
	// 0.4.0 (2026-10-18, ADR 0418 D5): stesso vocabolario della 0.3.0; cambia solo l'enum di
	// http/error.json (BILLING_REQUIRED).
	"0.4.0": {
		"connector-to-backend": {"catalog.install.progress", "catalog.install.result", "error", "event", "hello",
			"request.context", "request.respond.result", "session.closed", "session.opened", "sessions.list.result",
			"update.result"},
		"backend-to-connector": {"catalog.install", "config.update", "error", "events.ack", "request.respond",
			"session.close", "session.config", "session.open", "session.resize", "sessions.list", "update.available",
			"welcome"},
	},
	// 0.5.0 (2026-10-19, ADR 0410 A2, spec §2.14): request.context riceve questions, request.respond
	// riceve answers, e nasce request.closed (il connettore chiude da sé una richiesta annullata).
	"0.5.0": {
		"connector-to-backend": {"catalog.install.progress", "catalog.install.result", "error", "event", "hello",
			"request.closed", "request.context", "request.respond.result", "session.closed", "session.opened",
			"sessions.list.result", "update.result"},
		"backend-to-connector": {"catalog.install", "config.update", "error", "events.ack", "request.respond",
			"session.close", "session.config", "session.open", "session.resize", "sessions.list", "update.available",
			"welcome"},
	},
	// 0.6.0 (2026-10-27, ADR 0409 A1.2): fitToPane in session.open e sette comandi del terminale, ognuno
	// con la sua risposta per re.
	"0.6.0": {
		"connector-to-backend": {"catalog.install.progress", "catalog.install.result", "error", "event", "hello",
			"request.closed", "request.context", "request.respond.result", "session.buffer.result",
			"session.capture.result", "session.closed", "session.interrupt.result", "session.kill.result",
			"session.opened", "session.refresh.result", "session.rename.result", "session.zoom.result",
			"sessions.list.result", "update.result"},
		"backend-to-connector": {"catalog.install", "config.update", "error", "events.ack", "request.respond",
			"session.buffer", "session.capture", "session.close", "session.config", "session.interrupt", "session.kill",
			"session.open", "session.refresh", "session.rename", "session.resize", "session.zoom", "sessions.list",
			"update.available", "welcome"},
	},
	// 0.7.0 (2026-10-27, ADR 0409 A1.5): DragMeHere sugli host, file.put e file.cancel verso ~/dragmehere
	// (decisione BOSS 2026-10-27: niente navigazione delle cartelle, quindi niente dir.list).
	"0.7.0": {
		"connector-to-backend": {"catalog.install.progress", "catalog.install.result", "error",
			"event", "file.cancel.result", "file.put.result", "hello", "request.closed", "request.context",
			"request.respond.result", "session.buffer.result", "session.capture.result", "session.closed",
			"session.interrupt.result", "session.kill.result", "session.opened", "session.refresh.result",
			"session.rename.result", "session.zoom.result", "sessions.list.result", "update.result"},
		"backend-to-connector": {"catalog.install", "config.update", "error", "events.ack", "file.cancel",
			"file.put", "request.respond", "session.buffer", "session.capture", "session.close", "session.config",
			"session.interrupt", "session.kill", "session.open", "session.refresh", "session.rename", "session.resize",
			"session.zoom", "sessions.list", "update.available", "welcome"},
	},
	// 0.8.0 (2026-10-30, ADR 0409 A2): nessun messaggio nuovo, solo HTTP.
	"0.8.0": {
		"connector-to-backend": {"catalog.install.progress", "catalog.install.result", "error",
			"event", "file.cancel.result", "file.put.result", "hello", "request.closed", "request.context",
			"request.respond.result", "session.buffer.result", "session.capture.result", "session.closed",
			"session.interrupt.result", "session.kill.result", "session.opened", "session.refresh.result",
			"session.rename.result", "session.zoom.result", "sessions.list.result", "update.result"},
		"backend-to-connector": {"catalog.install", "config.update", "error", "events.ack", "file.cancel",
			"file.put", "request.respond", "session.buffer", "session.capture", "session.close", "session.config",
			"session.interrupt", "session.kill", "session.open", "session.refresh", "session.rename", "session.resize",
			"session.zoom", "sessions.list", "update.available", "welcome"},
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

// TestZeroQuattroEstendeZeroTre: la 0.4.0 non toglie nulla alla 0.3.0 (file, proprietà, codici di
// errore) e aggiunge il solo codice BILLING_REQUIRED a http/error.json (ADR 0418 D5, spec §3.13:
// la barriera del billing smette di presentarsi come HOST_DISABLED ai connettori che dichiarano
// X-MonsterTerminal-Schema ≥ 0.4.0).
func TestZeroQuattroEstendeZeroTre(t *testing.T) {
	old, cur := loadAll(t, "0.3.0"), loadAll(t, "0.4.0")
	props := func(n any) map[string]any {
		m, _ := n.(map[string]any)
		p, _ := m["properties"].(map[string]any)
		return p
	}
	codes := func(all map[string]map[string]any) map[string]bool {
		out := map[string]bool{}
		for _, c := range props(all["http/error.json"])["code"].(map[string]any)["enum"].([]any) {
			out[c.(string)] = true
		}
		return out
	}
	for rel, o := range old {
		c, ok := cur[rel]
		if !ok {
			t.Errorf("%s: sparito in 0.4.0", rel)
			continue
		}
		for k := range props(o) {
			if _, ok := props(c)[k]; !ok {
				t.Errorf("%s: proprietà %s sparita", rel, k)
			}
		}
		if rel == "http/error.json" {
			continue
		}
		if string(mustJSON(t, o)) != string(mustJSON(t, c)) {
			t.Errorf("%s: 0.4.0 cambia un file che doveva restare uguale alla 0.3.0", rel)
		}
	}
	oldCodes, curCodes := codes(old), codes(cur)
	for c := range oldCodes {
		if !curCodes[c] {
			t.Errorf("codice %s sparito in 0.4.0", c)
		}
	}
	if !curCodes["BILLING_REQUIRED"] {
		t.Error("http/error.json 0.4.0 senza BILLING_REQUIRED")
	}
	if oldCodes["BILLING_REQUIRED"] {
		t.Error("BILLING_REQUIRED non deve esistere in 0.3.0: i connettori 0.3.0 ricevono HOST_DISABLED")
	}
	if len(curCodes) != len(oldCodes)+1 {
		t.Errorf("0.4.0 aggiunge %d codici, atteso 1", len(curCodes)-len(oldCodes))
	}
}

// TestZeroCinqueEstendeZeroQuattro: la 0.5.0 non toglie nulla alla 0.4.0 (file, proprietà) e aggiunge
// solo ciò che §2.14 prevede: questions (opzionale) in request.context, answers (opzionale) in
// request.respond, il messaggio request.closed. Tutto il resto resta byte per byte uguale (ADR 0410 A2).
func TestZeroCinqueEstendeZeroQuattro(t *testing.T) {
	old, cur := loadAll(t, "0.4.0"), loadAll(t, "0.5.0")
	props := func(n any) map[string]any {
		m, _ := n.(map[string]any)
		p, _ := m["properties"].(map[string]any)
		return p
	}
	bodyProps := func(doc map[string]any) map[string]any { return props(props(doc)["body"]) }
	required := func(doc map[string]any) map[string]bool {
		out := map[string]bool{}
		b, _ := props(doc)["body"].(map[string]any)
		r, _ := b["required"].([]any)
		for _, k := range r {
			out[k.(string)] = true
		}
		return out
	}
	changed := map[string]bool{
		"messages/connector-to-backend/request.context.json": true,
		"messages/backend-to-connector/request.respond.json": true,
	}
	for rel, o := range old {
		c, ok := cur[rel]
		if !ok {
			t.Errorf("%s: sparito in 0.5.0", rel)
			continue
		}
		for k := range props(o) {
			if _, ok := props(c)[k]; !ok {
				t.Errorf("%s: proprietà %s sparita", rel, k)
			}
		}
		if changed[rel] {
			for k := range bodyProps(o) {
				if _, ok := bodyProps(c)[k]; !ok {
					t.Errorf("%s: campo %s del corpo sparito", rel, k)
				}
			}
			if string(mustJSON(t, map[string]any{"r": required(o)})) != string(mustJSON(t, map[string]any{"r": required(c)})) {
				t.Errorf("%s: 0.5.0 cambia i campi obbligatori del corpo", rel)
			}
			continue
		}
		if string(mustJSON(t, o)) != string(mustJSON(t, c)) {
			t.Errorf("%s: 0.5.0 cambia un file che doveva restare uguale alla 0.4.0", rel)
		}
	}
	ctx := bodyProps(cur["messages/connector-to-backend/request.context.json"])
	if _, ok := ctx["questions"]; !ok {
		t.Error("request.context 0.5.0 senza questions")
	}
	if _, ok := bodyProps(old["messages/connector-to-backend/request.context.json"])["questions"]; ok {
		t.Error("questions non deve esistere in 0.4.0")
	}
	resp := bodyProps(cur["messages/backend-to-connector/request.respond.json"])
	if _, ok := resp["answers"]; !ok {
		t.Error("request.respond 0.5.0 senza answers")
	}
	if _, ok := cur["messages/connector-to-backend/request.closed.json"]; !ok {
		t.Error("request.closed manca in 0.5.0")
	}
	if _, ok := old["messages/connector-to-backend/request.closed.json"]; ok {
		t.Error("request.closed non deve esistere in 0.4.0")
	}
	if len(cur) != len(old)+1 {
		t.Errorf("0.5.0 aggiunge %d file, atteso 1", len(cur)-len(old))
	}
}

// TestZeroSeiEstendeZeroCinque: la 0.6.0 non toglie nulla alla 0.5.0 e aggiunge solo ciò che ADR 0409
// A1.2 prevede: fitToPane (opzionale) in session.open e i sette comandi del terminale con le risposte.
// Ogni altro file resta byte per byte uguale; ogni richiesta ha la sua risposta che esige re; i messaggi
// per canale portano channel, quelli per sessione session col nome della regola (§1.7).
func TestZeroSeiEstendeZeroCinque(t *testing.T) {
	old, cur := loadAll(t, "0.5.0"), loadAll(t, "0.6.0")
	props := func(n any) map[string]any {
		m, _ := n.(map[string]any)
		p, _ := m["properties"].(map[string]any)
		return p
	}
	bodyOf := func(doc map[string]any) map[string]any { b, _ := props(doc)["body"].(map[string]any); return b }
	list := func(n any) []string {
		var out []string
		r, _ := n.([]any)
		for _, k := range r {
			out = append(out, k.(string))
		}
		sort.Strings(out)
		return out
	}
	const open = "messages/backend-to-connector/session.open.json"
	for rel, o := range old {
		c, ok := cur[rel]
		if !ok {
			t.Errorf("%s: sparito in 0.6.0", rel)
			continue
		}
		if rel == open {
			continue
		}
		if string(mustJSON(t, o)) != string(mustJSON(t, c)) {
			t.Errorf("%s: 0.6.0 cambia un file che doveva restare uguale alla 0.5.0", rel)
		}
	}
	ob, cb := bodyOf(old[open]), bodyOf(cur[open])
	if fmt.Sprint(list(ob["required"])) != fmt.Sprint(list(cb["required"])) {
		t.Error("session.open: 0.6.0 cambia i campi obbligatori")
	}
	for k := range props(ob) {
		if _, ok := props(cb)[k]; !ok {
			t.Errorf("session.open: campo %s sparito", k)
		}
	}
	if f, _ := props(cb)["fitToPane"].(map[string]any); f == nil || f["type"] != "boolean" || len(props(cb)) != len(props(ob))+1 {
		t.Errorf("session.open 0.6.0: atteso il solo campo nuovo fitToPane booleano, trovato %v", props(cb))
	}
	perCanale := map[string]bool{"refresh": true, "interrupt": true, "buffer": true}
	cmds := []string{"buffer", "capture", "interrupt", "kill", "refresh", "rename", "zoom"}
	for _, cmd := range cmds {
		req := cur["messages/backend-to-connector/session."+cmd+".json"]
		res := cur["messages/connector-to-backend/session."+cmd+".result.json"]
		if req == nil || res == nil {
			t.Errorf("session.%s: manca la richiesta o la risposta", cmd)
			continue
		}
		key := "session"
		if perCanale[cmd] {
			key = "channel"
		}
		want := "../../common.json#/$defs/sessionName"
		if key == "channel" {
			want = "../../common.json#/$defs/channel"
		}
		for _, doc := range []map[string]any{req, res} {
			f, _ := props(bodyOf(doc))[key].(map[string]any)
			if f == nil || f["$ref"] != want || !slices.Contains(list(bodyOf(doc)["required"]), key) {
				t.Errorf("session.%s: %s obbligatorio con %s", cmd, key, want)
			}
		}
		if !slices.Contains(list(res["required"]), "re") || slices.Contains(list(req["required"]), "re") {
			t.Errorf("session.%s: re obbligatorio nella sola risposta", cmd)
		}
	}
	rng, _ := props(bodyOf(cur["messages/backend-to-connector/session.capture.json"]))["range"].(map[string]any)
	if fmt.Sprint(rng["enum"]) != "[screen view last200 last1000]" {
		t.Errorf("session.capture range: %v, atteso l'enum di parseCaptureRange (screen view last200 last1000)", rng["enum"])
	}
	txt, _ := props(bodyOf(cur["messages/connector-to-backend/session.capture.result.json"]))["text"].(map[string]any)
	if txt["maxLength"] != float64(256<<10) {
		t.Errorf("session.capture.result text: maxLength %v, atteso 256 KiB", txt["maxLength"])
	}
	buf, _ := props(bodyOf(cur["messages/connector-to-backend/session.buffer.result.json"]))["text"].(map[string]any)
	if fmt.Sprint(buf["type"]) != "[string null]" || buf["maxLength"] != float64(64<<10) {
		t.Errorf("session.buffer.result text: %v, atteso string|null al più 64 KiB", buf)
	}
	zoom := bodyOf(cur["messages/backend-to-connector/session.zoom.json"])
	if fmt.Sprint(list(zoom["required"])) != "[session]" {
		t.Errorf("session.zoom: on è facoltativo (assente = inverti), required %v", zoom["required"])
	}
	if len(cur) != len(old)+2*len(cmds) {
		t.Errorf("0.6.0 aggiunge %d file, attesi %d", len(cur)-len(old), 2*len(cmds))
	}
}

// TestZeroSetteEstendeZeroSei: la 0.7.0 non toglie nulla alla 0.6.0 e aggiunge solo ciò che ADR 0409
// A1.5 prevede: le definizioni pathSegment, dragmehereDir e fileName in common.json e i messaggi file.put
// e file.cancel con le risposte; niente dir.list (decisione BOSS 2026-10-27). Ogni altro file resta byte
// per byte uguale.
func TestZeroSetteEstendeZeroSei(t *testing.T) {
	old, cur := loadAll(t, "0.6.0"), loadAll(t, "0.7.0")
	props := func(n any) map[string]any {
		m, _ := n.(map[string]any)
		p, _ := m["properties"].(map[string]any)
		return p
	}
	bodyOf := func(doc map[string]any) map[string]any { b, _ := props(doc)["body"].(map[string]any); return b }
	for rel, o := range old {
		c, ok := cur[rel]
		if !ok {
			t.Errorf("%s: sparito in 0.7.0", rel)
			continue
		}
		if rel == "common.json" {
			od, _ := o["$defs"].(map[string]any)
			cd, _ := c["$defs"].(map[string]any)
			for k, v := range od {
				if string(mustJSON(t, map[string]any{"v": v})) != string(mustJSON(t, map[string]any{"v": cd[k]})) {
					t.Errorf("common.json: definizione %s cambiata", k)
				}
			}
			for _, k := range []string{"dragmehereDir", "fileName", "pathSegment"} {
				if cd[k] == nil {
					t.Errorf("common.json 0.7.0 senza %s", k)
				}
			}
			if len(cd) != len(od)+3 {
				t.Errorf("common.json: %d definizioni nuove, attese 3", len(cd)-len(od))
			}
			continue
		}
		if string(mustJSON(t, o)) != string(mustJSON(t, c)) {
			t.Errorf("%s: 0.7.0 cambia un file che doveva restare uguale alla 0.6.0", rel)
		}
	}
	required := func(doc map[string]any) []string {
		var out []string
		r, _ := doc["required"].([]any)
		for _, k := range r {
			out = append(out, k.(string))
		}
		sort.Strings(out)
		return out
	}
	for _, cmd := range []string{"file.put", "file.cancel"} {
		req := cur["messages/backend-to-connector/"+cmd+".json"]
		res := cur["messages/connector-to-backend/"+cmd+".result.json"]
		if req == nil || res == nil {
			t.Errorf("%s: manca la richiesta o la risposta", cmd)
			continue
		}
		if !slices.Contains(required(res), "re") || slices.Contains(required(req), "re") {
			t.Errorf("%s: re obbligatorio nella sola risposta", cmd)
		}
	}
	put := props(bodyOf(cur["messages/backend-to-connector/file.put.json"]))
	if put["size"].(map[string]any)["maximum"] != float64(50<<20) {
		t.Errorf("file.put size: tetto %v, atteso 50 MiB", put["size"])
	}
	if put["data"].(map[string]any)["maxLength"] != float64(262144) {
		t.Errorf("file.put data: %v, atteso al più 262144 caratteri base64 (192 KiB)", put["data"])
	}
	if slices.Contains(required(bodyOf(cur["messages/backend-to-connector/file.put.json"])), "overwrite") {
		t.Error("file.put overwrite è facoltativo (assente = false)")
	}
	for _, f := range []string{"path"} {
		if put[f].(map[string]any)["$ref"] != "../../common.json#/$defs/dragmehereDir" {
			t.Errorf("file.put %s non usa dragmehereDir", f)
		}
	}
	if _, ok := cur["messages/backend-to-connector/dir.list.json"]; ok {
		t.Error("dir.list non fa parte della 0.7.0 (decisione BOSS 2026-10-27)")
	}
	if len(cur) != len(old)+4 {
		t.Errorf("0.7.0 aggiunge %d file, attesi 4", len(cur)-len(old))
	}
}

// TestZeroOttoEstendeZeroSette: la 0.8.0 (ADR 0409 A2, decisione BOSS 2026-10-29) non toglie nulla alla
// 0.7.0 e aggiunge solo il codice HOST_OTHER_TENANT a http/error.json e la verifica non consumante
// POST /v1/register/check: corpo {machineId}; risposta same-tenant con sameMachine oppure not-registered,
// in due oggetti chiusi. Ogni altro file resta byte per byte uguale.
func TestZeroOttoEstendeZeroSette(t *testing.T) {
	old, cur := loadAll(t, "0.7.0"), loadAll(t, "0.8.0")
	props := func(n any) map[string]any {
		m, _ := n.(map[string]any)
		p, _ := m["properties"].(map[string]any)
		return p
	}
	codes := func(doc map[string]any) []string {
		var out []string
		for _, c := range props(doc)["code"].(map[string]any)["enum"].([]any) {
			out = append(out, c.(string))
		}
		return out
	}
	for rel, o := range old {
		c, ok := cur[rel]
		if !ok {
			t.Errorf("%s: sparito in 0.8.0", rel)
			continue
		}
		if rel == "http/error.json" {
			oc, cc := codes(o), codes(c)
			for _, k := range oc {
				if !slices.Contains(cc, k) {
					t.Errorf("http/error.json: codice %s sparito", k)
				}
			}
			if !slices.Contains(cc, "HOST_OTHER_TENANT") || len(cc) != len(oc)+1 {
				t.Errorf("http/error.json 0.8.0: atteso il solo HOST_OTHER_TENANT in più, trovati %v", cc)
			}
			continue
		}
		if string(mustJSON(t, o)) != string(mustJSON(t, c)) {
			t.Errorf("%s: 0.8.0 cambia un file che doveva restare uguale alla 0.7.0", rel)
		}
	}
	req := cur["http/register-check-request.json"]
	if req == nil {
		t.Fatal("manca http/register-check-request.json")
	}
	if r, _ := req["required"].([]any); len(r) != 1 || r[0] != "machineId" || len(props(req)) != 1 {
		t.Errorf("register-check: il corpo è il solo machineId, trovato %v", props(req))
	}
	if props(req)["machineId"].(map[string]any)["pattern"] != props(cur["http/register-request.json"])["machineId"].(map[string]any)["pattern"] {
		t.Error("register-check: machineId con una regola diversa da register")
	}
	res := cur["http/register-check-response.json"]
	if res == nil {
		t.Fatal("manca http/register-check-response.json")
	}
	alts, _ := res["oneOf"].([]any)
	got := map[string][]string{}
	for _, a := range alts {
		m := a.(map[string]any)
		if m["additionalProperties"] != false {
			t.Error("register-check: risposta non chiusa")
		}
		res := props(m)["result"].(map[string]any)["const"].(string)
		var req []string
		for _, k := range m["required"].([]any) {
			req = append(req, k.(string))
		}
		sort.Strings(req)
		got[res] = req
	}
	if len(got) != 2 || !slices.Equal(got["same-tenant"], []string{"result", "sameMachine"}) ||
		!slices.Equal(got["not-registered"], []string{"result"}) {
		t.Errorf("register-check: risposte %v, attese same-tenant{result,sameMachine} e not-registered{result}", got)
	}
	if len(cur) != len(old)+2 {
		t.Errorf("0.8.0 aggiunge %d file, attesi 2", len(cur)-len(old))
	}
}

// mustJSON serializza un documento senza $id, per confrontare due versioni dello stesso file.
func mustJSON(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	cp := map[string]any{}
	for k, v := range doc {
		if k != "$id" {
			cp[k] = v
		}
	}
	b, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
