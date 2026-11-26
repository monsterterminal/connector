package catalog

// Test di corrispondenza (#26, asse B) sulle ricette VERE del repository, recipes/<recipeId>/<ver>.json
// (spec §1.9 riscritta il 2026-11-26, ADR 0410 A4, piano di Fase 3 §3 L4 e decisioni 6-7 come
// modificate dal BOSS il 26/11 in §7.1): ogni ricetta è valida contro objects/recipe.json 0.11.0, è nei
// byte canonici che si firmano, dichiara il canale ufficiale in `source` coerente coi passi, non porta
// «latest» né http, ha `adapter.kind` hooks solo per Claude Code (con `hooks install -yes` ultimo
// passo) e generic per gli altri, e dove esiste la firma del BOSS (`.sig`) la firma si verifica con la
// chiave mt-recipes-2026 incorporata nel binario, attraverso lo stesso ParseRecipe del connettore.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func recipesRoot() string { return filepath.Join("..", "..", "recipes") }

type repoRecipe struct {
	path, sigPath string
	raw           []byte
	sig           string // base64, vuota se il file .sig non c'è
	r             Recipe
}

func loadRepoRecipes(t *testing.T) []repoRecipe {
	t.Helper()
	var out []repoRecipe
	err := filepath.WalkDir(recipesRoot(), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rr := repoRecipe{path: path, sigPath: path + ".sig", raw: raw}
		if sig, err := os.ReadFile(rr.sigPath); err == nil {
			rr.sig = strings.TrimSpace(string(sig))
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rr.r); err != nil {
			t.Errorf("%s: %v", path, err)
			return nil
		}
		out = append(out, rr)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("nessuna ricetta in recipes/: il catalogo della Fase 3 ne prevede almeno tre (L4)")
	}
	return out
}

// TestRicetteDelRepositoryRispettanoLoSchema: ogni ricetta è valida contro objects/recipe.json e passa
// la stessa Validate del connettore (§1.18: una ricetta fuori schema è signature-invalid sull'host).
func TestRicetteDelRepositoryRispettanoLoSchema(t *testing.T) {
	v := newValidator(t)
	for _, rr := range loadRepoRecipes(t) {
		if err := v.validateJSON("objects/recipe.json", rr.raw); err != nil {
			t.Errorf("%s: schema: %v", rr.path, err)
		}
		if err := rr.r.Validate(rr.raw); err != nil {
			t.Errorf("%s: Validate: %v", rr.path, err)
		}
	}
}

// TestRicetteDelRepositorySonoCanonicheECoerentiColPercorso: i byte del file sono la forma canonica
// (sono quelli che il BOSS firma e che il backend manda tal quali) e recipes/<id>/<ver>.json dice il
// vero su recipeId e recipeVersion.
func TestRicetteDelRepositorySonoCanonicheECoerentiColPercorso(t *testing.T) {
	pathRE := regexp.MustCompile(`recipes/([a-z0-9][a-z0-9-]*)/([1-9][0-9]*)\.json$`)
	for _, rr := range loadRepoRecipes(t) {
		canon, err := Canonical(json.RawMessage(rr.raw))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(canon, rr.raw) {
			t.Errorf("%s: non è in forma canonica (jq -cjS .)", rr.path)
		}
		m := pathRE.FindStringSubmatch(filepath.ToSlash(rr.path))
		if m == nil {
			t.Errorf("%s: non è recipes/<recipeId>/<recipeVersion>.json", rr.path)
			continue
		}
		if m[1] != rr.r.RecipeID {
			t.Errorf("%s: recipeId %q", rr.path, rr.r.RecipeID)
		}
		if v, _ := strconv.Atoi(m[2]); v != rr.r.RecipeVersion {
			t.Errorf("%s: recipeVersion %d", rr.path, rr.r.RecipeVersion)
		}
	}
}

// TestRicetteDelRepositoryVersioniFissateECanaleUfficiale: §1.9 «le versioni sono fissate: niente
// latest», solo https, e `source` coerente coi passi (npm → npm install -g pacchetto@versione; release
// → un fetch dell'URL versionato del vendor con sha256), con la scadenza entro il tetto di 180 giorni
// (il «già scaduta» lo controlla la CI con scripts/valida-ricetta.sh: qui si verifica il tetto).
func TestRicetteDelRepositoryVersioniFissateECanaleUfficiale(t *testing.T) {
	latest := regexp.MustCompile(`(?i)(^|[^a-z])latest([^a-z]|$)`)
	for _, rr := range loadRepoRecipes(t) {
		if latest.Match(rr.raw) {
			t.Errorf("%s: contiene «latest»", rr.path)
		}
		if bytes.Contains(bytes.ToLower(rr.raw), []byte("http://")) {
			t.Errorf("%s: contiene http://", rr.path)
		}
		var fetched []string
		npmRef := false
		for _, s := range rr.r.Steps {
			if s.Fetch != nil {
				fetched = append(fetched, s.Fetch.URL)
			} else if filepath.Base(s.Run[0]) == "npm" && in(rr.r.Source.Ref, s.Run) {
				npmRef = true
			}
		}
		switch rr.r.Source.Channel {
		case "npm":
			if !strings.Contains(rr.r.Source.Ref, "@") || !npmRef || len(fetched) > 0 {
				t.Errorf("%s: source npm %q non coerente coi passi", rr.path, rr.r.Source.Ref)
			}
			if !hasRequire(rr.r, "node") {
				t.Errorf("%s: una ricetta npm richiede node (§1.9: node solo dove il canale è npm)", rr.path)
			}
		case "release":
			if !in(rr.r.Source.Ref, fetched) {
				t.Errorf("%s: source release %q non è scaricato da un passo fetch", rr.path, rr.r.Source.Ref)
			}
			if hasRequire(rr.r, "node") {
				t.Errorf("%s: una ricetta release non richiede node (§1.9)", rr.path)
			}
		}
		exp, err := time.Parse(time.RFC3339Nano, rr.r.ExpiresAt)
		if err != nil {
			t.Errorf("%s: expiresAt: %v", rr.path, err)
		} else if exp.After(time.Now().Add(181 * 24 * time.Hour)) {
			t.Errorf("%s: expiresAt %s oltre 180 giorni da oggi", rr.path, rr.r.ExpiresAt)
		}
	}
}

func hasRequire(r Recipe, name string) bool {
	for _, q := range r.Requires {
		if q.Name == name {
			return true
		}
	}
	return false
}

// TestRicetteDelRepositoryAdattatoreCoerente: decisione 7 del piano di Fase 3 — Claude Code `hooks`
// con `monsterterminal-connector hooks install -yes` come ultimo passo (la conferma dell'owner vale come
// consenso), Codex e OpenCode `generic` finché la Fase 2 non porta l'adattatore; adapter.id = agent.
func TestRicetteDelRepositoryAdattatoreCoerente(t *testing.T) {
	seen := map[string]bool{}
	for _, rr := range loadRepoRecipes(t) {
		seen[rr.r.Agent] = true
		if rr.r.Adapter.ID != rr.r.Agent {
			t.Errorf("%s: adapter.id %q diverso da agent %q", rr.path, rr.r.Adapter.ID, rr.r.Agent)
		}
		last := rr.r.Steps[len(rr.r.Steps)-1].Run
		hooksStep := strings.Join(last, " ") == "/usr/local/bin/monsterterminal-connector hooks install -yes"
		switch rr.r.Agent {
		case "claude-code":
			if rr.r.Adapter.Kind != "hooks" {
				t.Errorf("%s: Claude Code vuole adapter.kind hooks, trovato %q", rr.path, rr.r.Adapter.Kind)
			}
			if !hooksStep {
				t.Errorf("%s: l'ultimo passo di Claude Code deve essere hooks install -yes, trovato %q", rr.path, last)
			}
		case "codex", "opencode":
			if rr.r.Adapter.Kind != "generic" {
				t.Errorf("%s: %s vuole adapter.kind generic in Fase 3, trovato %q", rr.path, rr.r.Agent, rr.r.Adapter.Kind)
			}
			if hooksStep {
				t.Errorf("%s: %s non installa hook in Fase 3", rr.path, rr.r.Agent)
			}
		default:
			t.Errorf("%s: agente %q fuori dai tre della Fase 3", rr.path, rr.r.Agent)
		}
	}
	for _, a := range []string{"claude-code", "codex", "opencode"} {
		if !seen[a] {
			t.Errorf("nessuna ricetta per %s", a)
		}
	}
}

// TestRicetteDelRepositoryFirmate: dove c'è <ver>.json.sig la firma è di mt-recipes-2026 sui byte esatti
// del file, verificata con lo stesso ParseRecipe del connettore (chiave nil = quella incorporata).
// Senza .sig la ricetta è una bozza in attesa della firma del BOSS: il test lo dice e non fallisce.
func TestRicetteDelRepositoryFirmate(t *testing.T) {
	for _, rr := range loadRepoRecipes(t) {
		if rr.sig == "" {
			t.Logf("%s: senza firma (bozza in attesa della firma del BOSS)", rr.path)
			continue
		}
		r, err := ParseRecipe(rr.raw, rr.sig, nil)
		if err != nil {
			t.Errorf("%s: firma %s non valida con mt-recipes-2026: %v", rr.path, rr.sigPath, err)
			continue
		}
		if r.RecipeID != rr.r.RecipeID {
			t.Errorf("%s: ParseRecipe legge %q", rr.path, r.RecipeID)
		}
		// Un byte in più e la stessa firma non vale più: si firma il file, non il suo significato.
		if _, err := ParseRecipe(append(append([]byte{}, rr.raw...), '\n'), rr.sig, nil); err == nil {
			t.Errorf("%s: la firma vale anche con un a capo in più", rr.path)
		}
	}
}
