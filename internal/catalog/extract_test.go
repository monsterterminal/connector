package catalog

// Test di `catalog extract` e `catalog path-add` (spec §1.9, §1.18): archivi tar.gz e zip aperti senza
// shell, nessuna voce fuori dalla cartella, niente symlink che escono, tetto di dimensione, risultato
// idempotente; una riga sola in ~/.profile, cartelle relative alla home.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func writeTarGz(t *testing.T, entries ...entry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Size: int64(len(e.body)), Linkname: e.link}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeZip(t *testing.T, entries ...entry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		fh := &zip.FileHeader{Name: e.name}
		mode := os.FileMode(0o644)
		if e.link != "" {
			mode = os.ModeSymlink | 0o777
		}
		fh.SetMode(mode)
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		body := e.body
		if e.link != "" {
			body = e.link
		}
		_, _ = w.Write([]byte(body))
	}
	zw.Close()
	p := filepath.Join(t.TempDir(), "a.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractTarGzEIdempotente(t *testing.T) {
	arc := writeTarGz(t,
		entry{name: "opencode/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "opencode/bin/opencode", body: "#!/bin/sh\n", mode: 0o4755},
		entry{name: "opencode/README", body: "leggimi"},
		entry{name: "opencode/latest", typ: tar.TypeSymlink, link: "bin/opencode"},
	)
	dest := filepath.Join(t.TempDir(), "dest")
	for i := 0; i < 2; i++ { // la seconda volta sostituisce: stesso risultato
		if err := Extract(arc, dest, ExtractLimits{}); err != nil {
			t.Fatalf("giro %d: %v", i, err)
		}
	}
	fi, err := os.Stat(filepath.Join(dest, "opencode/bin/opencode"))
	if err != nil || fi.Mode().Perm() != 0o755 || fi.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("binario: %v %v", fi.Mode(), err)
	}
	if l, err := os.Readlink(filepath.Join(dest, "opencode/latest")); err != nil || l != "bin/opencode" {
		t.Fatalf("symlink interno: %q %v", l, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "opencode/README")); string(b) != "leggimi" {
		t.Fatalf("contenuto: %q", b)
	}
}

func TestExtractZip(t *testing.T) {
	arc := writeZip(t, entry{name: "codex/codex", body: "bin"}, entry{name: "codex/link", link: "codex"})
	dest := filepath.Join(t.TempDir(), "dest")
	if err := Extract(arc, dest, ExtractLimits{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "codex/link")); string(b) != "bin" {
		t.Fatalf("zip: %q", b)
	}
}

// Path traversal: nessuna voce scrive fuori dalla cartella, in nessuna forma.
func TestExtractTraversalRifiutato(t *testing.T) {
	cases := map[string]string{
		"dot-dot":                writeTarGz(t, entry{name: "../evaso", body: "x"}),
		"dot-dot annidato":       writeTarGz(t, entry{name: "a/../../evaso", body: "x"}),
		"assoluto":               writeTarGz(t, entry{name: "/tmp/evaso-assoluto", body: "x"}),
		"symlink assoluto":       writeTarGz(t, entry{name: "l", typ: tar.TypeSymlink, link: "/etc"}),
		"symlink che sale":       writeTarGz(t, entry{name: "a/l", typ: tar.TypeSymlink, link: "../../x"}),
		"collegamento fisico":    writeTarGz(t, entry{name: "h", typ: tar.TypeLink, link: "/etc/passwd"}),
		"dispositivo":            writeTarGz(t, entry{name: "d", typ: tar.TypeChar}),
		"zip con dot-dot":        writeZip(t, entry{name: "../evaso", body: "x"}),
		"zip con symlink fuori":  writeZip(t, entry{name: "l", link: "../../etc"}),
		"barra rovescia dot-dot": writeZip(t, entry{name: "..\\evaso", body: "x"}),
	}
	for name, arc := range cases {
		parent := t.TempDir()
		dest := filepath.Join(parent, "dest")
		err := Extract(arc, dest, ExtractLimits{})
		if !errors.Is(err, ErrUnsafeEntry) {
			t.Errorf("%s: atteso ErrUnsafeEntry, ottenuto %v", name, err)
		}
		if entries, _ := os.ReadDir(parent); len(entries) != 1 {
			t.Errorf("%s: scritto fuori dalla cartella: %v", name, entries)
		}
	}
	if _, err := os.Stat("/tmp/evaso-assoluto"); err == nil {
		t.Fatal("voce assoluta scritta in /tmp")
	}
}

// Una voce che passa da un symlink interno preesistente resta comunque dentro la cartella (os.Root).
func TestExtractNonEsceAttraversoUnSymlinkPreesistente(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	// Un symlink messo da qualcun altro nella cartella di destinazione, che punta fuori.
	if err := os.Symlink(parent, filepath.Join(dest, "fuga")); err != nil {
		t.Fatal(err)
	}
	arc := writeTarGz(t, entry{name: "fuga/evaso", body: "x"})
	if err := Extract(arc, dest, ExtractLimits{}); err == nil {
		t.Fatal("scrittura attraverso un symlink che esce accettata")
	}
	if _, err := os.Stat(filepath.Join(parent, "evaso")); err == nil {
		t.Fatal("file scritto fuori dalla cartella")
	}
}

func TestExtractTetti(t *testing.T) {
	big := writeTarGz(t, entry{name: "a", body: strings.Repeat("x", 100)})
	if err := Extract(big, filepath.Join(t.TempDir(), "d"), ExtractLimits{MaxBytes: 10}); err == nil || !strings.Contains(err.Error(), "oltre") {
		t.Fatalf("oltre il tetto di byte: %v", err)
	}
	many := writeTarGz(t, entry{name: "a", body: "1"}, entry{name: "b", body: "2"}, entry{name: "c", body: "3"})
	if err := Extract(many, filepath.Join(t.TempDir(), "d"), ExtractLimits{MaxEntries: 2}); err == nil {
		t.Fatal("oltre il tetto di voci accettato")
	}
	notArc := filepath.Join(t.TempDir(), "x.tar.gz")
	_ = os.WriteFile(notArc, []byte("non sono un archivio"), 0o600)
	if err := Extract(notArc, filepath.Join(t.TempDir(), "d"), ExtractLimits{}); err == nil {
		t.Fatal("file qualunque estratto")
	}
}

func TestPathAddIdempotente(t *testing.T) {
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, ".profile"), []byte("# mio profilo\nexport X=1"), 0o644)
	changed, line, err := PathAdd(home, ".local/opencode")
	if err != nil || !changed || line != `export PATH="$HOME/.local/opencode:$PATH" `+ProfileMarker {
		t.Fatalf("prima volta: %v %q %v", changed, line, err)
	}
	for i := 0; i < 2; i++ {
		if changed, _, err := PathAdd(home, ".local/opencode"); err != nil || changed {
			t.Fatalf("ripetuto: %v %v", changed, err)
		}
	}
	if _, _, err := PathAdd(home, "/opt/agente/bin"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".profile"))
	want := "# mio profilo\nexport X=1\n" + line + "\n" + `export PATH="/opt/agente/bin:$PATH" ` + ProfileMarker + "\n"
	if string(raw) != want {
		t.Fatalf(".profile:\n%s\natteso:\n%s", raw, want)
	}
	dirs := ProfileDirs(home)
	if len(dirs) != 2 || dirs[0] != filepath.Join(home, ".local/opencode") || dirs[1] != "/opt/agente/bin" {
		t.Fatalf("ProfileDirs: %v", dirs)
	}
	// Senza ~/.profile lo crea.
	home2 := t.TempDir()
	if changed, _, err := PathAdd(home2, ".npm-global/bin"); err != nil || !changed {
		t.Fatal(err)
	}
}

func TestResolveDirRifiuti(t *testing.T) {
	home := "/home/mt"
	for _, d := range []string{"", "~/.local/bin", "../altro", "a/../../b", `a"b`, "a$(rm)", "a;b", "a\nb", "a`b`"} {
		if _, err := ResolveDir(home, d); err == nil {
			t.Errorf("%q accettato", d)
		}
	}
	if p, err := ResolveDir(home, ".local/bin"); err != nil || p != "/home/mt/.local/bin" {
		t.Fatalf("relativo: %q %v", p, err)
	}
	if p, err := ResolveDir(home, "/opt/x/"); err != nil || p != "/opt/x" {
		t.Fatalf("assoluto: %q %v", p, err)
	}
	if _, err := ResolveDir("", ".local/bin"); err == nil {
		t.Fatal("relativo senza home accettato")
	}
}
