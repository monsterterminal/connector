// Test della cassetta delle lettere fra connettore e updater (ADR 0438 D3 punti 2-4): marcatori,
// esiti, lettura senza seguire symlink, raccolta e consumo dopo welcome.
package update

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/monsterterminal/connector/internal/protocol"
)

func TestMarcatoriAndataERitorno(t *testing.T) {
	upd, updater := t.TempDir(), t.TempDir()
	if _, ok, err := ReadPending(updater); ok || err != nil {
		t.Fatalf("pending assente: %v %v", ok, err)
	}
	p := Pending{Version: "0.7.0", PreviousVersion: "0.6.1", At: "2026-11-17T10:00:00.000Z"}
	if err := WritePending(updater, p); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := ReadPending(updater); !ok || err != nil || got != p {
		t.Fatalf("pending: %+v %v %v", got, ok, err)
	}
	if st, _ := os.Stat(filepath.Join(updater, FilePending)); st.Mode().Perm() != 0o600 {
		t.Fatalf("pending.json modo %v", st.Mode())
	}
	if ConnectedExists(upd, "0.7.0") {
		t.Fatal("marcatore presente prima di scriverlo")
	}
	if err := WriteConnected(upd, "0.7.0"); err != nil || !ConnectedExists(upd, "0.7.0") || ConnectedExists(upd, "0.7.1") {
		t.Fatalf("marcatore: %v", err)
	}
	if err := WriteConnected(upd, "../x"); err == nil {
		t.Fatal("versione non semver accettata come nome del marcatore")
	}
	// Una cartella col nome del marcatore non è un marcatore.
	_ = os.Mkdir(ConnectedMarker(upd, "0.8.0"), 0o700)
	if ConnectedExists(upd, "0.8.0") {
		t.Fatal("una cartella vale come marcatore")
	}
	if err := WriteToMailbox(upd, FileApplyRejected, Outcome{Version: "0.7.0", Reason: ReasonBadSignature}); err != nil {
		t.Fatal(err)
	}
	if o, ok, err := ReadOutcome(upd, FileApplyRejected); !ok || err != nil || o.Reason != ReasonBadSignature || o.Version != "0.7.0" {
		t.Fatalf("esito: %+v %v %v", o, ok, err)
	}
	if st, _ := os.Stat(filepath.Join(upd, FileApplyRejected)); st.Mode().Perm() != 0o644 {
		t.Fatalf("apply.rejected modo %v: il connettore deve poterlo leggere", st.Mode())
	}
	if err := WriteToMailbox(upd, FileApplied, p); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := ReadApplied(upd); !ok || err != nil || got != p {
		t.Fatalf("applied: %+v %v %v", got, ok, err)
	}
}

// Root scrive nella cartella dell'utente: un symlink col nome del file non si segue mai, si toglie.
func TestScritturaNellaCassettaNonSegueSymlink(t *testing.T) {
	upd := t.TempDir()
	target := filepath.Join(t.TempDir(), "bersaglio")
	_ = os.WriteFile(target, []byte("intatto"), 0o600)
	if err := os.Symlink(target, filepath.Join(upd, FileRolledBack)); err != nil {
		t.Fatal(err)
	}
	if err := WriteToMailbox(upd, FileRolledBack, Outcome{Version: "0.7.0", Reason: ReasonNoWelcome}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "intatto" {
		t.Fatalf("il bersaglio del symlink è stato scritto: %q", got)
	}
	st, _ := os.Lstat(filepath.Join(upd, FileRolledBack))
	if st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("il symlink è rimasto al posto del file")
	}
	// E la lettura rifiuta un symlink al posto di un esito.
	_ = os.Remove(filepath.Join(upd, FileApplyRejected))
	_ = os.Symlink(target, filepath.Join(upd, FileApplyRejected))
	if _, _, err := ReadOutcome(upd, FileApplyRejected); err == nil {
		t.Fatal("symlink letto come esito")
	}
	// Un file troppo grande non si legge.
	big := make([]byte, MaxManifestBytes+1)
	_ = os.WriteFile(filepath.Join(upd, FileApplied), big, 0o600)
	if _, _, err := ReadApplied(upd); err == nil {
		t.Fatal("file oltre il tetto letto")
	}
}

// Dopo welcome il connettore raccoglie e CONSUMA gli esiti (D3 punti 3 e 4).
func TestCollectStartup(t *testing.T) {
	upd := t.TempDir()
	if rep := CollectStartup(upd, "0.7.0"); len(rep.Results) != 0 || rep.Connected != "" || len(rep.Warnings) != 0 {
		t.Fatalf("cassetta vuota: %+v", rep)
	}
	// Rifiuto dell'updater + residui del download: failed col motivo, tutto tolto.
	_ = os.WriteFile(filepath.Join(upd, FileBinary), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(upd, FileManifest), []byte("{}"), 0o600)
	_ = WriteToMailbox(upd, FileApplyRejected, Outcome{Version: "0.8.0", Reason: ReasonSHA256Mismatch})
	rep := CollectStartup(upd, "0.7.0")
	want := protocol.UpdateResult{Version: "0.8.0", Outcome: protocol.UpdateFailed, Reason: ReasonSHA256Mismatch}
	if len(rep.Results) != 1 || rep.Results[0] != want {
		t.Fatalf("rifiuto: %+v", rep)
	}
	if entries, _ := os.ReadDir(upd); len(entries) != 0 {
		t.Fatalf("file rimasti dopo il rifiuto: %v", entries)
	}
	// Ritorno indietro: rolled-back col motivo.
	_ = WriteToMailbox(upd, FileRolledBack, Outcome{Version: "0.8.0", Reason: ReasonNoWelcome})
	rep = CollectStartup(upd, "0.7.0")
	want = protocol.UpdateResult{Version: "0.8.0", Outcome: protocol.UpdateRolledBack, Reason: ReasonNoWelcome}
	if len(rep.Results) != 1 || rep.Results[0] != want {
		t.Fatalf("ritorno: %+v", rep)
	}
	// Installazione riuscita della propria versione: marcatore scritto e ok.
	_ = WriteToMailbox(upd, FileApplied, Pending{Version: "0.7.0", PreviousVersion: "0.6.1"})
	rep = CollectStartup(upd, "0.7.0")
	want = protocol.UpdateResult{Version: "0.7.0", Outcome: protocol.UpdateOK}
	if len(rep.Results) != 1 || rep.Results[0] != want || rep.Connected != "0.7.0" || !ConnectedExists(upd, "0.7.0") {
		t.Fatalf("installazione riuscita: %+v", rep)
	}
	if _, ok, _ := ReadApplied(upd); ok {
		t.Fatal("applied.json non consumato")
	}
	// Seconda raccolta: niente da riportare (gli esiti si consumano una volta sola).
	if rep := CollectStartup(upd, "0.7.0"); len(rep.Results) != 0 {
		t.Fatalf("esiti riportati due volte: %+v", rep)
	}
	// applied.json di un'altra versione (residuo dopo un ritorno): tolto, nessun ok.
	_ = WriteToMailbox(upd, FileApplied, Pending{Version: "0.9.0", PreviousVersion: "0.7.0"})
	rep = CollectStartup(upd, "0.7.0")
	if len(rep.Results) != 0 || len(rep.Warnings) != 1 || ConnectedExists(upd, "0.9.0") {
		t.Fatalf("residuo: %+v", rep)
	}
	if _, ok, _ := ReadApplied(upd); ok {
		t.Fatal("residuo non tolto")
	}
	// Un esito illeggibile: avviso, file tolto, nessun messaggio.
	_ = os.WriteFile(filepath.Join(upd, FileApplyRejected), []byte("non json"), 0o644)
	rep = CollectStartup(upd, "0.7.0")
	if len(rep.Results) != 0 || len(rep.Warnings) != 1 {
		t.Fatalf("esito illeggibile: %+v", rep)
	}
	if _, err := os.Lstat(filepath.Join(upd, FileApplyRejected)); !os.IsNotExist(err) {
		t.Fatal("esito illeggibile non tolto")
	}
}
