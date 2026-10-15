package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
)

// backend riproduce classifyEvent di connector-journal.service.ts (ADR 0409, spec §1.12 emendata
// 2026-10-14): è il giudice di coerenza di questi test.
type backend struct {
	last  int64
	saved []protocol.Event
}

func (b *backend) ingest(t *testing.T, ev protocol.Event) string {
	t.Helper()
	if ev.Seq <= b.last {
		return "duplicate"
	}
	if ev.Kind == protocol.KindJournalTruncated {
		var d protocol.JournalTruncated
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("data del troncamento illeggibile: %v", err)
		}
		o, _ := time.Parse(time.RFC3339Nano, d.OldestDroppedAt)
		n, _ := time.Parse(time.RFC3339Nano, d.NewestDroppedAt)
		if d.DroppedCount != ev.Seq-b.last || d.FirstKeptSeq != ev.Seq+1 || o.After(n) {
			return "TRUNCATION_INCOHERENT"
		}
	} else if ev.Seq != b.last+1 {
		return "SEQ_GAP"
	}
	b.last = ev.Seq
	b.saved = append(b.saved, ev)
	return "saved"
}

// drain invia tutto il pendente come farebbe il trasporto (Rebase sul precedente nel flusso) e
// conferma; ogni evento deve essere accettato.
func drain(t *testing.T, j *Journal, b *backend) {
	t.Helper()
	prev := b.last
	for _, ev := range j.Pending(prev, 1<<30) {
		ev = Rebase(ev, prev)
		if got := b.ingest(t, ev); got != "saved" && got != "duplicate" {
			t.Fatalf("evento %d (%s) rifiutato: %s", ev.Seq, ev.Kind, got)
		}
		prev = ev.Seq
	}
	if err := j.Ack(b.last); err != nil {
		t.Fatal(err)
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func open(t *testing.T, dir string, opts Options) *Journal {
	t.Helper()
	j, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func appendN(t *testing.T, j *Journal, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := j.Append("test.event", map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeqCrescentiConfermaERiavvio(t *testing.T) {
	dir := t.TempDir()
	j := open(t, dir, Options{})
	appendN(t, j, 5)
	evs := j.Pending(0, 10)
	if len(evs) != 5 || evs[0].Seq != 1 || evs[4].Seq != 5 {
		t.Fatalf("pendenti attesi 1..5, trovati %+v", evs)
	}
	if err := j.Ack(3); err != nil {
		t.Fatal(err)
	}
	if j.LastAcked() != 3 || j.Len() != 2 {
		t.Fatalf("dopo ack 3: segnalibro %d, pendenti %d", j.LastAcked(), j.Len())
	}
	// Ack più basso del segnalibro: non lo abbassa (per quello c'è Resync).
	if err := j.Ack(1); err != nil || j.LastAcked() != 3 {
		t.Fatalf("ack all'indietro ha mosso il segnalibro: %d %v", j.LastAcked(), err)
	}
	// Ack oltre lo scritto: limitato all'ultimo seq.
	if err := j.Ack(99); err != nil || j.LastAcked() != 5 {
		t.Fatalf("ack oltre lo scritto: segnalibro %d %v", j.LastAcked(), err)
	}
	appendN(t, j, 2)

	// Riavvio: il seq riprende da dove era, i confermati non tornano.
	j2 := open(t, dir, Options{})
	if j2.LastAcked() != 5 || j2.Len() != 2 {
		t.Fatalf("dopo il riavvio: segnalibro %d, pendenti %d", j2.LastAcked(), j2.Len())
	}
	ev, err := j2.Append("test.event", nil)
	if err != nil || ev.Seq != 8 {
		t.Fatalf("seq dopo il riavvio: %d %v", ev.Seq, err)
	}
}

func TestPermessiDelGiornale(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stato")
	j := open(t, dir, Options{})
	appendN(t, j, 1)
	if err := j.Ack(1); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, j.eventsPath(): 0o600, j.statePath(): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: permessi %o, attesi %o", path, fi.Mode().Perm(), want)
		}
	}
}

func TestRigaTroncataDaUnoSpegnimentoBrusco(t *testing.T) {
	dir := t.TempDir()
	j := open(t, dir, Options{})
	appendN(t, j, 3)
	f, err := os.OpenFile(j.eventsPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"seq":4,"kind":"test.ev`)
	f.Close()
	j2 := open(t, dir, Options{})
	if j2.Len() != 3 {
		t.Fatalf("pendenti dopo riga monca: %d, attesi 3", j2.Len())
	}
	ev, _ := j2.Append("test.event", nil)
	if ev.Seq != 4 {
		t.Fatalf("seq dopo riga monca: %d, atteso 4", ev.Seq)
	}
}

func TestDataOltre4KiB(t *testing.T) {
	j := open(t, t.TempDir(), Options{})
	big := make([]byte, protocol.MaxEventDataBytes)
	if _, err := j.Append("test.event", string(big)); err != ErrDataTooLarge {
		t.Fatalf("atteso ErrDataTooLarge, trovato %v", err)
	}
}

// Oltre il tetto di eventi: i più vecchi diventano UN journal.truncated coerente, accettato dal
// backend; il seq del troncamento è quello dell'ultimo scartato.
func TestTettoDiEventiTroncamentoCoerente(t *testing.T) {
	j := open(t, t.TempDir(), Options{MaxEvents: 10})
	appendN(t, j, 11)
	evs := j.Pending(0, 100)
	if evs[0].Kind != protocol.KindJournalTruncated {
		t.Fatalf("primo pendente %s, atteso journal.truncated", evs[0].Kind)
	}
	var d protocol.JournalTruncated
	json.Unmarshal(evs[0].Data, &d)
	// 11 eventi, tetto 10, blocco del 10% = 1: scartati 1..2, T prende seq 2.
	if evs[0].Seq != 2 || d.DroppedCount != 2 || d.FirstKeptSeq != 3 || evs[1].Seq != 3 {
		t.Fatalf("troncamento %+v %+v, successivo %d", evs[0], d, evs[1].Seq)
	}
	if d.OldestDroppedAt > d.NewestDroppedAt {
		t.Fatalf("istanti invertiti: %+v", d)
	}
	b := &backend{}
	drain(t, j, b)
	if b.last != 11 {
		t.Fatalf("segnalibro del backend %d, atteso 11", b.last)
	}
}

// Due ondate di scarti prima dell'invio: il primo troncamento resta, il secondo dichiara solo i
// suoi; il backend accetta la sequenza.
func TestDueTroncamentiInFila(t *testing.T) {
	j := open(t, t.TempDir(), Options{MaxEvents: 10})
	appendN(t, j, 30)
	b := &backend{}
	drain(t, j, b)
	if b.last != 30 {
		t.Fatalf("segnalibro %d, atteso 30", b.last)
	}
	var tronc, total int64
	for _, ev := range b.saved {
		if ev.Kind == protocol.KindJournalTruncated {
			var d protocol.JournalTruncated
			json.Unmarshal(ev.Data, &d)
			tronc += d.DroppedCount
		} else {
			total++
		}
	}
	// Ogni seq 1..30 è o consegnato o dichiarato perso, esattamente una volta.
	if tronc+total != 30 {
		t.Fatalf("consegnati %d + dichiarati persi %d != 30", total, tronc)
	}
}

// Eventi in volo già accettati dal backend, poi scartati dal giornale: il troncamento ricalcolato
// all'invio dichiara solo quelli che il backend non ha (il caso che senza Rebase darebbe
// TRUNCATION_INCOHERENT a ogni ricollegamento).
func TestScartoDiEventiGiaInVolo(t *testing.T) {
	j := open(t, t.TempDir(), Options{MaxEvents: 10})
	appendN(t, j, 10)
	b := &backend{}
	prev := int64(0)
	for _, ev := range j.Pending(0, 4) { // 1..4 accettati, ack non ancora arrivato
		b.ingest(t, ev)
		prev = ev.Seq
	}
	appendN(t, j, 1) // scarta 1..2 → T(2): il backend ha già 4
	next := j.Pending(prev, 100)
	if next[0].Seq != 5 {
		t.Fatalf("dopo 4 inviati il prossimo è %d, atteso 5", next[0].Seq)
	}
	// Caso peggiore: lo scarto supera i già inviati.
	j2 := open(t, t.TempDir(), Options{MaxEvents: 10})
	appendN(t, j2, 10)
	b2 := &backend{}
	for _, ev := range j2.Pending(0, 1) {
		b2.ingest(t, ev)
	}
	appendN(t, j2, 1) // scarta 1..2, il backend ha 1
	ev := Rebase(j2.Pending(1, 1)[0], 1)
	if got := b2.ingest(t, ev); got != "saved" {
		t.Fatalf("troncamento ricalcolato rifiutato: %s", got)
	}
	var d protocol.JournalTruncated
	json.Unmarshal(ev.Data, &d)
	if d.DroppedCount != 1 {
		t.Fatalf("droppedCount %d, atteso 1 (solo il seq 2)", d.DroppedCount)
	}
	// Senza Rebase lo stesso troncamento è incoerente: la controprova del perché esiste.
	b3 := &backend{last: 1}
	if got := b3.ingest(t, j2.Pending(1, 1)[0]); got != "TRUNCATION_INCOHERENT" {
		t.Fatalf("senza Rebase atteso TRUNCATION_INCOHERENT, trovato %s", got)
	}
}

func TestEtaMassima(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	j := open(t, dir, Options{MaxAge: 7 * 24 * time.Hour, Now: c.now})
	appendN(t, j, 3)
	c.t = c.t.Add(8 * 24 * time.Hour)
	appendN(t, j, 1)
	evs := j.Pending(0, 10)
	if len(evs) != 2 || evs[0].Kind != protocol.KindJournalTruncated || evs[0].Seq != 3 || evs[1].Seq != 4 {
		t.Fatalf("dopo 8 giorni attesi T(3) e 4, trovati %+v", evs)
	}
	// Anche al riavvio si applica l'età.
	appendN(t, j, 1)
	c.t = c.t.Add(8 * 24 * time.Hour)
	j2 := open(t, dir, Options{MaxAge: 7 * 24 * time.Hour, Now: c.now})
	evs = j2.Pending(0, 10)
	if evs[len(evs)-1].Kind != protocol.KindJournalTruncated || evs[len(evs)-1].Seq != 5 {
		t.Fatalf("al riavvio dopo 8 giorni atteso un troncamento fino a 5, trovati %+v", evs)
	}
	drain(t, j2, &backend{})
}

// ACK_AHEAD: il backend ha 3, il connettore credeva 7 confermati e li aveva già tolti. Resync
// dichiara persi 4..7 e il backend accetta; il riavvio non resuscita le righe vecchie.
func TestResyncAlSegnalibroDelBackend(t *testing.T) {
	dir := t.TempDir()
	j := open(t, dir, Options{})
	appendN(t, j, 9)
	if err := j.Ack(7); err != nil {
		t.Fatal(err)
	}
	if err := j.Resync(3); err != nil {
		t.Fatal(err)
	}
	if j.LastAcked() != 3 {
		t.Fatalf("segnalibro dopo Resync %d, atteso 3", j.LastAcked())
	}
	evs := j.Pending(3, 10)
	var d protocol.JournalTruncated
	json.Unmarshal(evs[0].Data, &d)
	if evs[0].Seq != 7 || d.DroppedCount != 4 || d.FirstKeptSeq != 8 || evs[1].Seq != 8 {
		t.Fatalf("dopo Resync: %+v %+v", evs[0], d)
	}
	select {
	case <-j.Notify():
	default:
		t.Fatal("Resync non ha svegliato il mittente")
	}
	// Resync in avanti o uguale: niente.
	if err := j.Resync(5); err != nil || j.Len() != 3 {
		t.Fatalf("Resync in avanti ha cambiato il giornale: %d %v", j.Len(), err)
	}
	j2 := open(t, dir, Options{})
	if j2.LastAcked() != 3 || j2.Len() != 3 {
		t.Fatalf("dopo il riavvio: segnalibro %d, pendenti %d (attesi 3, 3)", j2.LastAcked(), j2.Len())
	}
	b := &backend{last: 3}
	drain(t, j2, b)
	if b.last != 9 {
		t.Fatalf("backend a %d, atteso 9", b.last)
	}
	// Resync a zero (ACK_AHEAD senza events.ack: il backend non ha nulla).
	j3 := open(t, t.TempDir(), Options{})
	appendN(t, j3, 2)
	j3.Ack(2)
	j3.Resync(0)
	drain(t, j3, &backend{})
}

func TestCompattazione(t *testing.T) {
	dir := t.TempDir()
	j := open(t, dir, Options{})
	appendN(t, j, compactAfter+10)
	if err := j.Ack(compactAfter + 5); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(j.eventsPath())
	lines := 0
	for _, c := range raw {
		if c == '\n' {
			lines++
		}
	}
	if lines != 5 {
		t.Fatalf("dopo la compattazione %d righe, attese 5", lines)
	}
}

func TestVersioneDellUltimoAvvio(t *testing.T) {
	dir := t.TempDir()
	j := open(t, dir, Options{})
	if j.LastVersion() != "" {
		t.Fatal("versione iniziale non vuota")
	}
	j.SetLastVersion("0.1.0")
	if open(t, dir, Options{}).LastVersion() != "0.1.0" {
		t.Fatal("versione non conservata")
	}
}

// Un buco nel file (righe perse) si dichiara come troncamento invece di provocare SEQ_GAP.
func TestBucoNelFileDichiarato(t *testing.T) {
	dir := t.TempDir()
	j := open(t, dir, Options{})
	appendN(t, j, 5)
	var keep []byte
	for _, ev := range j.Pending(0, 10) {
		if ev.Seq == 2 || ev.Seq == 3 {
			continue
		}
		line, _ := json.Marshal(ev)
		keep = append(append(keep, line...), '\n')
	}
	os.WriteFile(j.eventsPath(), keep, 0o600)
	j2 := open(t, dir, Options{})
	b := &backend{}
	drain(t, j2, b)
	if b.last != 5 {
		t.Fatalf("backend a %d, atteso 5", b.last)
	}
}
