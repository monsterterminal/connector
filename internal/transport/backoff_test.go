package transport

import (
	"testing"
	"time"
)

// §1.12: ogni 4 s per i primi 30 tentativi, poi da 8 s a 60 s raddoppiando, variazione ±20%,
// ripartenza dopo un collegamento riuscito.
func TestBackoffRapidoPoiCresceFinoAlTetto(t *testing.T) {
	b := DefaultBackoff()
	b.rnd = func() float64 { return 0.5 } // nessuna variazione
	var totale time.Duration
	for i := 0; i < 30; i++ {
		got := b.Next()
		if got != 4*time.Second {
			t.Fatalf("tentativo %d: %v, atteso 4s", i, got)
		}
		totale += got
	}
	if totale != 2*time.Minute {
		t.Fatalf("fase rapida di %v, attesi 2 minuti", totale)
	}
	for i, w := range []time.Duration{8, 16, 32, 60, 60} {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("dopo la fase rapida, tentativo %d: %v, atteso %v", i, got, w*time.Second)
		}
	}
	b.Reset()
	if got := b.Next(); got != 4*time.Second {
		t.Fatalf("dopo Reset: %v, atteso 4s", got)
	}
}

func TestBackoffRapidoEntroTreCinqueSecondi(t *testing.T) {
	for _, r := range []float64{0, 0.999999} {
		b := DefaultBackoff()
		b.rnd = func() float64 { return r }
		if got := b.Next(); got < 3*time.Second || got > 5*time.Second {
			t.Fatalf("r=%v: %v fuori da 3-5 s", r, got)
		}
	}
}

// Senza fase rapida resta la sola crescita: è la forma usata dai banchi dei test.
func TestBackoffSenzaFaseRapida(t *testing.T) {
	b := &Backoff{Min: time.Second, Max: 4 * time.Second}
	for i, w := range []time.Duration{1, 2, 4, 4} {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("tentativo %d: %v, atteso %v", i, got, w*time.Second)
		}
	}
}

func TestBackoffVariazioneEntroIlVentiPerCento(t *testing.T) {
	for _, r := range []float64{0, 0.999999} {
		b := DefaultBackoff()
		b.rnd = func() float64 { return r }
		for i := 0; i < b.Fast+6; i++ {
			b.Next()
		}
		got := b.Next() // base 60 s
		if got < 48*time.Second || got > 72*time.Second {
			t.Fatalf("r=%v: %v fuori da 48-72 s", r, got)
		}
	}
}
