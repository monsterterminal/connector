// Test della consegna degli eventi contro il server finto allineato al gateway vero (ADR 0409,
// verbale 2026-10-13; spec monsterterminal-saas.md §1.12 emendata 2026-10-14, §1.16, §1.17).
package transport_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/journal"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/transport"
)

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// withEvents collega il giornale e usa attese vere ma corte: i tempi del riallineamento contano.
func withEvents(j *journal.Journal) func(*transport.Config) {
	return func(c *transport.Config) {
		c.Events = j
		c.Sleep = realSleep
		c.SyncGrace = 150 * time.Millisecond
		c.AckAheadWait = 300 * time.Millisecond
		c.Backoff = &transport.Backoff{Min: 20 * time.Millisecond, Max: 100 * time.Millisecond}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("non avvenuto entro 10 s: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newJournal(t *testing.T, n int) *journal.Journal {
	t.Helper()
	j, err := journal.Open(t.TempDir(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := j.Append("test.event", map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	return j
}

func seqs(evs []protocol.Event) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEventiConsegnatiEConfermati(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	j := newJournal(t, 5)
	start(t, s, tok, withEvents(j))
	if h := waitHello(t, s); h.LastAckedSeq != 0 {
		t.Fatalf("hello.lastAckedSeq %d, atteso 0", h.LastAckedSeq)
	}
	eventually(t, "giornale confermato fino a 5", func() bool { return j.LastAcked() == 5 })
	// Un evento nuovo parte subito, senza ricollegamento.
	if _, err := j.Append("test.event", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "evento 6 confermato", func() bool { return j.LastAcked() == 6 })
	if got := seqs(s.Events(id)); !equal(got, []int64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("eventi ricevuti dal backend %v", got)
	}
	if s.Connects() != 1 {
		t.Fatalf("collegamenti %d, atteso 1", s.Connects())
	}
}

// Il backend accetta l'evento 3 e chiude con 1011 prima dell'ack: al ricollegamento hello dice 2,
// il backend risponde col suo segnalibro 3, e il connettore riprende dal 4 senza doppioni.
func TestChiusura1011FraScritturaEConfermaRiprendeDalSegnalibro(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	s.CloseAfterEvent(3, transport.CloseInternal)
	j := newJournal(t, 5)
	r := start(t, s, tok, withEvents(j))
	waitHello(t, s)
	if h := waitHello(t, s); h.LastAckedSeq != 2 {
		t.Fatalf("hello dopo la caduta: lastAckedSeq %d, atteso 2", h.LastAckedSeq)
	}
	eventually(t, "giornale confermato fino a 5", func() bool { return j.LastAcked() == 5 })
	if got := seqs(s.Events(id)); !equal(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("eventi ricevuti dal backend %v", got)
	}
	select {
	case err := <-r.done:
		t.Fatalf("il connettore si è fermato dopo 1011: %v", err)
	default:
	}
}

// ACK_AHEAD (§1.12, fa fede il backend): il connettore credeva confermati 1..6, il backend ne ha 2.
// Il giornale dichiara persi 3..6 con un journal.truncated coerente e consegna il resto.
func TestAckAheadRiallineaAlSegnalibroDelBackend(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	s.SetBookmark(id, 2)
	j := newJournal(t, 8)
	if err := j.Ack(6); err != nil {
		t.Fatal(err)
	}
	start(t, s, tok, withEvents(j))
	if h := waitHello(t, s); h.LastAckedSeq != 6 {
		t.Fatalf("hello.lastAckedSeq %d, atteso 6", h.LastAckedSeq)
	}
	eventually(t, "giornale confermato fino a 8", func() bool { return j.LastAcked() == 8 })
	evs := s.Events(id)
	if got := seqs(evs); !equal(got, []int64{6, 7, 8}) {
		t.Fatalf("eventi ricevuti dal backend %v, attesi il troncamento 6 poi 7, 8", got)
	}
	var d protocol.JournalTruncated
	_ = json.Unmarshal(evs[0].Data, &d)
	if evs[0].Kind != protocol.KindJournalTruncated || d.DroppedCount != 4 || d.FirstKeptSeq != 7 {
		t.Fatalf("troncamento %s %+v", evs[0].Kind, d)
	}
	if s.Connects() != 1 {
		t.Fatalf("collegamenti %d: il riallineamento non deve passare da un ricollegamento", s.Connects())
	}
}

// ACK_AHEAD senza events.ack: il backend non ha nulla (segnalibro 0).
func TestAckAheadSenzaSegnalibroDichiaraPersoTutto(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	j := newJournal(t, 4)
	if err := j.Ack(3); err != nil {
		t.Fatal(err)
	}
	start(t, s, tok, withEvents(j))
	eventually(t, "giornale confermato fino a 4", func() bool { return j.LastAcked() == 4 })
	evs := s.Events(id)
	var d protocol.JournalTruncated
	_ = json.Unmarshal(evs[0].Data, &d)
	if got := seqs(evs); !equal(got, []int64{3, 4}) || d.DroppedCount != 3 {
		t.Fatalf("eventi %v, troncamento %+v", got, d)
	}
}

// SEQ_GAP: il backend ha perso il segnalibro a metà sessione. Il connettore chiude, si ricollega,
// hello porta ACK_AHEAD e il riallineamento dichiara la perdita: nessun ciclo di rifiuti.
func TestSeqGapChiudeERiallineaAlRicollegamento(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	j := newJournal(t, 3)
	start(t, s, tok, withEvents(j))
	eventually(t, "confermati 1..3", func() bool { return j.LastAcked() == 3 })
	s.SetBookmark(id, 0)
	if _, err := j.Append("test.event", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "confermato 4 dopo il riallineamento", func() bool { return j.LastAcked() == 4 && s.Bookmark(id) == 4 })
	if s.Connects() != 2 {
		t.Fatalf("collegamenti %d, attesi 2 (SEQ_GAP chiude la sessione)", s.Connects())
	}
}

// §1.17: 50 messaggi al secondo per host. Il connettore resta sotto: 150 eventi in fila passano
// senza 4429. La controprova con un tetto oltre i 50 mostra che il server finto morde davvero.
func TestInvioDosatoSottoIlLimiteDelBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("richiede qualche secondo")
	}
	run := func(rate, burst int) (*fakeserver.Server, string, *journal.Journal) {
		s := fakeserver.New()
		id, tok := s.AddHost()
		j := newJournal(t, 150)
		start(t, s, tok, func(c *transport.Config) {
			withEvents(j)(c)
			c.MaxRate, c.RateBurst = rate, burst
		})
		return s, id, j
	}
	s, id, j := run(0, 0) // limiti di default
	defer s.Close()
	began := time.Now()
	eventually(t, "150 eventi confermati", func() bool { return j.LastAcked() == 150 })
	if s.Connects() != 1 {
		t.Fatalf("collegamenti %d: il backend ha chiuso per troppi messaggi", s.Connects())
	}
	if el := time.Since(began); el < 3*time.Second {
		t.Fatalf("150 eventi in %v: troppo veloce per 40 al secondo", el)
	}
	if got := len(s.Events(id)); got != 150 {
		t.Fatalf("eventi ricevuti %d", got)
	}

	c, _, _ := run(1000, 200)
	defer c.Close()
	eventually(t, "chiusura 4429 senza dosaggio", func() bool { return c.Connects() >= 2 })
}

func TestRateLimited429RispettaRetryAfter(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	_, tok := s.AddHost()
	s.RejectUpgrades(
		fakeserver.Rejection{Status: 429, Code: "RATE_LIMITED", RetryAfter: "120"},
		fakeserver.Rejection{Status: 503, Code: "UNAVAILABLE", RetryAfter: "5"},
		fakeserver.Rejection{Status: 503, Code: "UNAVAILABLE"},
	)
	r := start(t, s, tok, nil)
	waitHello(t, s)
	waitWelcome(t, r)
	sl := r.sl.all()
	if len(sl) != 3 {
		t.Fatalf("attese %v, attese 3", sl)
	}
	if sl[0] < 120*time.Second {
		t.Fatalf("dopo 429 Retry-After 120 attesi almeno 120 s, trovato %v", sl[0])
	}
	if sl[1] < 5*time.Second {
		t.Fatalf("dopo 503 Retry-After 5 attesi almeno 5 s, trovato %v", sl[1])
	}
	if sl[2] >= 5*time.Second {
		t.Fatalf("dopo 503 senza Retry-After vale il backoff, trovato %v", sl[2])
	}
	select {
	case err := <-r.done:
		t.Fatalf("il connettore si è fermato: %v", err)
	default:
	}
}

func TestChiusura1011NonFermaIlConnettore(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	r := start(t, s, tok, nil)
	waitHello(t, s)
	waitWelcome(t, r)
	s.CloseWith(id, transport.CloseInternal)
	waitHello(t, s)
	if sl := r.sl.all(); len(sl) != 1 {
		t.Fatalf("attese %v, attesa una sola col backoff", sl)
	}
}

// Il troncamento ricalcolato all'invio passa anche quando il giornale scarta eventi già in volo:
// il backend è lento (AckDelay), tre eventi sono inviati e non confermati, e intanto il giornale
// pieno scarta anche quelli. Senza journal.Rebase il primo troncamento dopo gli inviati dichiara
// eventi che il backend ha già, e ogni ricollegamento finirebbe in TRUNCATION_INCOHERENT.
func TestTroncamentoDuranteLaSessione(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.AckDelay = 100 * time.Millisecond
	id, tok := s.AddHost()
	j, err := journal.Open(t.TempDir(), journal.Options{MaxEvents: 10})
	if err != nil {
		t.Fatal(err)
	}
	start(t, s, tok, func(c *transport.Config) {
		withEvents(j)(c)
		c.MaxInFlight = 3
	})
	waitHello(t, s)
	for i := 0; i < 3; i++ {
		if _, err := j.Append("test.event", nil); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "primo evento accettato", func() bool { return s.Bookmark(id) >= 1 })
	for i := 0; i < 57; i++ {
		if _, err := j.Append("test.event", nil); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "giornale confermato fino a 60", func() bool { return j.LastAcked() == 60 })
	// Ogni seq 1..60 consegnato o dichiarato perso esattamente una volta.
	var total int64
	for _, ev := range s.Events(id) {
		if ev.Kind == protocol.KindJournalTruncated {
			var d protocol.JournalTruncated
			_ = json.Unmarshal(ev.Data, &d)
			total += d.DroppedCount
		} else {
			total++
		}
	}
	if total != 60 {
		t.Fatalf("seq coperti %d, attesi 60", total)
	}
	if s.Connects() != 1 {
		t.Fatalf("collegamenti %d: un troncamento è stato rifiutato", s.Connects())
	}
}
