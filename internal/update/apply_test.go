// Test dell'updater root (ADR 0438 D3 punti 2-4, A1; decisione BOSS 2026-11-17: l'updater NON si fida
// di ciò che il connettore ha preparato; revisione di sicurezza 2026-11-17). Niente root e niente
// systemd qui: il binario installato è un file in una cartella temporanea, systemctl è una funzione
// che registra le chiamate, la cartella dell'updater sta ACCANTO a quella di stato.
package update

import (
	"crypto/ed25519"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

type bench struct {
	base, state, updateDir, updaterDir, bin, conf string
	pub                                           ed25519.PublicKey
	priv                                          ed25519.PrivateKey
	calls                                         []string
}

func newBench(t *testing.T) *bench {
	t.Helper()
	root := t.TempDir()
	b := &bench{base: root, state: filepath.Join(root, "state"), bin: filepath.Join(root, "bin", "monsterterminal-connector"), conf: filepath.Join(root, "connector.conf")}
	b.updateDir = filepath.Join(b.state, UpdateSubdir)
	b.updaterDir = UpdaterDirFor(b.state)
	if err := os.MkdirAll(b.updateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(b.bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.bin, []byte("binario installato 0.6.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.pub, b.priv = keypair(t)
	return b
}

func (b *bench) cfg() ApplyConfig {
	return ApplyConfig{
		StateDir:    b.state,
		TrustedBase: b.base,
		ConfigFile:  b.conf,
		Binary:      b.bin,
		Running:     "0.6.1",
		Platform:    Platform{OS: "linux", Arch: "amd64", Protocol: 1, Version: "0.6.1"},
		Key:         b.pub,
		Systemctl:   func(args ...string) error { b.calls = append(b.calls, strings.Join(args, " ")); return nil },
		UID:         os.Getuid(),
		ServiceUID:  -1, // l'owner di <state>; TestApplyUtenteDelServizio lo mette esplicito
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
}

func (b *bench) mailbox(t *testing.T) *Mailbox {
	t.Helper()
	mb, err := OpenMailbox(b.updateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mb.Close() })
	return mb
}

// prepare lascia nella cassetta ciò che il connettore avrebbe preparato: binario, manifest canonico,
// firma e richiesta. sig nil = firma con la chiave del banco.
func (b *bench) prepare(t *testing.T, body []byte, m protocol.UpdateManifest, sig []byte) {
	t.Helper()
	if m.SHA256 == "" {
		m.SHA256 = shaOf(body)
	}
	canon, err := Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	if sig == nil {
		sig = ed25519.Sign(b.priv, canon)
	}
	if err := os.WriteFile(filepath.Join(b.updateDir, FileBinary), body, 0o600); err != nil {
		t.Fatal(err)
	}
	mb := b.mailbox(t)
	if err := mb.WriteRequest(canon, sig, m.Version); err != nil {
		t.Fatal(err)
	}
}

func (b *bench) rejected(t *testing.T) Outcome {
	t.Helper()
	o, ok, err := b.mailbox(t).ReadOutcome(FileApplyRejected)
	if !ok || err != nil {
		t.Fatalf("apply.rejected assente o illeggibile: %v %v", ok, err)
	}
	return o
}

func (b *bench) binaryIs(t *testing.T, want string) {
	t.Helper()
	got, err := os.ReadFile(b.bin)
	if err != nil || string(got) != want {
		t.Fatalf("binario installato %q (%v), atteso %q", got, err, want)
	}
}

func (b *bench) noRequest(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(b.updateDir, FileApplyRequest)); !os.IsNotExist(err) {
		t.Fatal("apply.request non tolto")
	}
}

func (b *bench) pending(t *testing.T) (Pending, bool) {
	t.Helper()
	st, err := OpenStore(b.updaterDir, os.Getuid(), b.base)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, ok, err := st.ReadPending()
	if err != nil {
		t.Fatal(err)
	}
	return p, ok
}

func newManifest(version string) protocol.UpdateManifest {
	return protocol.UpdateManifest{Arch: "amd64", MinProtocol: 1, OS: "linux",
		URL: ReleasesPrefix + "v" + version + "/monsterterminal-connector_" + version + "_linux_amd64", Version: version}
}

func TestApplyInstallaUnAggiornamentoVerificato(t *testing.T) {
	b := newBench(t)
	body := []byte("binario nuovo 0.7.0")
	b.prepare(t, body, newManifest("0.7.0"), nil)
	if err := Apply(b.cfg()); err != nil {
		t.Fatalf("aggiornamento buono rifiutato: %v", err)
	}
	b.binaryIs(t, string(body))
	st, _ := os.Stat(b.bin)
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("binario installato con modo %v", st.Mode())
	}
	if prev, _ := os.ReadFile(b.bin + PrevSuffix); string(prev) != "binario installato 0.6.1" {
		t.Fatalf(".prev %q", prev)
	}
	if _, err := os.Lstat(b.bin + NewSuffix); !os.IsNotExist(err) {
		t.Fatal(".new rimasto dopo la rinomina")
	}
	p, ok := b.pending(t)
	if !ok || p.Version != "0.7.0" || p.PreviousVersion != "0.6.1" || p.At == "" {
		t.Fatalf("pending.json: %+v %v", p, ok)
	}
	if a, ok, _ := b.mailbox(t).ReadApplied(); !ok || a != p {
		t.Fatalf("applied.json: %+v %v (pending %+v)", a, ok, p)
	}
	if st, _ := os.Stat(b.updaterDir); st.Mode().Perm() != 0o700 || filepath.Dir(b.updaterDir) != filepath.Dir(b.state) {
		t.Fatalf("cartella dell'updater %s modo %v: deve stare accanto a quella di stato, root 0700", b.updaterDir, st.Mode())
	}
	b.noRequest(t)
	for _, n := range []string{FileBinary, FileManifest, FileSignature} {
		if _, err := os.Lstat(filepath.Join(b.updaterDir, n)); !os.IsNotExist(err) {
			t.Errorf("copia %s rimasta nella cartella dell'updater", n)
		}
	}
	if got := strings.Join(b.calls, ";"); got != "restart "+RollbackTimer+";restart "+ServiceName {
		t.Fatalf("systemctl: %s", got)
	}
	if _, ok, _ := b.mailbox(t).ReadOutcome(FileApplyRejected); ok {
		t.Fatal("apply.rejected scritto su un'installazione riuscita")
	}
}

// Il test negativo obbligatorio dell'ADR 0438 D3: ciò che l'utente del connettore mette in update/
// senza una firma valida NON si installa, mai.
func TestApplyRifiutaCioCheNonEFirmato(t *testing.T) {
	_, otherPriv := keypair(t)
	body := []byte("binario che l'utente vorrebbe far installare")
	cases := []struct {
		name   string
		set    func(b *bench)
		reason string
	}{
		{"firma assente (zeri)", func(b *bench) { b.prepare(t, body, newManifest("0.7.0"), make([]byte, 64)) }, ReasonBadSignature},
		{"firma di un'altra chiave (per esempio quella delle ricette)", func(b *bench) {
			canon, _ := Canonical(withSHA(newManifest("0.7.0"), body))
			b.prepare(t, body, newManifest("0.7.0"), ed25519.Sign(otherPriv, canon))
		}, ReasonBadSignature},
		{"firma valida ma binario con sha diverso", func(b *bench) {
			m := withSHA(newManifest("0.7.0"), []byte("un altro binario"))
			b.prepare(t, body, m, nil)
		}, ReasonSHA256Mismatch},
		{"firma valida ma versione non maggiore", func(b *bench) { b.prepare(t, body, newManifest("0.6.1"), nil) }, ReasonNotNewer},
		{"firma valida ma altra piattaforma", func(b *bench) {
			m := newManifest("0.7.0")
			m.Arch = "arm64"
			b.prepare(t, body, m, nil)
		}, ReasonWrongPlatform},
		{"firma valida ma protocollo oltre il nostro", func(b *bench) {
			m := newManifest("0.7.0")
			m.MinProtocol = 2
			b.prepare(t, body, m, nil)
		}, ReasonProtocolTooOld},
		{"firma valida ma url fuori dal repository", func(b *bench) {
			m := newManifest("0.7.0")
			m.URL = "https://releases.banco/x"
			b.prepare(t, body, m, nil)
		}, ReasonBadURL},
		{"manifest non JSON", func(b *bench) {
			b.prepare(t, body, newManifest("0.7.0"), nil)
			_ = os.WriteFile(filepath.Join(b.updateDir, FileManifest), []byte("{non json"), 0o600)
		}, ReasonBadSignature},
		{"manifest con un campo in più", func(b *bench) {
			b.prepare(t, body, newManifest("0.7.0"), nil)
			_ = os.WriteFile(filepath.Join(b.updateDir, FileManifest), []byte(`{"arch":"amd64","extra":1,"minProtocol":1,"os":"linux","sha256":"`+shaOf(body)+`","url":"`+ReleasesPrefix+`v0.7.0/x","version":"0.7.0"}`), 0o600)
		}, ReasonBadSignature},
		{"firma di 63 byte", func(b *bench) {
			canon, _ := Canonical(withSHA(newManifest("0.7.0"), body))
			b.prepare(t, body, newManifest("0.7.0"), ed25519.Sign(b.priv, canon)[:63])
		}, ReasonBadSignature},
		{"binario mancante", func(b *bench) {
			b.prepare(t, body, newManifest("0.7.0"), nil)
			_ = os.Remove(filepath.Join(b.updateDir, FileBinary))
		}, ReasonDownloadFailed},
		{"solo apply.request, nient'altro", func(b *bench) {
			_ = os.WriteFile(filepath.Join(b.updateDir, FileApplyRequest), []byte("0.7.0\n"), 0o600)
		}, ReasonDownloadFailed},
		{"auto_update = no nel file dell'host (D6, assoluto): anche con tutto in regola", func(b *bench) {
			b.prepare(t, body, newManifest("0.7.0"), nil)
			_ = os.WriteFile(b.conf, []byte("auto_update=no\n"), 0o644)
		}, ReasonHostConfig},
		{"file dell'host illeggibile vale spento", func(b *bench) {
			b.prepare(t, body, newManifest("0.7.0"), nil)
			_ = os.WriteFile(b.conf, []byte("auto_updat=no\n"), 0o644)
		}, ReasonHostConfig},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBench(t)
			c.set(b)
			err := Apply(b.cfg())
			if err == nil {
				t.Fatal("installato")
			}
			if got := ReasonOf(err, ""); got != c.reason {
				t.Fatalf("motivo %q, atteso %q (%v)", got, c.reason, err)
			}
			b.binaryIs(t, "binario installato 0.6.1")
			if _, err := os.Lstat(b.bin + PrevSuffix); !os.IsNotExist(err) {
				t.Fatal(".prev creato su un rifiuto")
			}
			if o := b.rejected(t); o.Reason != c.reason {
				t.Fatalf("apply.rejected: %+v", o)
			}
			if _, ok := b.pending(t); ok {
				t.Fatal("pending.json scritto su un rifiuto")
			}
			b.noRequest(t)
			if len(b.calls) != 0 {
				t.Fatalf("systemctl chiamato su un rifiuto: %v", b.calls)
			}
		})
	}
}

func withSHA(m protocol.UpdateManifest, body []byte) protocol.UpdateManifest {
	m.SHA256 = shaOf(body)
	return m
}

// Un symlink in update/ (verso un file di root, o verso il binario stesso; assoluto o relativo che
// resta dentro la cassetta) non si segue: la copia è per contenuto e solo da file regolari, dal
// descrittore della cartella.
func TestApplyNonSegueSymlinkNellaCassetta(t *testing.T) {
	for _, name := range []string{FileBinary, FileManifest, FileSignature, "relativo"} {
		t.Run(name, func(t *testing.T) {
			b := newBench(t)
			body := []byte("binario nuovo 0.7.0")
			b.prepare(t, body, newManifest("0.7.0"), nil)
			target := filepath.Join(t.TempDir(), "file-di-root")
			_ = os.WriteFile(target, []byte("segreto"), 0o600)
			link := target
			if name == "relativo" {
				name = FileBinary
				_ = os.WriteFile(filepath.Join(b.updateDir, "dentro"), []byte("segreto"), 0o600)
				link = "dentro"
			}
			p := filepath.Join(b.updateDir, name)
			_ = os.Remove(p)
			if err := os.Symlink(link, p); err != nil {
				t.Fatal(err)
			}
			err := Apply(b.cfg())
			if err == nil || ReasonOf(err, "") != ReasonDownloadFailed {
				t.Fatalf("symlink al posto di %s: %v", name, err)
			}
			b.binaryIs(t, "binario installato 0.6.1")
			if o := b.rejected(t); o.Reason != ReasonDownloadFailed {
				t.Fatalf("apply.rejected: %+v", o)
			}
			if got, _ := os.ReadFile(target); string(got) != "segreto" {
				t.Fatal("bersaglio del symlink modificato")
			}
			for _, n := range []string{FileBinary, FileManifest, FileSignature} {
				if got, err := os.ReadFile(filepath.Join(b.updaterDir, n)); err == nil && string(got) == "segreto" {
					t.Fatalf("il contenuto del bersaglio è finito nella cartella dell'updater (%s)", n)
				}
			}
			b.noRequest(t)
		})
	}
	// Una cartella al posto del binario: non è un file regolare.
	b := newBench(t)
	b.prepare(t, []byte("x"), newManifest("0.7.0"), nil)
	_ = os.Remove(filepath.Join(b.updateDir, FileBinary))
	_ = os.Mkdir(filepath.Join(b.updateDir, FileBinary), 0o700)
	if err := Apply(b.cfg()); ReasonOf(err, "") != ReasonDownloadFailed {
		t.Fatalf("cartella al posto del binario: %v", err)
	}
}

// Una FIFO al posto del manifest non blocca l'updater (O_NONBLOCK + fstat): rifiuto in tempo breve.
func TestApplyNonSiBloccaSuUnaFIFO(t *testing.T) {
	for _, name := range []string{FileManifest, FileBinary} {
		b := newBench(t)
		b.prepare(t, []byte("binario nuovo 0.7.0"), newManifest("0.7.0"), nil)
		_ = os.Remove(filepath.Join(b.updateDir, name))
		if err := syscall.Mkfifo(filepath.Join(b.updateDir, name), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- Apply(b.cfg()) }()
		select {
		case err := <-done:
			if ReasonOf(err, "") != ReasonDownloadFailed {
				t.Fatalf("FIFO al posto di %s: %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("l'updater si è bloccato su una FIFO al posto di %s", name)
		}
		b.binaryIs(t, "binario installato 0.6.1")
		b.noRequest(t)
	}
}

// La copia FINALE (quella che finisce in .new) ha il suo SHA-256 confrontato col manifest: un file
// diverso da quello verificato non arriva mai al posto del binario (punto 1c della revisione).
func TestInstallVerifiedConfrontaLaCopiaFinale(t *testing.T) {
	src, _ := openStore(t)
	dstDir, _, err := OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dstDir.Close()
	good := []byte("binario verificato")
	_ = src.writeExclusive(FileBinary, good, 0o700)
	if err := installVerified(src.Dir, FileBinary, dstDir, "connector.new", shaOf(good)); err != nil {
		t.Fatalf("copia buona rifiutata: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dstDir.Path, "connector.new")); string(got) != string(good) {
		t.Fatalf("copia %q", got)
	}
	if st, _ := os.Stat(filepath.Join(dstDir.Path, "connector.new")); st.Mode().Perm() != 0o755 {
		t.Fatalf("modo %v", st.Mode().Perm())
	}
	// Il contenuto cambia dopo la prima verifica (lo sha atteso è quello del file buono): rifiuto e
	// nessun .new lasciato.
	_ = src.writeExclusive(FileBinary, []byte("binario sostituito fra le due letture"), 0o700)
	err = installVerified(src.Dir, FileBinary, dstDir, "connector.new", shaOf(good))
	if ReasonOf(err, "") != ReasonSHA256Mismatch {
		t.Fatalf("copia sostituita: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dstDir.Path, "connector.new")); !os.IsNotExist(err) {
		t.Fatal(".new lasciato dopo il rifiuto")
	}
}

func TestApplyRifiutaBinarioOltreIlTettoDiCopia(t *testing.T) {
	b := newBench(t)
	b.prepare(t, []byte("piccolo"), newManifest("0.7.0"), nil)
	// Il tetto della copia è MaxBinaryBytes: si simula con un manifest oltre il suo (64 KiB).
	_ = os.WriteFile(filepath.Join(b.updateDir, FileManifest), make([]byte, MaxManifestBytes+1), 0o600)
	if err := Apply(b.cfg()); ReasonOf(err, "") != ReasonTooLarge {
		t.Fatalf("manifest oltre il tetto: %v", err)
	}
	if o := b.rejected(t); o.Reason != ReasonTooLarge {
		t.Fatalf("apply.rejected: %+v", o)
	}
}

// La cartella dell'updater inutilizzabile (permessi larghi, symlink, antenato in mano ad altri,
// altro proprietario): l'updater si ferma, ma scrive comunque apply.rejected (download-failed), così
// il backend non resta «in corso» muto (punto 5 della revisione).
func TestApplyCartellaDellUpdater(t *testing.T) {
	cases := map[string]func(b *bench) ApplyConfig{
		"permessi larghi": func(b *bench) ApplyConfig {
			_ = os.Mkdir(b.updaterDir, 0o755)
			_ = os.Chmod(b.updaterDir, 0o755)
			return b.cfg()
		},
		"symlink al suo posto": func(b *bench) ApplyConfig {
			_ = os.Symlink(t.TempDir(), b.updaterDir)
			return b.cfg()
		},
		"altro proprietario atteso": func(b *bench) ApplyConfig {
			c := b.cfg()
			c.UID = os.Getuid() + 1
			return c
		},
		"antenato scrivibile da altri": func(b *bench) ApplyConfig {
			loose := filepath.Join(b.base, "loose")
			_ = os.Mkdir(loose, 0o777)
			_ = os.Chmod(loose, 0o777)
			c := b.cfg()
			c.UpdaterDir = filepath.Join(loose, "state-updater")
			return c
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			b := newBench(t)
			b.prepare(t, []byte("binario nuovo 0.7.0"), newManifest("0.7.0"), nil)
			err := Apply(mk(b))
			if err == nil || ReasonOf(err, "") != ReasonDownloadFailed {
				t.Fatalf("cartella dell'updater inutilizzabile accettata: %v", err)
			}
			b.binaryIs(t, "binario installato 0.6.1")
			if o := b.rejected(t); o.Reason != ReasonDownloadFailed || o.Version != "0.7.0" {
				t.Fatalf("apply.rejected: %+v", o)
			}
			b.noRequest(t)
			if len(b.calls) != 0 {
				t.Fatalf("systemctl: %v", b.calls)
			}
		})
	}
}

// Il servizio può girare come root (`install.sh --user root`): <state> è di root e l'updater lo accetta
// quando l'utente del servizio è root; qui si prova con l'uid corrente, owner di <state>. Con un utente
// del servizio diverso dall'owner la cassetta non si apre, niente si installa, e apply.request resta
// (toglierlo non sarebbe sicuro: ci pensa il limite di avvio dell'unità). Se invece la cassetta non si
// apre per un symlink al posto di update/, apply.request si toglie lo stesso.
func TestApplyUtenteDelServizio(t *testing.T) {
	b := newBench(t)
	b.prepare(t, []byte("binario nuovo 0.7.0"), newManifest("0.7.0"), nil)
	cfg := b.cfg()
	cfg.ServiceUID = os.Getuid()
	if err := Apply(cfg); err != nil {
		t.Fatalf("utente del servizio = owner di <state>: %v", err)
	}
	b.binaryIs(t, "binario nuovo 0.7.0")

	b = newBench(t)
	b.prepare(t, []byte("binario nuovo 0.7.0"), newManifest("0.7.0"), nil)
	cfg = b.cfg()
	cfg.ServiceUID = os.Getuid() + 1
	if err := Apply(cfg); err == nil || !strings.Contains(err.Error(), "utente del servizio") {
		t.Fatalf("utente del servizio diverso dall'owner: %v", err)
	}
	b.binaryIs(t, "binario installato 0.6.1")
	if _, err := os.Lstat(filepath.Join(b.updateDir, FileApplyRequest)); err != nil {
		t.Fatal("apply.request toccato in una cartella di stato non dell'utente del servizio")
	}
	if len(b.calls) != 0 {
		t.Fatalf("systemctl: %v", b.calls)
	}

	b = newBench(t)
	b.prepare(t, []byte("binario nuovo 0.7.0"), newManifest("0.7.0"), nil)
	_ = os.Rename(b.updateDir, filepath.Join(b.state, "altrove"))
	_ = os.Symlink("altrove", b.updateDir)
	if err := Apply(b.cfg()); err == nil {
		t.Fatal("symlink al posto di update/ accettato")
	}
	b.binaryIs(t, "binario installato 0.6.1")
	if _, err := os.Lstat(filepath.Join(b.state, "altrove", FileApplyRequest)); !os.IsNotExist(err) {
		t.Fatal("apply.request non tolto: l'unità path ripartirebbe in ciclo")
	}
}

// Se il timer di ritorno non si arma, il binario non si scambia: niente rete, niente installazione.
func TestApplySenzaTimerNonInstalla(t *testing.T) {
	b := newBench(t)
	b.prepare(t, []byte("binario nuovo 0.7.0"), newManifest("0.7.0"), nil)
	cfg := b.cfg()
	cfg.Systemctl = func(args ...string) error {
		b.calls = append(b.calls, strings.Join(args, " "))
		if args[1] == RollbackTimer {
			return os.ErrPermission
		}
		return nil
	}
	if err := Apply(cfg); err == nil {
		t.Fatal("installato senza il timer di ritorno")
	}
	b.binaryIs(t, "binario installato 0.6.1")
	if _, err := os.Lstat(b.bin + NewSuffix); !os.IsNotExist(err) {
		t.Fatal(".new rimasto")
	}
	if _, err := os.Lstat(b.bin + PrevSuffix); !os.IsNotExist(err) {
		t.Fatal(".prev rimasto")
	}
	if _, ok := b.pending(t); ok {
		t.Fatal("pending.json rimasto")
	}
	if o := b.rejected(t); o.Reason != ReasonDownloadFailed {
		t.Fatalf("apply.rejected: %+v", o)
	}
}

// Le unità girano con UMask=0077 (rilievo del banco del 17/11): i modi si impongono con fchmod.
func TestApplyConUmaskStretta(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	b := newBench(t)
	body := []byte("binario nuovo 0.7.0")
	b.prepare(t, body, newManifest("0.7.0"), nil)
	if err := Apply(b.cfg()); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]os.FileMode{b.bin: 0o755, b.bin + PrevSuffix: 0o755, filepath.Join(b.updateDir, FileApplied): 0o644} {
		if st, err := os.Stat(f); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: modo %v (%v), atteso %o", f, st.Mode().Perm(), err, want)
		}
	}
	if _, ok, err := b.mailbox(t).ReadApplied(); !ok || err != nil {
		t.Fatalf("applied.json illeggibile: %v %v", ok, err)
	}
	b.calls = nil
	if a, err := RollbackCheck(b.cfg()); err != nil || a != RollbackRestore {
		t.Fatalf("ritorno: %s %v", a, err)
	}
	if st, _ := os.Stat(b.bin); st.Mode().Perm() != 0o755 {
		t.Fatalf("binario ripristinato con modo %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(b.updateDir, FileRolledBack)); st.Mode().Perm() != 0o644 {
		t.Fatalf("rolled-back.json con modo %v", st.Mode().Perm())
	}
}

func TestDecideRollback(t *testing.T) {
	cases := []struct {
		pending, connected bool
		want               RollbackAction
	}{
		{false, false, RollbackNothing}, {false, true, RollbackNothing},
		{true, true, RollbackDisarm}, {true, false, RollbackRestore},
	}
	for _, c := range cases {
		if got := DecideRollback(c.pending, c.connected); got != c.want {
			t.Errorf("pending=%v connected=%v: %s, atteso %s", c.pending, c.connected, got, c.want)
		}
	}
}

// rollback-check (D3 punti 3 e 4): col marcatore disarma (e toglie anche i marcatori stantii), senza
// ripristina .prev e lascia rolled-back.json con motivo no-welcome; con un pending.json aperto un
// secondo aggiornamento è rifiutato (punto 6 della revisione).
func TestRollbackCheck(t *testing.T) {
	b := newBench(t)
	body := []byte("binario nuovo 0.7.0")
	b.prepare(t, body, newManifest("0.7.0"), nil)
	if err := Apply(b.cfg()); err != nil {
		t.Fatal(err)
	}
	// Secondo aggiornamento con pending.json aperto e senza marcatore: rifiutato.
	b.calls = nil
	b.prepare(t, []byte("altro 0.7.1"), newManifest("0.7.1"), nil)
	if err := Apply(b.cfg()); ReasonOf(err, "") != ReasonDownloadFailed || !strings.Contains(err.Error(), "in attesa del collegamento") {
		t.Fatalf("secondo aggiornamento con pending aperto: %v", err)
	}
	b.binaryIs(t, string(body))
	if o := b.rejected(t); o.Version != "0.7.1" {
		t.Fatalf("apply.rejected: %+v", o)
	}
	_ = b.mailbox(t).Remove(FileApplyRejected)
	// Il connettore nuovo si è collegato: disarmo, anche dei marcatori stantii.
	mb := b.mailbox(t)
	if err := mb.WriteConnected("0.7.0"); err != nil {
		t.Fatal(err)
	}
	_ = mb.WriteConnected("0.5.0")
	cfg := b.cfg()
	a, err := RollbackCheck(cfg)
	if err != nil || a != RollbackDisarm {
		t.Fatalf("disarmo: %s %v", a, err)
	}
	b.binaryIs(t, string(body))
	if _, err := os.Lstat(b.bin + PrevSuffix); !os.IsNotExist(err) {
		t.Fatal(".prev non tolto")
	}
	if _, ok := b.pending(t); ok || mb.ConnectedExists("0.7.0") || mb.ConnectedExists("0.5.0") {
		t.Fatal("pending.json o marcatori non tolti")
	}
	if got := strings.Join(b.calls, ";"); got != "stop "+RollbackTimer {
		t.Fatalf("systemctl: %s", got)
	}
	// Senza aggiornamento in attesa: niente.
	b.calls = nil
	if a, err := RollbackCheck(cfg); err != nil || a != RollbackNothing || len(b.calls) != 0 {
		t.Fatalf("niente da fare: %s %v %v", a, err, b.calls)
	}

	// Secondo aggiornamento, nessun welcome: ritorno.
	b.prepare(t, []byte("binario rotto 0.8.0"), newManifest("0.8.0"), nil)
	cfg.Running = "0.7.0"
	cfg.Platform.Version = "0.7.0"
	if err := Apply(cfg); err != nil {
		t.Fatal(err)
	}
	b.calls = nil
	a, err = RollbackCheck(cfg)
	if err != nil || a != RollbackRestore {
		t.Fatalf("ritorno: %s %v", a, err)
	}
	b.binaryIs(t, string(body))
	if _, err := os.Lstat(b.bin + PrevSuffix); !os.IsNotExist(err) {
		t.Fatal(".prev non consumato dal ripristino")
	}
	o, ok, err := mb.ReadOutcome(FileRolledBack)
	if !ok || err != nil || o.Version != "0.8.0" || o.Reason != ReasonNoWelcome {
		t.Fatalf("rolled-back.json: %+v %v %v", o, ok, err)
	}
	if _, ok := b.pending(t); ok {
		t.Fatal("pending.json rimasto dopo il ritorno")
	}
	if _, ok, _ := mb.ReadApplied(); ok {
		t.Fatal("applied.json rimasto dopo il ritorno")
	}
	if got := strings.Join(b.calls, ";"); got != "restart "+ServiceName {
		t.Fatalf("systemctl: %s", got)
	}
	// Il connettore vecchio, ripartito, trova il ritorno da riportare e nient'altro.
	s := mb.Collect("0.7.0")
	if len(s.Items) != 1 || s.Items[0].Result != (protocol.UpdateResult{Version: "0.8.0", Outcome: protocol.UpdateRolledBack, Reason: ReasonNoWelcome}) || s.Applied != nil {
		t.Fatalf("esito riportato: %+v", s)
	}
}
