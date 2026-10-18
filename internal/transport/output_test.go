package transport

import (
	"context"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

func noSleep(context.Context, time.Duration) error { return nil }

// §1.7: oltre 4 MiB in coda il canale si ferma; riparte solo sotto 1 MiB.
func TestContropressione(t *testing.T) {
	q := newOutQueue(4<<20, 1<<20, time.Now)
	big := make([]byte, 3<<20)
	if err := q.Output(1, big); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		_ = q.Output(1, big) // 6 MiB > 4: deve bloccarsi
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("Output non si è bloccato oltre il tetto")
	case <-time.After(100 * time.Millisecond):
	}
	// Si svuota a pezzi da 64 KiB: finché il totale resta sopra 1 MiB, il canale resta fermo.
	for q.Buffered() >= 1<<20+protocol.MaxOutputChunk {
		if _, _, ok := q.next(context.Background(), 0, noSleep); !ok {
			t.Fatal("coda chiusa")
		}
		select {
		case <-released:
			t.Fatalf("ripartito con %d byte in coda, sopra la soglia bassa", q.Buffered())
		default:
		}
	}
	for q.Buffered() >= 1<<20 {
		q.next(context.Background(), 0, noSleep)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatalf("non ripartito con %d byte in coda", q.Buffered())
	}
}

// Pezzi da al massimo 64 KiB, a turno fra i canali; Discard toglie l'output di un canale chiuso;
// close sblocca chi aspetta.
func TestCodaATurnoEChiusura(t *testing.T) {
	q := newOutQueue(4<<20, 1<<20, time.Now)
	_ = q.Output(1, make([]byte, 100<<10))
	_ = q.Output(2, []byte("b"))
	_ = q.Output(3, []byte("c"))
	ch, chunk, _ := q.next(context.Background(), 0, noSleep)
	if ch != 1 || len(chunk) != protocol.MaxOutputChunk {
		t.Fatalf("primo pezzo: canale %d, %d byte", ch, len(chunk))
	}
	ch, _, _ = q.next(context.Background(), 0, noSleep)
	if ch != 2 {
		t.Fatalf("secondo pezzo dal canale %d: il resto del canale 1 doveva andare in fondo", ch)
	}
	q.Discard(3)
	ch, chunk, _ = q.next(context.Background(), 0, noSleep)
	if ch != 1 || len(chunk) != 36<<10 {
		t.Fatalf("terzo pezzo: canale %d, %d byte (il canale 3 era scartato)", ch, len(chunk))
	}
	if q.Buffered() != 0 {
		t.Fatalf("in coda %d byte", q.Buffered())
	}
	done := make(chan bool)
	go func() { _, _, ok := q.next(context.Background(), 0, noSleep); done <- ok }()
	q.close()
	if <-done {
		t.Fatal("next dopo close")
	}
	if err := q.Output(1, []byte("x")); err == nil {
		t.Fatal("Output accettato a coda chiusa")
	}
}

// L'output si accorpa: due scritture ravvicinate partono in un pezzo solo, dopo il ritardo.
func TestAccorpamento(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	q := newOutQueue(4<<20, 1<<20, func() time.Time { return now })
	_ = q.Output(1, []byte("ab"))
	_ = q.Output(1, []byte("cd"))
	var slept time.Duration
	sleep := func(_ context.Context, d time.Duration) error { slept += d; now = now.Add(d); return nil }
	_, chunk, _ := q.next(context.Background(), 5*time.Millisecond, sleep)
	if string(chunk) != "abcd" || slept != 5*time.Millisecond {
		t.Fatalf("pezzo %q dopo %v", chunk, slept)
	}
}
