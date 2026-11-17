// Test della cassetta delle lettere e della cartella dell'updater (ADR 0438 D3 punti 2-4, A1;
// revisione di sicurezza 2026-11-17): tutto dai descrittori delle cartelle, mai dai percorsi; la
// cartella dell'updater fuori dall'albero dell'utente con gli antenati controllati; esiti consumati
// solo dopo la consegna; aperture che non seguono i link e non si bloccano.
package update

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

func openMB(t *testing.T) (*Mailbox, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state", UpdateSubdir)
	mb, err := OpenMailbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mb.Close() })
	return mb, dir
}

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "state-updater")
	st, err := OpenStore(dir, os.Getuid(), base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir
}

func TestMarcatoriAndataERitorno(t *testing.T) {
	mb, upd := openMB(t)
	st, updater := openStore(t)
	if info, _ := os.Stat(updater); info.Mode().Perm() != 0o700 {
		t.Fatalf("cartella dell'updater creata con modo %v", info.Mode().Perm())
	}
	if _, ok, err := st.ReadPending(); ok || err != nil {
		t.Fatalf("pending assente: %v %v", ok, err)
	}
	p := Pending{Version: "0.7.0", PreviousVersion: "0.6.1", At: "2026-11-17T10:00:00.000Z"}
	if err := st.WritePending(p); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := st.ReadPending(); !ok || err != nil || got != p {
		t.Fatalf("pending: %+v %v %v", got, ok, err)
	}
	if info, _ := os.Stat(filepath.Join(updater, FilePending)); info.Mode().Perm() != 0o600 {
		t.Fatalf("pending.json modo %v", info.Mode())
	}
	if mb.ConnectedExists("0.7.0") {
		t.Fatal("marcatore presente prima di scriverlo")
	}
	if err := mb.WriteConnected("0.7.0"); err != nil || !mb.ConnectedExists("0.7.0") || mb.ConnectedExists("0.7.1") {
		t.Fatalf("marcatore: %v", err)
	}
	if err := mb.WriteConnected("../x"); err == nil {
		t.Fatal("versione non semver accettata come nome del marcatore")
	}
	// Una cartella col nome del marcatore non è un marcatore; i marcatori stantii si tolgono tutti.
	_ = os.Mkdir(filepath.Join(upd, ConnectedMarker("0.8.0")), 0o700)
	if mb.ConnectedExists("0.8.0") {
		t.Fatal("una cartella vale come marcatore")
	}
	_ = mb.WriteConnected("0.5.0")
	mb.RemoveConnectedMarkers()
	if mb.ConnectedExists("0.7.0") || mb.ConnectedExists("0.5.0") {
		t.Fatal("marcatori non tolti")
	}
	if err := mb.WriteOutcome(FileApplyRejected, Outcome{Version: "0.7.0", Reason: ReasonBadSignature}); err != nil {
		t.Fatal(err)
	}
	if o, ok, err := mb.ReadOutcome(FileApplyRejected); !ok || err != nil || o.Reason != ReasonBadSignature || o.Version != "0.7.0" {
		t.Fatalf("esito: %+v %v %v", o, ok, err)
	}
	if info, _ := os.Stat(filepath.Join(upd, FileApplyRejected)); info.Mode().Perm() != 0o644 {
		t.Fatalf("apply.rejected modo %v: il connettore deve poterlo leggere", info.Mode())
	}
	if err := mb.WriteOutcome(FileApplied, p); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := mb.ReadApplied(); !ok || err != nil || got != p {
		t.Fatalf("applied: %+v %v %v", got, ok, err)
	}
	if err := mb.WriteRequest([]byte(`{"a":1}`), []byte("s"), "0.7.0"); err != nil {
		t.Fatal(err)
	}
	if v, err := mb.ReadRequestVersion(); err != nil || v != "0.7.0" {
		t.Fatalf("versione della richiesta: %q %v", v, err)
	}
	mb.RemoveDownload()
	if _, err := mb.ReadRequestVersion(); err == nil {
		t.Fatal("richiesta assente letta")
	}
	_ = os.WriteFile(filepath.Join(upd, FileApplyRequest), []byte("non-una-versione\n"), 0o600)
	if _, err := mb.ReadRequestVersion(); err == nil {
		t.Fatal("richiesta con versione non semver accettata")
	}
}

// Root scrive nella cartella dell'utente: un symlink col nome del file non si segue mai, si toglie;
// la lettura rifiuta symlink, file oltre il tetto e FIFO (senza bloccarsi).
func TestCassettaNonSegueLinkENonSiBlocca(t *testing.T) {
	mb, upd := openMB(t)
	target := filepath.Join(t.TempDir(), "bersaglio")
	_ = os.WriteFile(target, []byte("intatto"), 0o600)
	if err := os.Symlink(target, filepath.Join(upd, FileRolledBack)); err != nil {
		t.Fatal(err)
	}
	if err := mb.WriteOutcome(FileRolledBack, Outcome{Version: "0.7.0", Reason: ReasonNoWelcome}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "intatto" {
		t.Fatalf("il bersaglio del symlink è stato scritto: %q", got)
	}
	if st, _ := os.Lstat(filepath.Join(upd, FileRolledBack)); st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("il symlink è rimasto al posto del file")
	}
	_ = os.Symlink(target, filepath.Join(upd, FileApplyRejected))
	if _, _, err := mb.ReadOutcome(FileApplyRejected); err == nil {
		t.Fatal("symlink letto come esito")
	}
	// Anche un symlink RELATIVO che resta dentro la cassetta (os.Root lo seguirebbe da sé).
	_ = os.Remove(filepath.Join(upd, FileApplyRejected))
	_ = os.WriteFile(filepath.Join(upd, "dentro.json"), []byte(`{"version":"0.7.0","reason":"bad-url"}`), 0o600)
	_ = os.Symlink("dentro.json", filepath.Join(upd, FileApplyRejected))
	if _, _, err := mb.ReadOutcome(FileApplyRejected); err == nil {
		t.Fatal("symlink relativo letto come esito")
	}
	_ = os.Remove(filepath.Join(upd, FileApplyRejected))
	_ = os.WriteFile(filepath.Join(upd, FileApplied), make([]byte, MaxManifestBytes+1), 0o600)
	if _, _, err := mb.ReadApplied(); err == nil {
		t.Fatal("file oltre il tetto letto")
	}
	// Una FIFO al posto di un esito: l'apertura non deve bloccarsi (O_NONBLOCK) e fstat la scopre.
	_ = os.Remove(filepath.Join(upd, FileApplyRejected))
	if err := syscall.Mkfifo(filepath.Join(upd, FileApplyRejected), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := mb.ReadOutcome(FileApplyRejected); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO letta come esito")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("l'apertura di una FIFO si è bloccata")
	}
	// E la scrittura di root sopra una FIFO la toglie e scrive un file regolare.
	if err := mb.WriteOutcome(FileApplyRejected, Outcome{Version: "0.7.0", Reason: ReasonBadURL}); err != nil {
		t.Fatal(err)
	}
	if !mb.IsRegular(FileApplyRejected) {
		t.Fatal("al posto della FIFO non c'è un file regolare")
	}
}

// La cartella dell'updater (A1): fuori dall'albero dell'utente, antenati controllati, fstat sul
// descrittore. Un antenato che è un symlink o scrivibile da altri la rende inutilizzabile.
func TestCartellaDellUpdater(t *testing.T) {
	base := t.TempDir()
	// Antenato symlink.
	real := filepath.Join(base, "real")
	_ = os.Mkdir(real, 0o755)
	_ = os.Symlink(real, filepath.Join(base, "link"))
	if _, err := OpenStore(filepath.Join(base, "link", "x-updater"), os.Getuid(), base); err == nil {
		t.Fatal("antenato symlink accettato")
	}
	// Antenato scrivibile da altri.
	loose := filepath.Join(base, "loose")
	_ = os.Mkdir(loose, 0o777)
	_ = os.Chmod(loose, 0o777)
	if _, err := OpenStore(filepath.Join(loose, "x-updater"), os.Getuid(), base); err == nil {
		t.Fatal("antenato scrivibile da altri accettato")
	}
	// Antenato di un altro proprietario atteso.
	if _, err := OpenStore(filepath.Join(real, "x-updater"), os.Getuid()+1, base); err == nil {
		t.Fatal("antenato di un altro uid accettato")
	}
	// Buona: creata 0700, riaperta con permessi larghi → rifiutata; symlink al suo posto → rifiutata.
	good := filepath.Join(real, "x-updater")
	st, err := OpenStore(good, os.Getuid(), base)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	_ = os.Chmod(good, 0o755)
	if _, err := OpenStore(good, os.Getuid(), base); err == nil {
		t.Fatal("cartella con permessi larghi accettata")
	}
	_ = os.Chmod(good, 0o700)
	other := filepath.Join(base, "altrove")
	_ = os.Mkdir(other, 0o700)
	_ = os.RemoveAll(good)
	_ = os.Symlink(other, good)
	if _, err := OpenStore(good, os.Getuid(), base); err == nil {
		t.Fatal("symlink al posto della cartella accettato")
	}
	// Un file al posto della cartella.
	_ = os.Remove(good)
	_ = os.WriteFile(good, []byte("x"), 0o600)
	if _, err := OpenStore(good, os.Getuid(), base); err == nil {
		t.Fatal("file al posto della cartella accettato")
	}
	if UpdaterDirFor("/var/lib/monsterterminal") != "/var/lib/monsterterminal-updater" {
		t.Fatalf("cartella dell'updater %s", UpdaterDirFor("/var/lib/monsterterminal"))
	}
}

// La cassetta vista da root: dal Root di <state>, la sottocartella update/ dello stesso utente, mai
// un symlink. <state> deve essere dell'utente del servizio: quello dell'unità (root compreso, quando
// il servizio gira come root: qui lo si prova con l'uid corrente, che è l'owner della cartella),
// oppure, senza indicazione, l'owner stesso di <state>.
func TestCassettaVistaDaRoot(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	_ = os.MkdirAll(filepath.Join(state, UpdateSubdir), 0o700)
	for _, uid := range []int{-1, os.Getuid()} {
		mb, err := OpenMailboxUnder(state, uid)
		if err != nil {
			t.Fatalf("utente del servizio %d: %v", uid, err)
		}
		mb.Close()
	}
	if _, err := OpenMailboxUnder(state, os.Getuid()+1); err == nil {
		t.Fatal("cartella di stato di un altro utente accettata")
	}
	_ = os.RemoveAll(filepath.Join(state, UpdateSubdir))
	_ = os.Symlink(t.TempDir(), filepath.Join(state, UpdateSubdir))
	if _, err := OpenMailboxUnder(state, -1); err == nil {
		t.Fatal("symlink al posto di update/ accettato")
	}
	_ = os.Remove(filepath.Join(state, UpdateSubdir))
	if _, err := OpenMailboxUnder(state, -1); err == nil {
		t.Fatal("update/ assente accettata")
	}
	if _, err := OpenMailboxUnder(filepath.Join(state, "non-esiste"), -1); err == nil {
		t.Fatal("stato assente accettato")
	}
	// apply.request si toglie anche senza cassetta, ma solo se <state> è dell'utente del servizio.
	// Un symlink relativo che resta dentro <state> si attraversa; uno assoluto no: os.Root lo tratta
	// come un'uscita dalla radice e rifiuta (allora resta il limite di avvio dell'unità).
	other := filepath.Join(state, "altrove")
	_ = os.Mkdir(other, 0o700)
	_ = os.WriteFile(filepath.Join(other, FileApplyRequest), []byte("0.7.0\n"), 0o600)
	_ = os.Symlink(other, filepath.Join(state, UpdateSubdir))
	if err := RemoveRequestBestEffort(state, os.Getuid()); err == nil {
		t.Fatal("symlink assoluto attraversato dal Root")
	}
	_ = os.Remove(filepath.Join(state, UpdateSubdir))
	_ = os.Symlink("altrove", filepath.Join(state, UpdateSubdir))
	if err := RemoveRequestBestEffort(state, os.Getuid()+1); err == nil {
		t.Fatal("apply.request toccato in una cartella di stato di un altro utente")
	}
	if _, err := os.Lstat(filepath.Join(other, FileApplyRequest)); err != nil {
		t.Fatal("apply.request tolto nonostante il rifiuto")
	}
	if err := RemoveRequestBestEffort(state, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(other, FileApplyRequest)); !os.IsNotExist(err) {
		t.Fatal("apply.request non tolto")
	}
	if err := RemoveRequestBestEffort(state, -1); err != nil {
		t.Fatalf("senza richiesta da togliere: %v", err)
	}
}

// Dopo welcome il connettore raccoglie gli esiti e li CONSUMA solo con Ack, dopo la consegna
// (D3 punti 3 e 4; punto 3 della revisione).
func TestCollect(t *testing.T) {
	mb, upd := openMB(t)
	if s := mb.Collect("0.7.0"); len(s.Items) != 0 || s.Applied != nil || len(s.Warnings) != 0 {
		t.Fatalf("cassetta vuota: %+v", s)
	}
	// Rifiuto dell'updater + residui del download: failed col motivo; senza Ack resta tutto.
	_ = os.WriteFile(filepath.Join(upd, FileBinary), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(upd, FileManifest), []byte("{}"), 0o600)
	_ = mb.WriteOutcome(FileApplyRejected, Outcome{Version: "0.8.0", Reason: ReasonSHA256Mismatch})
	s := mb.Collect("0.7.0")
	want := protocol.UpdateResult{Version: "0.8.0", Outcome: protocol.UpdateFailed, Reason: ReasonSHA256Mismatch}
	if len(s.Items) != 1 || s.Items[0].Result != want {
		t.Fatalf("rifiuto: %+v", s)
	}
	if s2 := mb.Collect("0.7.0"); len(s2.Items) != 1 {
		t.Fatalf("senza Ack l'esito deve ripresentarsi: %+v", s2)
	}
	if err := s.Items[0].Ack(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(upd); len(entries) != 0 {
		t.Fatalf("file rimasti dopo l'Ack: %v", entries)
	}
	// Un rifiuto per interruttore è skipped, non failed.
	_ = mb.WriteOutcome(FileApplyRejected, Outcome{Version: "0.8.0", Reason: ReasonHostConfig})
	s = mb.Collect("0.7.0")
	if len(s.Items) != 1 || s.Items[0].Result != (protocol.UpdateResult{Version: "0.8.0", Outcome: protocol.UpdateSkipped, Reason: ReasonHostConfig}) {
		t.Fatalf("host-config: %+v", s)
	}
	_ = s.Items[0].Ack()
	// Ritorno indietro: rolled-back col motivo.
	_ = mb.WriteOutcome(FileRolledBack, Outcome{Version: "0.8.0", Reason: ReasonNoWelcome})
	s = mb.Collect("0.7.0")
	if len(s.Items) != 1 || s.Items[0].Result != (protocol.UpdateResult{Version: "0.8.0", Outcome: protocol.UpdateRolledBack, Reason: ReasonNoWelcome}) {
		t.Fatalf("ritorno: %+v", s)
	}
	_ = s.Items[0].Ack()
	// Installazione riuscita della propria versione: Applied, nessun marcatore ancora, file intatto.
	_ = mb.WriteOutcome(FileApplied, Pending{Version: "0.7.0", PreviousVersion: "0.6.1"})
	s = mb.Collect("0.7.0")
	if s.Applied == nil || s.Applied.Version != "0.7.0" || len(s.Items) != 0 || mb.ConnectedExists("0.7.0") {
		t.Fatalf("installazione riuscita: %+v", s)
	}
	if _, ok, _ := mb.ReadApplied(); !ok {
		t.Fatal("applied.json consumato prima della finestra di collegamento")
	}
	_ = mb.Remove(FileApplied)
	// applied.json di un'altra versione (residuo dopo un ritorno): tolto, nessun ok.
	_ = mb.WriteOutcome(FileApplied, Pending{Version: "0.9.0", PreviousVersion: "0.7.0"})
	s = mb.Collect("0.7.0")
	if s.Applied != nil || len(s.Items) != 0 || len(s.Warnings) != 1 {
		t.Fatalf("residuo: %+v", s)
	}
	if _, ok, _ := mb.ReadApplied(); ok {
		t.Fatal("residuo non tolto")
	}
	// Un esito illeggibile: avviso, file tolto, nessun messaggio.
	_ = os.WriteFile(filepath.Join(upd, FileApplyRejected), []byte("non json"), 0o644)
	s = mb.Collect("0.7.0")
	if len(s.Items) != 0 || len(s.Warnings) != 1 || mb.IsRegular(FileApplyRejected) {
		t.Fatalf("esito illeggibile: %+v", s)
	}
}
