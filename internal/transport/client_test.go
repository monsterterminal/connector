// Test del collegamento contro il server finto (spec monsterterminal-saas.md §1.2, §1.4, §1.12, §1.13).
package transport_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/transport"
)

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	// attesa reale minima, per non girare a vuoto
	t := time.NewTimer(10 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *sleeps) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

func hello() protocol.Hello {
	return protocol.Hello{ConnectorVersion: "0.1.0-dev", Protocol: 1, OS: "linux", Arch: "amd64",
		RunAsUser: "monsterterminal", Capabilities: []string{}, Agents: []protocol.Agent{}}
}

type run struct {
	cfg      transport.Config
	sl       *sleeps
	welcomes chan protocol.Welcome
	done     chan error
	cancel   context.CancelFunc
}

func start(t *testing.T, s *fakeserver.Server, token string, mod func(*transport.Config)) *run {
	t.Helper()
	r := &run{sl: &sleeps{}, welcomes: make(chan protocol.Welcome, 16), done: make(chan error, 1)}
	r.cfg = transport.Config{
		URL: s.WSURL(), Token: token, HTTPClient: s.Client(), Hello: hello,
		Sleep: r.sl.sleep, OnWelcome: func(w protocol.Welcome) { r.welcomes <- w },
		WelcomeTimeout: 5 * time.Second,
	}
	if mod != nil {
		mod(&r.cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	r.cancel = cancel
	go func() { r.done <- transport.Run(ctx, r.cfg) }()
	t.Cleanup(cancel)
	return r
}

func waitHello(t *testing.T, s *fakeserver.Server) protocol.Hello {
	t.Helper()
	select {
	case h := <-s.Hellos():
		return h
	case <-time.After(5 * time.Second):
		t.Fatal("hello non arrivato")
	}
	return protocol.Hello{}
}

func waitWelcome(t *testing.T, r *run) protocol.Welcome {
	t.Helper()
	select {
	case w := <-r.welcomes:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("welcome non arrivato")
	}
	return protocol.Welcome{}
}

func waitDone(t *testing.T, r *run) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run non è terminato")
	}
	return nil
}

func TestCollegamentoHelloWelcome(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	r := start(t, s, tok, nil)
	if h := waitHello(t, s); h.Protocol != 1 || h.OS != "linux" || h.RunAsUser != "monsterterminal" {
		t.Fatalf("hello inatteso: %+v", h)
	}
	if w := waitWelcome(t, r); w.HostID != id || w.Config.DefaultIdleThresholdSeconds != 300 {
		t.Fatalf("welcome inatteso: %+v", w)
	}
	r.cancel()
	if err := waitDone(t, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run terminato con %v", err)
	}
}

func TestCadutaERicollegamento(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	r := start(t, s, tok, nil)
	waitHello(t, s)
	waitWelcome(t, r)
	s.Drop(id)
	waitHello(t, s) // nuovo hello dopo la caduta
	waitWelcome(t, r)
	if s.Connects() != 2 {
		t.Fatalf("collegamenti %d, attesi 2", s.Connects())
	}
	sl := r.sl.all()
	if len(sl) != 1 || sl[0] < 800*time.Millisecond || sl[0] > 1200*time.Millisecond {
		t.Fatalf("attese %v: attesa una sola di circa 1 s (la prima, dopo un collegamento riuscito)", sl)
	}
}

func TestAttesaCrescenteFinchéIlBackendRifiuta(t *testing.T) {
	s := fakeserver.New()
	_, tok := s.AddHost()
	r := start(t, s, tok, nil)
	waitHello(t, s)
	waitWelcome(t, r)
	s.Close() // il backend sparisce: la connessione cade e ogni tentativo fallisce
	deadline := time.After(10 * time.Second)
	for len(r.sl.all()) < 4 {
		select {
		case <-deadline:
			t.Fatalf("attese registrate: %v", r.sl.all())
		case <-time.After(20 * time.Millisecond):
		}
	}
	sl := r.sl.all()
	for i := 1; i < 4; i++ {
		if sl[i] <= sl[i-1] {
			t.Fatalf("attese non crescenti: %v", sl[:4])
		}
	}
}

func TestRevocaDuranteIlCollegamentoFermaIlConnettore(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	r := start(t, s, tok, nil)
	waitHello(t, s)
	waitWelcome(t, r)
	s.Revoke(id)
	if err := waitDone(t, r); !errors.Is(err, transport.ErrRevoked) {
		t.Fatalf("atteso ErrRevoked, ottenuto %v", err)
	}
	if s.Connects() != 1 || len(r.sl.all()) != 0 {
		t.Fatalf("dopo la revoca: %d collegamenti, attese %v", s.Connects(), r.sl.all())
	}
}

func TestHostGiàRevocatoAllUpgrade(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	s.Revoke(id)
	r := start(t, s, tok, nil)
	if err := waitDone(t, r); !errors.Is(err, transport.ErrRevoked) {
		t.Fatalf("atteso ErrRevoked, ottenuto %v", err)
	}
}

func TestTokenSconosciutoFermaIlConnettore(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	r := start(t, s, "mth_sconosciutosconosciutosconosciutosconosciuto", nil)
	if err := waitDone(t, r); !errors.Is(err, transport.ErrTokenInvalid) {
		t.Fatalf("atteso ErrTokenInvalid, ottenuto %v", err)
	}
}

func TestKillSwitchAttendeAlmenoQuindiciMinuti(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	r := start(t, s, tok, nil)
	waitHello(t, s)
	waitWelcome(t, r)
	s.Disable(id)
	deadline := time.After(5 * time.Second)
	for len(r.sl.all()) < 2 {
		select {
		case <-deadline:
			t.Fatalf("attese registrate: %v", r.sl.all())
		case <-time.After(20 * time.Millisecond):
		}
	}
	for i, d := range r.sl.all()[:2] {
		if d < transport.DisabledMinWait {
			t.Fatalf("attesa %d dopo il kill-switch: %v, minimo 15 minuti", i, d)
		}
	}
	select {
	case err := <-r.done:
		t.Fatalf("il connettore si è fermato dopo il kill-switch: %v", err)
	default:
	}
}

func TestHeartbeatScadutoChiudeERicollega(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	s.PingInterval = 50 * time.Millisecond
	_, tok := s.AddHost()
	r := start(t, s, tok, func(c *transport.Config) { c.HeartbeatTimeout = 400 * time.Millisecond })
	waitHello(t, s)
	waitWelcome(t, r)
	time.Sleep(600 * time.Millisecond) // i ping tengono viva la connessione oltre il tempo dell'heartbeat
	if s.Connects() != 1 {
		t.Fatalf("connessione caduta nonostante i ping: %d collegamenti", s.Connects())
	}
	s.Silence()
	waitHello(t, s) // dopo il silenzio il connettore chiude e si ricollega
	if s.Connects() != 2 {
		t.Fatalf("collegamenti %d, attesi 2", s.Connects())
	}
}

func TestConnessioneSostituitaSiRicollega(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	_, tok := s.AddHost()
	r1 := start(t, s, tok, nil)
	waitHello(t, s)
	waitWelcome(t, r1)
	r1.cancel()
	waitDone(t, r1)
	r2 := start(t, s, tok, nil)
	waitHello(t, s)
	waitWelcome(t, r2)
	if s.Connects() != 2 {
		t.Fatalf("collegamenti %d, attesi 2", s.Connects())
	}
}

func TestCertificatoNonFidatoNonInviaHello(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	_, tok := s.AddHost()
	r := start(t, s, tok, func(c *transport.Config) { c.HTTPClient = &http.Client{} })
	deadline := time.After(3 * time.Second)
	for len(r.sl.all()) < 2 {
		select {
		case <-deadline:
			t.Fatalf("nessun tentativo registrato")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if s.Connects() != 0 {
		t.Fatalf("upgrade riuscito con un certificato non fidato: %d", s.Connects())
	}
}

func TestURLNonCifratoRifiutato(t *testing.T) {
	err := transport.Run(context.Background(), transport.Config{URL: "ws://connect.example/v1", Hello: hello})
	if err == nil {
		t.Fatal("ws:// accettato")
	}
}

// Barriera del billing (ADR 0418 D5, §3.13): la chiusura 4402 e il rifiuto 402 BILLING_REQUIRED
// all'upgrade si trattano come un kill-switch (attesa minima di 15 minuti, nessuna uscita) e, tolta
// la barriera, il connettore si riaggancia da solo. Ogni upgrade dichiara X-MonsterTerminal-Schema.
func TestAbbonamentoMancanteAttendeERiaggancia(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	_, tok := s.AddHost()
	r := start(t, s, tok, nil)
	waitHello(t, s)
	waitWelcome(t, r)
	s.RequireBilling()
	deadline := time.After(5 * time.Second)
	for len(r.sl.all()) < 2 {
		select {
		case <-deadline:
			t.Fatalf("attese registrate: %v", r.sl.all())
		case <-time.After(20 * time.Millisecond):
		}
	}
	for i, d := range r.sl.all()[:2] {
		if d < transport.DisabledMinWait {
			t.Fatalf("attesa %d senza abbonamento: %v, minimo 15 minuti", i, d)
		}
	}
	select {
	case err := <-r.done:
		t.Fatalf("il connettore si è fermato per il billing: %v", err)
	default:
	}
	s.AllowBilling()
	waitWelcome(t, r)
	for i, v := range s.DeclaredSchemas() {
		if v != "0.6.0" {
			t.Fatalf("upgrade %d senza X-MonsterTerminal-Schema 0.6.0: %q", i, v)
		}
	}
}
