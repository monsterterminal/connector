package transport

import (
	"math/rand/v2"
	"time"
)

// Backoff calcola l'attesa crescente fra i tentativi di collegamento (§1.12): da Min a Max
// raddoppiando, con una variazione casuale di ±Jitter.
type Backoff struct {
	Min, Max time.Duration
	Jitter   float64
	attempt  int
	rnd      func() float64 // in [0,1); sostituibile nei test
}

// DefaultBackoff è l'attesa della spec: da 1 s a 60 s, variazione del 20%.
func DefaultBackoff() *Backoff {
	return &Backoff{Min: time.Second, Max: 60 * time.Second, Jitter: 0.2}
}

// Next restituisce la prossima attesa e avanza.
func (b *Backoff) Next() time.Duration {
	d := b.Min
	for i := 0; i < b.attempt && d < b.Max; i++ {
		d *= 2
	}
	if d > b.Max {
		d = b.Max
	}
	b.attempt++
	r := rand.Float64
	if b.rnd != nil {
		r = b.rnd
	}
	f := 1 + b.Jitter*(2*r()-1)
	return time.Duration(float64(d) * f)
}

// Reset riparte dall'attesa minima, dopo un collegamento riuscito.
func (b *Backoff) Reset() { b.attempt = 0 }
