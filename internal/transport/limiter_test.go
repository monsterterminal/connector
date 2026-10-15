package transport

import (
	"testing"
	"time"
)

// §1.17: il secchio lascia passare la raffica, poi un messaggio ogni 1/rate secondi.
func TestLimitatore(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	l := newLimiter(40, 10, func() time.Time { return now })
	for i := 0; i < 10; i++ {
		if d := l.reserve(); d != 0 {
			t.Fatalf("messaggio %d della raffica in attesa %v", i, d)
		}
	}
	if d := l.reserve(); d != 25*time.Millisecond {
		t.Fatalf("undicesimo: attesa %v, attesi 25 ms", d)
	}
	now = now.Add(time.Second)
	// Dopo un secondo il secchio è di nuovo pieno (mai oltre la raffica).
	for i := 0; i < 10; i++ {
		if d := l.reserve(); d != 0 {
			t.Fatalf("dopo un secondo, messaggio %d in attesa %v", i, d)
		}
	}
	if d := l.reserve(); d == 0 {
		t.Fatal("il secchio ha superato la raffica")
	}
}

// In un secondo qualsiasi il limitatore di default resta a 45 messaggi, sotto i 50 del backend.
func TestLimitatoreDiDefaultSottoICinquanta(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	l := newLimiter(DefaultMaxRate, DefaultRateBurst, func() time.Time { return now })
	var sent []time.Time
	for len(sent) < 500 {
		if d := l.reserve(); d > 0 {
			now = now.Add(d)
		}
		sent = append(sent, now)
	}
	for i := range sent {
		n := 0
		for k := i; k < len(sent) && sent[k].Sub(sent[i]) < time.Second; k++ {
			n++
		}
		if n > 45 {
			t.Fatalf("%d messaggi nel secondo che parte da %v", n, sent[i])
		}
	}
}
