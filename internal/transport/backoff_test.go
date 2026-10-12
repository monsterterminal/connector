package transport

import (
	"testing"
	"time"
)

// §1.12: da 1 s a 60 s raddoppiando, variazione ±20%, ripartenza dopo un collegamento riuscito.
func TestBackoffCresceFinoAlTetto(t *testing.T) {
	b := DefaultBackoff()
	b.rnd = func() float64 { return 0.5 } // nessuna variazione
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("tentativo %d: %v, atteso %v", i, got, w*time.Second)
		}
	}
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Fatalf("dopo Reset: %v, atteso 1s", got)
	}
}

func TestBackoffVariazioneEntroIlVentiPerCento(t *testing.T) {
	for _, r := range []float64{0, 0.999999} {
		b := DefaultBackoff()
		b.rnd = func() float64 { return r }
		for i := 0; i < 6; i++ {
			b.Next()
		}
		got := b.Next() // base 60 s
		if got < 48*time.Second || got > 72*time.Second {
			t.Fatalf("r=%v: %v fuori da 48-72 s", r, got)
		}
	}
}
