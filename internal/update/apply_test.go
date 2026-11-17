// Test dell'updater root (ADR 0438 D3 punti 2-4; decisione BOSS 2026-11-17: l'updater NON si fida
// di ciò che il connettore ha preparato). Niente root e niente systemd qui: il binario installato è
// un file in una cartella temporanea, systemctl è una funzione che registra le chiamate.
package update

import (
	"crypto/ed25519"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterterminal/connector/internal/protocol"
)

type bench struct {
	state, updateDir, updaterDir, bin string
	pub                               ed25519.PublicKey
	priv                              ed25519.PrivateKey
	calls                             []string
}

func newBench(t *testing.T) *bench {
	t.Helper()
	root := t.TempDir()
	b := &bench{state: filepath.Join(root, "state"), bin: filepath.Join(root, "bin", "monsterterminal-connector")}
	b.updateDir = filepath.Join(b.state, UpdateSubdir)
	b.updaterDir = filepath.Join(b.state, UpdaterSubdir)
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
		StateDir:  b.state,
		Binary:    b.bin,
		Running:   "0.6.1",
		Platform:  Platform{OS: "linux", Arch: "amd64", Protocol: 1, Version: "0.6.1"},
		Key:       b.pub,
		Systemctl: func(args ...string) error { b.calls = append(b.calls, strings.Join(args, " ")); return nil },
		UID:       os.Getuid(),
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
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
	if err := WriteRequest(b.updateDir, canon, sig, m.Version); err != nil {
		t.Fatal(err)
	}
}

func (b *bench) rejected(t *testing.T) Outcome {
	t.Helper()
	o, ok, err := ReadOutcome(b.updateDir, FileApplyRejected)
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
	p, ok, err := ReadPending(b.updaterDir)
	if !ok || err != nil || p.Version != "0.7.0" || p.PreviousVersion != "0.6.1" || p.At == "" {
		t.Fatalf("pending.json: %+v %v %v", p, ok, err)
	}
	if a, ok, _ := ReadApplied(b.updateDir); !ok || a != p {
		t.Fatalf("applied.json: %+v %v (pending %+v)", a, ok, p)
	}
	if st, _ := os.Stat(b.updaterDir); st.Mode().Perm() != 0o700 {
		t.Fatalf("updater/ modo %v", st.Mode())
	}
	b.noRequest(t)
	for _, n := range []string{FileBinary, FileManifest, FileSignature} {
		if _, err := os.Lstat(filepath.Join(b.updaterDir, n)); !os.IsNotExist(err) {
			t.Errorf("copia %s rimasta in updater/", n)
		}
	}
	if got := strings.Join(b.calls, ";"); got != "restart "+RollbackTimer+";restart "+ServiceName {
		t.Fatalf("systemctl: %s", got)
	}
	if _, ok, _ := ReadOutcome(b.updateDir, FileApplyRejected); ok {
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
			if _, ok, _ := ReadPending(b.updaterDir); ok {
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

// Un symlink in update/ (verso un file di root, o verso il binario stesso) non si segue: la copia è
// per contenuto e solo da file regolari (O_NOFOLLOW + Lstat + fstat).
func TestApplyNonSegueSymlinkNellaCassetta(t *testing.T) {
	for _, name := range []string{FileBinary, FileManifest, FileSignature} {
		t.Run(name, func(t *testing.T) {
			b := newBench(t)
			body := []byte("binario nuovo 0.7.0")
			b.prepare(t, body, newManifest("0.7.0"), nil)
			target := filepath.Join(t.TempDir(), "file-di-root")
			_ = os.WriteFile(target, []byte("segreto"), 0o600)
			p := filepath.Join(b.updateDir, name)
			_ = os.Remove(p)
			if err := os.Symlink(target, p); err != nil {
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
			// Il bersaglio non è stato copiato né toccato.
			if got, _ := os.ReadFile(target); string(got) != "segreto" {
				t.Fatal("bersaglio del symlink modificato")
			}
			for _, n := range []string{FileBinary, FileManifest, FileSignature} {
				if got, err := os.ReadFile(filepath.Join(b.updaterDir, n)); err == nil && string(got) == "segreto" {
					t.Fatalf("il contenuto del bersaglio è finito in updater/%s", n)
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

func TestApplyRifiutaBinarioOltreIlTettoDiCopia(t *testing.T) {
	b := newBench(t)
	b.prepare(t, []byte("piccolo"), newManifest("0.7.0"), nil)
	// Il tetto della copia è MaxBinaryBytes: si simula con un manifest oltre il suo (64 KiB).
	big := make([]byte, MaxManifestBytes+1)
	_ = os.WriteFile(filepath.Join(b.updateDir, FileManifest), big, 0o600)
	if err := Apply(b.cfg()); ReasonOf(err, "") != ReasonTooLarge {
		t.Fatalf("manifest oltre il tetto: %v", err)
	}
	if o := b.rejected(t); o.Reason != ReasonTooLarge {
		t.Fatalf("apply.rejected: %+v", o)
	}
}

// La cartella root-only: creata 0700 dall'updater; se esiste con altri permessi ci si ferma.
func TestApplyCartellaDellUpdater(t *testing.T) {
	b := newBench(t)
	b.prepare(t, []byte("binario nuovo 0.7.0"), newManifest("0.7.0"), nil)
	_ = os.MkdirAll(b.updaterDir, 0o755)
	_ = os.Chmod(b.updaterDir, 0o755)
	err := Apply(b.cfg())
	if err == nil || !strings.Contains(err.Error(), "permessi") {
		t.Fatalf("cartella con permessi larghi accettata: %v", err)
	}
	b.binaryIs(t, "binario installato 0.6.1")
	if _, ok, _ := ReadOutcome(b.updateDir, FileApplyRejected); ok {
		t.Fatal("apply.rejected scritto per un problema dell'updater stesso")
	}
	b.noRequest(t)
	// Un symlink al posto della cartella.
	b = newBench(t)
	b.prepare(t, []byte("binario nuovo 0.7.0"), newManifest("0.7.0"), nil)
	_ = os.Symlink(t.TempDir(), b.updaterDir)
	if err := Apply(b.cfg()); err == nil || !strings.Contains(err.Error(), "non è una cartella") {
		t.Fatalf("symlink al posto di updater/: %v", err)
	}
	// Un altro proprietario atteso.
	b = newBench(t)
	b.prepare(t, []byte("binario nuovo 0.7.0"), newManifest("0.7.0"), nil)
	cfg := b.cfg()
	cfg.UID = os.Getuid() + 1
	if err := Apply(cfg); err == nil || !strings.Contains(err.Error(), "appartiene") {
		t.Fatalf("altro proprietario: %v", err)
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
	if _, ok, _ := ReadPending(b.updaterDir); ok {
		t.Fatal("pending.json rimasto")
	}
	if o := b.rejected(t); o.Reason != ReasonDownloadFailed {
		t.Fatalf("apply.rejected: %+v", o)
	}
}

func TestDecideRollback(t *testing.T) {
	cases := []struct {
		pending, connected bool
		want                   RollbackAction
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

// rollback-check (D3 punti 3 e 4): col marcatore disarma, senza ripristina .prev e lascia
// rolled-back.json con motivo no-welcome.
func TestRollbackCheck(t *testing.T) {
	b := newBench(t)
	body := []byte("binario nuovo 0.7.0")
	b.prepare(t, body, newManifest("0.7.0"), nil)
	if err := Apply(b.cfg()); err != nil {
		t.Fatal(err)
	}
	b.calls = nil
	// Il connettore nuovo si è collegato: disarmo.
	if err := WriteConnected(b.updateDir, "0.7.0"); err != nil {
		t.Fatal(err)
	}
	cfg := b.cfg()
	cfg.Running = "0.6.1" // gira il .prev
	a, err := RollbackCheck(cfg)
	if err != nil || a != RollbackDisarm {
		t.Fatalf("disarmo: %s %v", a, err)
	}
	b.binaryIs(t, string(body))
	if _, err := os.Lstat(b.bin + PrevSuffix); !os.IsNotExist(err) {
		t.Fatal(".prev non tolto")
	}
	if _, ok, _ := ReadPending(b.updaterDir); ok || ConnectedExists(b.updateDir, "0.7.0") {
		t.Fatal("pending.json o marcatore non tolti")
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
	o, ok, err := ReadOutcome(b.updateDir, FileRolledBack)
	if !ok || err != nil || o.Version != "0.8.0" || o.Reason != ReasonNoWelcome {
		t.Fatalf("rolled-back.json: %+v %v %v", o, ok, err)
	}
	if _, ok, _ := ReadPending(b.updaterDir); ok {
		t.Fatal("pending.json rimasto dopo il ritorno")
	}
	if _, ok, _ := ReadApplied(b.updateDir); ok {
		t.Fatal("applied.json rimasto dopo il ritorno")
	}
	if got := strings.Join(b.calls, ";"); got != "restart "+ServiceName {
		t.Fatalf("systemctl: %s", got)
	}
	// Il connettore vecchio, ripartito, riporta il ritorno e nient'altro.
	rep := CollectStartup(b.updateDir, "0.7.0")
	if len(rep.Results) != 1 || rep.Results[0] != (protocol.UpdateResult{Version: "0.8.0", Outcome: protocol.UpdateRolledBack, Reason: ReasonNoWelcome}) {
		t.Fatalf("esito riportato: %+v", rep)
	}
}
