package transport

import (
	"math/rand/v2"
	"time"
)

// Backoff calcola l'attesa fra i tentativi di collegamento (§1.12): i primi Fast tentativi ogni
// FastEvery, poi da Min a Max raddoppiando, con una variazione casuale di ±Jitter.
type Backoff struct {
	Min, Max  time.Duration
	Fast      int
	FastEvery time.Duration
	Jitter    float64
	attempt   int
	rnd       func() float64 // in [0,1); sostituibile nei test
}

// DefaultBackoff è l'attesa della spec: ogni 4 s (3,2-4,8 con la variazione del 20%) per circa 2
// minuti, poi da 8 s a 60 s. Il backend che si riavvia a un deploy torna in meno di un minuto: con la
// sola attesa esponenziale il connettore cadeva nel buco fra i tentativi a 31 e 63 s.
func DefaultBackoff() *Backoff {
	return &Backoff{Min: 8 * time.Second, Max: 60 * time.Second, Fast: 30, FastEvery: 4 * time.Second, Jitter: 0.2}
}

// Next restituisce la prossima attesa e avanza.
func (b *Backoff) Next() time.Duration {
	d := b.FastEvery
	if b.attempt >= b.Fast {
		d = b.Min
		for i := b.Fast; i < b.attempt && d < b.Max; i++ {
			d *= 2
		}
		if d > b.Max {
			d = b.Max
		}
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
