// Test del download (ADR 0438 D3 punto 1): tetto di 64 MiB, timeout, temporaneo + rinomina, SHA-256
// verificato, mai un file eseguibile.
package update

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
)

func serveBody(t *testing.T, status int, body []byte, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func shaOf(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestDownloadScriveBinarioVerificato(t *testing.T) {
	body := []byte("binario di prova, non eseguibile")
	srv := serveBody(t, 200, body, 0)
	dir := filepath.Join(t.TempDir(), "update")
	// Un apply.request di un giro precedente non deve sopravvivere al nuovo download.
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, FileApplyRequest), []byte("0.0.1\n"), 0o600)
	path, err := Download(context.Background(), srv.Client(), srv.URL+"/x", shaOf(body), dir, time.Second)
	if err != nil {
		t.Fatalf("download buono rifiutato: %v", err)
	}
	if path != filepath.Join(dir, FileBinary) {
		t.Fatalf("percorso %s", path)
	}
	got, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if string(got) != string(body) || st.Mode().Perm() != 0o600 {
		t.Fatalf("file %q modo %v", got, st.Mode())
	}
	if _, err := os.Stat(filepath.Join(dir, FileApplyRequest)); !os.IsNotExist(err) {
		t.Fatal("apply.request residuo non tolto")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporanei rimasti: %v", entries)
	}
}

func TestDownloadRifiuta(t *testing.T) {
	body := []byte("contenuto")
	saved := binaryLimit
	binaryLimit = 16
	t.Cleanup(func() { binaryLimit = saved })
	cases := []struct {
		name   string
		srv    *httptest.Server
		sha    string
		reason string
	}{
		{"sha diverso", serveBody(t, 200, body, 0), strings.Repeat("00", 32), ReasonSHA256Mismatch},
		{"HTTP 404", serveBody(t, 404, body, 0), shaOf(body), ReasonDownloadFailed},
		{"oltre il tetto di un byte", serveBody(t, 200, []byte(strings.Repeat("x", 17)), 0), shaOf([]byte(strings.Repeat("x", 17))), ReasonTooLarge},
		{"tempo scaduto", serveBody(t, 200, body, 300*time.Millisecond), shaOf(body), ReasonDownloadFailed},
	}
	for _, c := range cases {
		dir := filepath.Join(t.TempDir(), "update")
		_, err := Download(context.Background(), c.srv.Client(), c.srv.URL+"/x", c.sha, dir, 100*time.Millisecond)
		if got := ReasonOf(err, ""); err == nil || got != c.reason {
			t.Errorf("%s: %v (motivo %q, atteso %q)", c.name, err, got, c.reason)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s: file rimasti %v", c.name, entries)
		}
	}
	// Esattamente il tetto passa.
	exact := []byte(strings.Repeat("y", 16))
	srv := serveBody(t, 200, exact, 0)
	if _, err := Download(context.Background(), srv.Client(), srv.URL+"/x", shaOf(exact), filepath.Join(t.TempDir(), "u"), time.Second); err != nil {
		t.Fatalf("file al tetto rifiutato: %v", err)
	}
	// Un URL non raggiungibile è download-failed.
	if _, err := Download(context.Background(), &http.Client{}, "https://127.0.0.1:1/x", shaOf(body), t.TempDir(), 200*time.Millisecond); ReasonOf(err, "") != ReasonDownloadFailed {
		t.Fatalf("connessione rifiutata: %v", err)
	}
}

func TestWriteRequestERemoveDownload(t *testing.T) {
	dir := t.TempDir()
	if err := WriteRequest(dir, []byte(`{"a":1}`), []byte(strings.Repeat("s", 64)), "0.7.0"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{FileManifest: `{"a":1}`, FileSignature: strings.Repeat("s", 64), FileApplyRequest: "0.7.0\n"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
	if v, err := ReadRequestVersion(dir); err != nil || v != "0.7.0" {
		t.Fatalf("versione della richiesta: %q %v", v, err)
	}
	RemoveDownload(dir)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("file rimasti: %v", entries)
	}
	if _, err := ReadRequestVersion(dir); err == nil {
		t.Fatal("richiesta assente letta")
	}
	_ = os.WriteFile(filepath.Join(dir, FileApplyRequest), []byte("non-una-versione\n"), 0o600)
	if _, err := ReadRequestVersion(dir); err == nil {
		t.Fatal("richiesta con versione non semver accettata")
	}
}
