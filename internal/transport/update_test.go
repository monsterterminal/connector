// L'aggiornamento automatico visto dal trasporto (ADR 0438 D4): il gestore si aggancia DOPO welcome
// (quando l'interruttore del backend è già arrivato con OnWelcome), riceve update.available e
// update.none prima che il trasporto risponda TYPE_UNKNOWN, e si sgancia alla caduta.
package transport_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterterminal/connector/internal/fakeserver"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/transport"
	"github.com/monsterterminal/connector/internal/update"
)

type fakeUpdates struct {
	mu       sync.Mutex
	attached int
	detached int
	handled  []string
}

func (f *fakeUpdates) Attach(out update.Out) {
	f.mu.Lock()
	f.attached++
	f.mu.Unlock()
	_ = out.Control(protocol.TypeUpdateCheck, "", struct{}{})
}

func (f *fakeUpdates) Handle(env protocol.Envelope) bool {
	if env.Type != protocol.TypeUpdateAvailable && env.Type != protocol.TypeUpdateNone {
		return false
	}
	f.mu.Lock()
	f.handled = append(f.handled, env.Type+"/"+env.Re)
	f.mu.Unlock()
	return true
}

func (f *fakeUpdates) Detach() {
	f.mu.Lock()
	f.detached++
	f.mu.Unlock()
}

func (f *fakeUpdates) snapshot() (int, int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attached, f.detached, append([]string(nil), f.handled...)
}

func TestAggiornamentiAgganciatiDopoWelcome(t *testing.T) {
	s := fakeserver.New()
	defer s.Close()
	id, tok := s.AddHost()
	off := false
	s.SetAutoUpdate(&off)
	fu := &fakeUpdates{}
	r := start(t, s, tok, func(c *transport.Config) {
		c.Updates = fu
		c.Sleep = realSleep
		c.Backoff = &transport.Backoff{Min: 20 * time.Millisecond, Max: 100 * time.Millisecond}
	})
	t.Cleanup(r.cancel)
	waitHello(t, s)
	if w := waitWelcome(t, r); w.Config.AutoUpdate == nil || *w.Config.AutoUpdate {
		t.Fatalf("welcome.config.autoUpdate %v, atteso false", w.Config.AutoUpdate)
	}
	// L'update.check mandato da Attach arriva al server e riceve update.none (nessuna offerta), con re.
	eventually(t, "update.check al server", func() bool { return s.UpdateChecks() == 1 })
	eventually(t, "update.none gestito", func() bool { _, _, h := fu.snapshot(); return len(h) == 1 })
	if _, _, h := fu.snapshot(); !strings.HasPrefix(h[0], protocol.TypeUpdateNone+"/") || len(h[0]) == len(protocol.TypeUpdateNone)+1 {
		t.Fatalf("update.none senza re: %v", h)
	}
	// Un update.available spinto (senza re) va al gestore, non a TYPE_UNKNOWN.
	sec := true
	m := protocol.UpdateManifest{
		Arch:        "amd64",
		MinProtocol: 1,
		OS:          "linux",
		Security:    &sec,
		SHA256:      strings.Repeat("0", 64),
		URL:         "https://github.com/monsterterminal/connector/releases/download/v9.9.9/x",
		Version:     "9.9.9",
	}
	o := fakeserver.Offer{Manifest: m, Signature: strings.Repeat("A", 86) + "=="}
	if n := s.PushUpdate(o); n != 1 {
		t.Fatalf("spinto a %d host", n)
	}
	eventually(t, "update.available gestito", func() bool { _, _, h := fu.snapshot(); return len(h) == 2 })
	if a, d, h := fu.snapshot(); a != 1 || d != 0 || h[1] != protocol.TypeUpdateAvailable+"/" {
		t.Fatalf("attach %d detach %d gestiti %v", a, d, h)
	}
	// Alla caduta il gestore si sgancia; al nuovo welcome si riaggancia e chiede di nuovo.
	s.Drop(id)
	waitHello(t, s)
	waitWelcome(t, r)
	eventually(t, "secondo update.check", func() bool { return s.UpdateChecks() == 2 })
	if a, d, _ := fu.snapshot(); a != 2 || d != 1 {
		t.Fatalf("dopo la caduta: attach %d detach %d", a, d)
	}
	// Con un'offerta, update.check riceve update.available con re.
	s.SetOffer(&o)
	s.Drop(id)
	waitHello(t, s)
	waitWelcome(t, r)
	eventually(t, "update.available con re", func() bool {
		_, _, h := fu.snapshot()
		if len(h) < 4 {
			return false
		}
		last := h[len(h)-1]
		return strings.HasPrefix(last, protocol.TypeUpdateAvailable+"/") && len(last) > len(protocol.TypeUpdateAvailable)+1
	})
	r.cancel()
	waitDone(t, r)
	if _, d, _ := fu.snapshot(); d != 3 {
		t.Fatalf("detach %d, attesi 3", d)
	}
}
