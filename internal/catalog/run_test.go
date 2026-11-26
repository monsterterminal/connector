package catalog

// Test dell'esecuzione dei passi (spec §1.9, §1.18, ADR 0410 A4): un passo che fallisce ferma i
// successivi, timeout, logTail troncato a 8 KiB, niente shell, {work} solo all'inizio, fetch con
// sha256, tetto, https e dest nella cartella di lavoro.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

func runRecipe(steps ...Step) Recipe {
	r := testRecipe()
	r.Steps = steps
	return r
}

func runStep(timeout int, args ...string) Step { return Step{Run: args, TimeoutSeconds: timeout} }

func runCfg(t *testing.T) (RunConfig, *[]string) {
	t.Helper()
	var progress []string
	return RunConfig{
		WorkDir: filepath.Join(t.TempDir(), "catalog", "work", "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b"),
		Home:    t.TempDir(),
		Progress: func(step, of int, msg string) {
			progress = append(progress, msg)
			if step < 1 || step > of {
				t.Errorf("progresso %d di %d", step, of)
			}
		},
	}, &progress
}

func TestRunPassiInOrdineEProgressoDaUno(t *testing.T) {
	cfg, progress := runCfg(t)
	marker := filepath.Join(t.TempDir(), "m")
	f := Run(context.Background(), runRecipe(runStep(5, "/bin/true"), runStep(5, "/usr/bin/touch", marker)), cfg)
	if f != nil {
		t.Fatalf("passi riusciti segnalati come falliti: %v", f)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("secondo passo non eseguito")
	}
	if len(*progress) != 2 || !strings.HasPrefix((*progress)[0], "passo 1 di 2") || !strings.HasPrefix((*progress)[1], "passo 2 di 2") {
		t.Fatalf("progresso: %q", *progress)
	}
	if _, err := os.Stat(cfg.WorkDir); !os.IsNotExist(err) {
		t.Fatal("cartella di lavoro rimasta dopo il successo")
	}
}

func TestRunPassoCheFallisceFermaTutto(t *testing.T) {
	cfg, progress := runCfg(t)
	marker := filepath.Join(t.TempDir(), "dopo")
	f := Run(context.Background(), runRecipe(
		runStep(5, "/bin/true"),
		runStep(5, "/bin/sh", "-c", "echo rotto >&2; exit 3"),
		runStep(5, "/usr/bin/touch", marker),
	), cfg)
	if f == nil || f.Reason != protocol.ReasonStepFailed || f.StepIndex != 1 {
		t.Fatalf("esito: %+v", f)
	}
	if !strings.Contains(f.LogTail, "rotto") || !strings.Contains(f.Detail, "exit status 3") {
		t.Fatalf("logTail o detail: %+v", f)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("il passo dopo quello fallito è stato eseguito")
	}
	if len(*progress) != 1 {
		t.Fatalf("progresso dopo il fallimento: %q", *progress)
	}
	if _, err := os.Stat(cfg.WorkDir); !os.IsNotExist(err) {
		t.Fatal("cartella di lavoro rimasta dopo il fallimento")
	}
	// Un programma che non esiste è step-failed, non un errore del connettore.
	cfg2, _ := runCfg(t)
	if f := Run(context.Background(), runRecipe(runStep(5, "/non/esiste")), cfg2); f == nil || f.Reason != protocol.ReasonStepFailed || f.StepIndex != 0 {
		t.Fatalf("programma assente: %+v", f)
	}
}

func TestRunTimeout(t *testing.T) {
	cfg, _ := runCfg(t)
	cfg.TimeoutUnit = 100 * time.Millisecond
	start := time.Now()
	// sh lancia sleep come figlio: oltre il tempo si uccide il gruppo intero.
	f := Run(context.Background(), runRecipe(runStep(2, "/bin/sh", "-c", "echo parto; sleep 30; echo mai")), cfg)
	if f == nil || f.Reason != protocol.ReasonTimeout || f.StepIndex != 0 {
		t.Fatalf("esito: %+v", f)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("il timeout non ha fermato il passo: %s", d)
	}
	if !strings.Contains(f.LogTail, "parto") || strings.Contains(f.LogTail, "mai") {
		t.Fatalf("logTail: %q", f.LogTail)
	}
}

func TestRunLogTailTroncatoA8KiB(t *testing.T) {
	cfg, _ := runCfg(t)
	f := Run(context.Background(), runRecipe(runStep(10, "/bin/sh", "-c", "head -c 20000 /dev/zero | tr '\\0' a; echo; echo FINE; exit 1")), cfg)
	if f == nil || f.Reason != protocol.ReasonStepFailed {
		t.Fatalf("esito: %+v", f)
	}
	if len(f.LogTail) != protocol.MaxCatalogLogTail || !strings.HasSuffix(f.LogTail, "FINE\n") {
		t.Fatalf("logTail di %d byte, fine %q", len(f.LogTail), f.LogTail[len(f.LogTail)-10:])
	}
}

// Niente shell: un argomento con `|`, `;`, `$` o `>` arriva tal quale al programma; {work} si
// sostituisce solo all'inizio di un argomento.
func TestRunSenzaShellEWorkSoloAllInizio(t *testing.T) {
	cfg, _ := runCfg(t)
	out := filepath.Join(t.TempDir(), "argomenti")
	// Il programma è sh SOLO per scrivere i suoi argomenti posizionali in un file: gli argomenti
	// passano come $1…, mai interpretati.
	f := Run(context.Background(), runRecipe(runStep(5, "/bin/sh", "-c", `printf '%s\n' "$@" > "$0"`, out,
		"a|b", "x;rm -rf /tmp/no", "$HOME", "{work}/file", "pre{work}", "{work}")), cfg)
	if f != nil {
		t.Fatalf("passo fallito: %+v", f)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"a|b", "x;rm -rf /tmp/no", "$HOME", cfg.WorkDir + "/file", "pre{work}", cfg.WorkDir}, "\n") + "\n"
	if string(raw) != want {
		t.Fatalf("argomenti ricevuti:\n%s\nattesi:\n%s", raw, want)
	}
	if got := ExpandArgs([]string{"{work}{work}", "{WORK}/x", " {work}"}, "/w"); strings.Join(got, ",") != "/w{work},{WORK}/x, {work}" {
		t.Fatalf("ExpandArgs: %q", got)
	}
}

// Un passo run gira nella home dell'utente del connettore: i percorsi relativi sono relativi alla home.
func TestRunCartellaCorrenteEHome(t *testing.T) {
	cfg, _ := runCfg(t)
	f := Run(context.Background(), runRecipe(runStep(5, "/usr/bin/touch", "creato-qui")), cfg)
	if f != nil {
		t.Fatal(f)
	}
	if _, err := os.Stat(filepath.Join(cfg.Home, "creato-qui")); err != nil {
		t.Fatal("il percorso relativo non è finito nella home")
	}
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func fetchServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tool":
			_, _ = w.Write(body)
		case "/v1/redir-http":
			http.Redirect(w, r, "http://example.invalid/x", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fetchClient(srv *httptest.Server) *http.Client {
	c := srv.Client()
	c.CheckRedirect = DefaultHTTPClient().CheckRedirect
	return c
}

func TestFetchConChecksumGiustoEBitDiEsecuzione(t *testing.T) {
	body := []byte("#!/bin/sh\necho ciao\n")
	srv := fetchServer(t, body)
	cfg, _ := runCfg(t)
	cfg.HTTPClient = fetchClient(srv)
	// Il passo dopo il fetch controlla dalla cartella di lavoro che il file ci sia e sia eseguibile.
	f := Run(context.Background(), runRecipe(
		Step{Fetch: &Fetch{URL: srv.URL + "/v1/tool", SHA256: sum(body), Dest: "tool", Executable: true}, TimeoutSeconds: 10},
		runStep(5, "/usr/bin/test", "-x", "{work}/tool"),
		Step{Fetch: &Fetch{URL: srv.URL + "/v1/tool", SHA256: sum(body), Dest: "dati"}, TimeoutSeconds: 10},
		runStep(5, "/bin/sh", "-c", `test -f "$0" && ! test -x "$0"`, "{work}/dati"),
	), cfg)
	if f != nil {
		t.Fatalf("fetch: %+v", f)
	}
}

func TestFetchChecksumDiversoNessunFileLasciato(t *testing.T) {
	body := []byte("artefatto")
	srv := fetchServer(t, body)
	cfg, _ := runCfg(t)
	cfg.HTTPClient = fetchClient(srv)
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := fetch(context.Background(), cfg, Fetch{URL: srv.URL + "/v1/tool", SHA256: sum([]byte("altro")), Dest: "tool"}, 10*time.Second)
	if f == nil || f.Reason != protocol.ReasonChecksumMismatch || !strings.Contains(f.Detail, sum(body)) {
		t.Fatalf("esito: %+v", f)
	}
	entries, _ := os.ReadDir(cfg.WorkDir)
	if len(entries) != 0 {
		t.Fatalf("file lasciati nella cartella di lavoro: %v", entries)
	}
	// Dentro Run: motivo checksum-mismatch con l'indice del passo, e i passi dopo non partono.
	cfg2, _ := runCfg(t)
	cfg2.HTTPClient = fetchClient(srv)
	marker := filepath.Join(t.TempDir(), "dopo")
	rf := Run(context.Background(), runRecipe(runStep(5, "/bin/true"),
		Step{Fetch: &Fetch{URL: srv.URL + "/v1/tool", SHA256: sum([]byte("altro")), Dest: "tool"}, TimeoutSeconds: 10},
		runStep(5, "/usr/bin/touch", marker)), cfg2)
	if rf == nil || rf.Reason != protocol.ReasonChecksumMismatch || rf.StepIndex != 1 {
		t.Fatalf("Run: %+v", rf)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("passo eseguito dopo il checksum diverso")
	}
}

func TestFetchRifiuti(t *testing.T) {
	body := make([]byte, 100)
	srv := fetchServer(t, body)
	cfg, _ := runCfg(t)
	cfg.HTTPClient = fetchClient(srv)
	cfg.MaxFetch = 10
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]Fetch{
		"url http":        {URL: "http://example.com/v1/tool", SHA256: sum(body), Dest: "tool"},
		"dest con barra":  {URL: srv.URL + "/v1/tool", SHA256: sum(body), Dest: "../tool"},
		"dest assoluto":   {URL: srv.URL + "/v1/tool", SHA256: sum(body), Dest: "/etc/passwd"},
		"dest nascosto":   {URL: srv.URL + "/v1/tool", SHA256: sum(body), Dest: ".profile"},
		"oltre il tetto":  {URL: srv.URL + "/v1/tool", SHA256: sum(body), Dest: "tool"},
		"404":             {URL: srv.URL + "/v2/nulla", SHA256: sum(body), Dest: "tool"},
		"redirect a http": {URL: srv.URL + "/v1/redir-http", SHA256: sum(body), Dest: "tool"},
	}
	for name, fx := range cases {
		f := fetch(context.Background(), cfg, fx, 10*time.Second)
		if f == nil || f.Reason != protocol.ReasonStepFailed {
			t.Errorf("%s: %+v", name, f)
		}
	}
	entries, _ := os.ReadDir(cfg.WorkDir)
	if len(entries) != 0 {
		t.Fatalf("file lasciati: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.WorkDir), "tool")); !os.IsNotExist(err) {
		t.Fatal("dest uscito dalla cartella di lavoro")
	}
	if MaxFetchBytes != 256<<20 {
		t.Fatalf("tetto di scarico %d, la spec dice 256 MiB", MaxFetchBytes)
	}
}

func TestFetchTimeout(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	cfg, _ := runCfg(t)
	cfg.HTTPClient = srv.Client()
	cfg.TimeoutUnit = 100 * time.Millisecond
	f := Run(context.Background(), runRecipe(Step{Fetch: &Fetch{URL: srv.URL + "/v1/x", SHA256: sum(nil), Dest: "x"}, TimeoutSeconds: 1}), cfg)
	if f == nil || f.Reason != protocol.ReasonTimeout || f.StepIndex != 0 {
		t.Fatalf("esito: %+v", f)
	}
}
