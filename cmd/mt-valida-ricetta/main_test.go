package main

// Prove del validatore delle ricette (spec §1.9, ADR 0410 A4, lotto L4). Asse A: ogni controllo ha un
// caso che lo fa scattare e la ricetta buona passa pulita. Asse B (#26): i vincoli verificati sono
// quelli di §1.9 («mai http, mai latest, versioni fissate, forma canonica, tetto di 180 giorni, source
// coerente») e della decisione 7 del piano di Fase 3 (hooks install -yes come ultimo passo).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/catalog"
)

var adesso = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func buona() map[string]any {
	conn := "/usr/local/bin/monsterterminal-connector"
	url := "https://github.com/anomalyco/opencode/releases/download/v2.4.2/opencode-linux-x64.tar.gz"
	return map[string]any{
		"recipeId": "opencode-amd64", "recipeVersion": 1, "expiresAt": "2027-05-25T00:00:00Z",
		"source":    map[string]any{"channel": "release", "ref": url},
		"agent":     "opencode",
		"platforms": []any{map[string]any{"os": "linux", "arch": []any{"amd64"}, "distro": []any{"debian", "ubuntu", "rhel"}}},
		"requires":  []any{},
		"steps": []any{
			map[string]any{"fetch": map[string]any{"url": url, "sha256": strings.Repeat("ab", 32), "dest": "opencode-linux-x64.tar.gz"}, "timeoutSeconds": 900},
			map[string]any{"run": []any{conn, "catalog", "extract", "{work}/opencode-linux-x64.tar.gz", ".local/opencode"}, "timeoutSeconds": 300},
			map[string]any{"run": []any{conn, "catalog", "path-add", ".local/opencode"}, "timeoutSeconds": 30},
		},
		"auth":    map[string]any{"instructions": "Open a session and run: opencode auth login"},
		"start":   []any{"opencode"},
		"adapter": map[string]any{"kind": "generic", "id": "opencode"},
	}
}

func npmBuona() map[string]any {
	conn := "/usr/local/bin/monsterterminal-connector"
	m := buona()
	m["recipeId"], m["agent"] = "claude-code", "claude-code"
	m["source"] = map[string]any{"channel": "npm", "ref": "@anthropic-ai/claude-code@2.1.332"}
	m["requires"] = []any{map[string]any{"check": "command", "name": "node", "minVersion": "22.0.0"}}
	m["steps"] = []any{
		map[string]any{"run": []any{"npm", "install", "-g", "@anthropic-ai/claude-code@2.1.332", "--prefix", ".npm-global"}, "timeoutSeconds": 900},
		map[string]any{"run": []any{conn, "catalog", "path-add", ".npm-global/bin"}, "timeoutSeconds": 30},
		map[string]any{"run": []any{conn, "hooks", "install", "-yes"}, "timeoutSeconds": 60},
	}
	m["adapter"] = map[string]any{"kind": "hooks", "id": "claude-code"}
	return m
}

func canon(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := catalog.Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func steps(m map[string]any) []any { return m["steps"].([]any) }

func TestRicetteBuonePassano(t *testing.T) {
	for name, m := range map[string]map[string]any{"release": buona(), "npm": npmBuona()} {
		path := "recipes/" + m["recipeId"].(string) + "/1.json"
		if errs := Check(path, canon(t, m), adesso, MaxValidityDays); len(errs) != 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

func TestOgniControlloScatta(t *testing.T) {
	cases := map[string]struct {
		edit func(m map[string]any)
		raw  func(raw []byte) []byte // ritocco dei byte dopo la forma canonica
		path string
		want string
	}{
		"campo sconosciuto":            {edit: func(m map[string]any) { m["extra"] = 1 }, want: "non è una ricetta"},
		"fuori schema (arch)":          {edit: func(m map[string]any) { m["platforms"].([]any)[0].(map[string]any)["arch"] = []any{"mips"} }, want: "schema 0.11.0"},
		"non canonica (spazi)":         {raw: func(raw []byte) []byte { return bytes.Replace(raw, []byte(`":`), []byte(`": `), 1) }, want: "forma canonica"},
		"non canonica (a capo finale)": {raw: func(raw []byte) []byte { return append(raw, '\n') }, want: "forma canonica"},
		"percorso con altro id":        {path: "recipes/opencode/1.json", want: "recipeId"},
		"percorso con altra versione":  {path: "recipes/opencode-amd64/2.json", want: "recipeVersion"},
		"scaduta":                      {edit: func(m map[string]any) { m["expiresAt"] = "2026-11-26T11:59:59Z" }, want: "già passato"},
		"oltre 180 giorni":             {edit: func(m map[string]any) { m["expiresAt"] = "2027-05-26T00:00:00Z" }, want: "oltre 180 giorni"},
		"http":                         {edit: func(m map[string]any) { m["auth"].(map[string]any)["instructions"] = "see http://example.com" }, want: "http://"},
		"latest nell'url": {edit: func(m map[string]any) {
			steps(m)[0].(map[string]any)["fetch"].(map[string]any)["url"] = "https://example.com/latest/x.tar.gz"
		}, want: "latest"},
		"latest in source.ref":        {edit: func(m map[string]any) { m["source"].(map[string]any)["ref"] = "https://example.com/latest/x.tar.gz" }, want: "latest"},
		"release senza fetch del ref": {edit: func(m map[string]any) { m["source"].(map[string]any)["ref"] = "https://example.com/v1/altro.tar.gz" }, want: "nessun passo fetch"},
		"release che chiama npm": {edit: func(m map[string]any) {
			m["steps"] = append(steps(m), map[string]any{"run": []any{"npm", "install", "-g", "x@1.0.0"}, "timeoutSeconds": 10})
		}, want: "non chiama npm"},
		"adapter.id diverso da agent": {edit: func(m map[string]any) { m["adapter"].(map[string]any)["id"] = "codex" }, want: "adapter.id"},
		"contenuto dopo l'oggetto":    {raw: func(raw []byte) []byte { return append(raw, []byte("{}")...) }, want: "dopo l'oggetto"},
		"expiresAt dentro il tetto giusto": {
			edit: func(m map[string]any) { m["expiresAt"] = "2027-05-25T12:00:00Z" }, // esattamente 180 giorni
			want: "",
		},
	}
	for name, c := range cases {
		m := buona()
		if c.edit != nil {
			c.edit(m)
		}
		raw := canon(t, m)
		if c.raw != nil {
			raw = c.raw(raw)
		}
		path := c.path
		if path == "" {
			path = "recipes/opencode-amd64/1.json"
		}
		errs := Check(path, raw, adesso, MaxValidityDays)
		if c.want == "" {
			if len(errs) != 0 {
				t.Errorf("%s: errori inattesi %v", name, errs)
			}
			continue
		}
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), c.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: atteso un errore con %q, avuti %v", name, c.want, errs)
		}
	}
}

func TestControlliDelCanaleNpm(t *testing.T) {
	cases := map[string]struct {
		edit func(m map[string]any)
		want string
	}{
		"ref senza versione": {edit: func(m map[string]any) { m["source"].(map[string]any)["ref"] = "@anthropic-ai/claude-code" }, want: "pacchetto@versione"},
		"ref con latest":     {edit: func(m map[string]any) { m["source"].(map[string]any)["ref"] = "@anthropic-ai/claude-code@latest" }, want: "latest"},
		"nessun npm install": {edit: func(m map[string]any) { m["steps"] = steps(m)[1:] }, want: "nessun passo `npm install -g"},
		"npm con un fetch":   {edit: func(m map[string]any) { m["steps"] = append([]any{steps(buona())[0]}, steps(m)...) }, want: "non ha passi fetch"},
		"hooks senza -yes":   {edit: func(m map[string]any) { m["steps"] = steps(m)[:2] }, want: "hooks install -yes"},
		"versione diversa": {edit: func(m map[string]any) {
			steps(m)[0].(map[string]any)["run"] = []any{"npm", "install", "-g", "@anthropic-ai/claude-code@2.1.331", "--prefix", ".npm-global"}
		}, want: "nessun passo `npm install -g"},
		"npm senza -g (locale)": {edit: func(m map[string]any) {
			steps(m)[0].(map[string]any)["run"] = []any{"npm", "install", "@anthropic-ai/claude-code@2.1.332"}
		}, want: "nessun passo `npm install -g"},
	}
	for name, c := range cases {
		m := npmBuona()
		c.edit(m)
		errs := Check("recipes/claude-code/1.json", canon(t, m), adesso, MaxValidityDays)
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), c.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: atteso un errore con %q, avuti %v", name, c.want, errs)
		}
	}
}

func TestComandoCodiciDiUscita(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "recipes", "opencode-amd64", "1.json")
	if err := os.MkdirAll(filepath.Dir(good), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, canon(t, buona()), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "brutta.json")
	pretty, _ := json.MarshalIndent(buona(), "", "  ")
	if err := os.WriteFile(bad, pretty, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	now := func() time.Time { return adesso }
	if code := run([]string{good}, &out, &errOut, now); code != 0 || !strings.Contains(out.String(), "ok   ") {
		t.Errorf("ricetta buona: codice %d, out %q, err %q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{good, bad}, &out, &errOut, now); code != 1 || !strings.Contains(errOut.String(), "forma canonica") {
		t.Errorf("una buona e una brutta: codice %d, err %q", code, errOut.String())
	}
	if code := run([]string{"-adesso", "2027-06-01T00:00:00Z", good}, &out, &errOut, now); code != 1 || !strings.Contains(errOut.String(), "già passato") {
		t.Errorf("-adesso oltre la scadenza: codice %d, err %q", code, errOut.String())
	}
	if code := run(nil, &out, &errOut, now); code != 2 {
		t.Errorf("senza file: codice %d", code)
	}
	if code := run([]string{"-adesso", "ieri", good}, &out, &errOut, now); code != 2 {
		t.Errorf("-adesso malformato: codice %d", code)
	}
	if code := run([]string{filepath.Join(dir, "manca.json")}, &out, &errOut, now); code != 1 {
		t.Errorf("file assente: codice %d", code)
	}
}
