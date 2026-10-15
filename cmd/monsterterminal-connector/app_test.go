// Test del cablaggio del comando (register, run, version) contro il server finto: flag, lettura
// del token di installazione, sostituzione, file del token, giornale, connector.restarted, codici
// di uscita. Spec monsterterminal-saas.md §1.3, §1.8, §1.12, §1.13; ADR 0409.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ciarasino/monsterterminal-connector/internal/fakeserver"
	"github.com/ciarasino/monsterterminal-connector/internal/hostinfo"
	"github.com/ciarasino/monsterterminal-connector/internal/protocol"
	"github.com/ciarasino/monsterterminal-connector/internal/register"
	"github.com/ciarasino/monsterterminal-connector/internal/transport"
)

const (
	installTok = "mti_installazione-di-prova-0001"
	machineID  = "0123456789abcdef0123456789abcdef"
)

// syncBuf è un buffer sicuro per il log scritto da più goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type harness struct {
	s         *fakeserver.Server
	dir       string
	tokenFile string
	stateDir  string
	env       map[string]string
	stdin     string
	stdout    *syncBuf
	stderr    *syncBuf
	boot      time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s := fakeserver.New()
	t.Cleanup(s.Close)
	dir := t.TempDir()
	return &harness{
		s: s, dir: dir, tokenFile: filepath.Join(dir, "etc", "host-token"),
		stateDir: filepath.Join(dir, "state"), env: map[string]string{},
		stdout: &syncBuf{}, stderr: &syncBuf{}, boot: time.Now().Add(-24 * time.Hour),
	}
}

func (h *harness) app() app {
	return app{
		getenv:     func(k string) string { return h.env[k] },
		stdin:      strings.NewReader(h.stdin),
		stdout:     h.stdout,
		stderr:     h.stderr,
		httpClient: h.s.Client(),
		collect: func() (hostinfo.Info, error) {
			return hostinfo.Info{MachineID: machineID, Hostname: "srv-prova", Distro: "ubuntu",
				DistroVersion: "24.04", Arch: "amd64", User: "monsterterminal"}, nil
		},
		bootTime: func() (time.Time, error) { return h.boot, nil },
		now:      time.Now,
		configure: func(c *transport.Config) {
			c.SyncGrace = 50 * time.Millisecond
			c.Backoff = &transport.Backoff{Min: 20 * time.Millisecond, Max: 100 * time.Millisecond}
		},
	}
}

func (h *harness) args(cmd string, extra ...string) []string {
	return append([]string{cmd, "-server", h.s.BaseURL(), "-token-file", h.tokenFile, "-state-dir", h.stateDir}, extra...)
}

func (h *harness) register(t *testing.T, extra ...string) int {
	t.Helper()
	return h.app().run(context.Background(), h.args("register", extra...))
}

// start lancia run in background; restituisce l'uscita e la funzione che lo ferma come un SIGTERM.
func (h *harness) start(t *testing.T) (<-chan int, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- h.app().run(ctx, h.args("run")) }()
	t.Cleanup(cancel)
	return done, cancel
}

func waitExit(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case c := <-done:
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("run non è terminato")
	}
	return -1
}

func waitHello(t *testing.T, s *fakeserver.Server) protocol.Hello {
	t.Helper()
	select {
	case h := <-s.Hellos():
		return h
	case <-time.After(10 * time.Second):
		t.Fatal("hello non arrivato")
	}
	return protocol.Hello{}
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

func hostIDFromStdout(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, "id ")
	if i < 0 {
		t.Fatalf("id non stampato: %q", out)
	}
	return strings.TrimRight(out[i+3:i+3+36], ")")
}

func TestRegisterDaVariabileDAmbiente(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	if code := h.register(t); code != exitOK {
		t.Fatalf("uscita %d, stderr %s", code, h.stderr.String())
	}
	st, err := os.Stat(h.tokenFile)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file del token: %v %v", st, err)
	}
	regs := h.s.Registrations()
	if len(regs) != 1 || regs[0].MachineID != machineID || regs[0].ConnectorVersion != version || regs[0].OS != "linux" {
		t.Fatalf("registrazione ricevuta %+v", regs)
	}
	if !strings.Contains(h.stdout.String(), "Host registrato per Acme") {
		t.Fatalf("stdout %q", h.stdout.String())
	}
	if strings.Contains(h.stdout.String()+h.stderr.String(), installTok) {
		t.Fatal("il token di installazione è finito nell'output")
	}
}

func TestRegisterDaStandardInput(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.stdin = installTok + "\n"
	if code := h.register(t); code != exitOK {
		t.Fatalf("uscita %d, stderr %s", code, h.stderr.String())
	}
}

func TestRegisterSenzaTokenDiInstallazione(t *testing.T) {
	h := newHarness(t)
	if code := h.register(t); code != exitError || !strings.Contains(h.stderr.String(), "MONSTERTERMINAL_INSTALL_TOKEN") {
		t.Fatalf("uscita %d, stderr %s", code, h.stderr.String())
	}
}

// Host già registrato: senza -replace non parte nessuna richiesta.
func TestRegisterRifiutaSeGiaRegistratoSenzaReplace(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	h.s.AddInstallToken(installTok+"b", time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok + "b"
	if code := h.register(t); code != exitError || !strings.Contains(h.stderr.String(), "-replace") {
		t.Fatalf("uscita %d, stderr %s", code, h.stderr.String())
	}
	if len(h.s.Registrations()) != 1 || !h.s.InstallTokenUsable(installTok+"b") {
		t.Fatal("la seconda registrazione non doveva partire")
	}
}

// -replace: il token attuale va nell'header, il vecchio host è revocato, il file contiene il nuovo.
func TestRegisterReplaceSostituisceColTokenAttuale(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	oldID := hostIDFromStdout(t, h.stdout.String())
	oldTok, _ := register.LoadToken(h.tokenFile)

	h.s.AddInstallToken(installTok+"b", time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok + "b"
	if code := h.register(t, "-replace"); code != exitOK {
		t.Fatalf("uscita %d, stderr %s", code, h.stderr.String())
	}
	if hd := h.s.ReplaceHeaders(); len(hd) != 2 || hd[1] != oldTok {
		t.Fatal("il token attuale non è arrivato nell'header della sostituzione")
	}
	if !h.s.HostRevoked(oldID) {
		t.Fatal("il vecchio host non è stato revocato")
	}
	newTok, _ := register.LoadToken(h.tokenFile)
	if newTok == oldTok {
		t.Fatal("il file del token non è stato aggiornato")
	}
	if !strings.Contains(h.stdout.String(), "Host sostituito") {
		t.Fatalf("stdout %q", h.stdout.String())
	}
}

func TestRegisterReplaceSenzaHostDaSostituire(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	if code := h.register(t, "-replace"); code != exitError || len(h.s.Registrations()) != 0 {
		t.Fatalf("uscita %d, stderr %s", code, h.stderr.String())
	}
}

// 403 HOST_DISABLED alla registrazione: errore con la spiegazione, nessun file del token.
func TestRegisterMacchinaFermata(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.s.DisableMachine(machineID)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	if code := h.register(t); code != exitError {
		t.Fatalf("uscita %d", code)
	}
	if !strings.Contains(h.stderr.String(), "HOST_DISABLED") || !strings.Contains(h.stderr.String(), "kill-switch") {
		t.Fatalf("stderr senza spiegazione: %s", h.stderr.String())
	}
	if register.TokenExists(h.tokenFile) {
		t.Fatal("file del token creato nonostante il rifiuto")
	}
}

// run: hello con la versione, uscita regolare al segnale; al secondo avvio connector.restarted
// (service-restart) arriva al backend; il primo avvio non ne produce.
func TestRunCollegaEDichiaraIRiavvii(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	id := hostIDFromStdout(t, h.stdout.String())

	done, stop := h.start(t)
	if hl := waitHello(t, h.s); hl.ConnectorVersion != version || hl.LastAckedSeq != 0 {
		t.Fatalf("hello %+v", hl)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(h.s.Events(id)); n != 0 {
		t.Fatalf("il primo avvio ha prodotto %d eventi", n)
	}
	stop()
	if code := waitExit(t, done); code != exitOK {
		t.Fatalf("uscita %d", code)
	}

	done, stop = h.start(t)
	waitHello(t, h.s)
	eventually(t, "connector.restarted consegnato", func() bool { return len(h.s.Events(id)) == 1 })
	ev := h.s.Events(id)[0]
	var d protocol.ConnectorRestarted
	_ = json.Unmarshal(ev.Data, &d)
	if ev.Kind != protocol.KindConnectorRestarted || d.Reason != "service-restart" || d.PreviousVersion != version || d.Version != version {
		t.Fatalf("evento %s %+v", ev.Kind, d)
	}
	stop()
	waitExit(t, done)
}

// Un avvio che trova il precedente ancora «in corsa» dichiara crash; dopo un riavvio della macchina,
// host-reboot.
func TestRunCrashERiavvioMacchina(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	id := hostIDFromStdout(t, h.stdout.String())

	done, stop := h.start(t)
	waitHello(t, h.s)
	stop()
	waitExit(t, done)
	// Simula un'uscita brusca: lo stato resta «in corsa».
	if err := writeRunState(h.stateDir, runState{StartedAt: protocol.Timestamp(time.Now()), Running: true}); err != nil {
		t.Fatal(err)
	}
	done, stop = h.start(t)
	eventually(t, "crash consegnato", func() bool { return len(h.s.Events(id)) == 1 })
	stop()
	waitExit(t, done)

	h.boot = time.Now().Add(time.Second) // la macchina è ripartita dopo l'ultimo avvio
	done, stop = h.start(t)
	eventually(t, "host-reboot consegnato", func() bool { return len(h.s.Events(id)) == 2 })
	stop()
	waitExit(t, done)
	reasons := []string{}
	for _, ev := range h.s.Events(id) {
		var d protocol.ConnectorRestarted
		_ = json.Unmarshal(ev.Data, &d)
		reasons = append(reasons, d.Reason)
	}
	if reasons[0] != "crash" || reasons[1] != "host-reboot" {
		t.Fatalf("motivi %v", reasons)
	}
}

// Revoca durante il collegamento: run termina con il codice 3 (non ritentare).
func TestRunRevocatoEsceConTre(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	id := hostIDFromStdout(t, h.stdout.String())
	done, _ := h.start(t)
	waitHello(t, h.s)
	h.s.Revoke(id)
	if code := waitExit(t, done); code != exitRevoked {
		t.Fatalf("uscita %d, attesa %d", code, exitRevoked)
	}
	if !strings.Contains(h.stderr.String(), "nuova registrazione") {
		t.Fatalf("stderr %s", h.stderr.String())
	}
}

func TestRunSenzaTokenDellHost(t *testing.T) {
	h := newHarness(t)
	if code := h.app().run(context.Background(), h.args("run")); code != exitError {
		t.Fatalf("uscita %d", code)
	}
}

func TestRunRifiutaServizioNonHttps(t *testing.T) {
	h := newHarness(t)
	a := h.app()
	code := a.run(context.Background(), []string{"run", "-server", "http://connect.example", "-token-file", h.tokenFile, "-state-dir", h.stateDir})
	if code != exitError || !strings.Contains(h.stderr.String(), "https") {
		t.Fatalf("uscita %d, stderr %s", code, h.stderr.String())
	}
}

func TestVersioneEUso(t *testing.T) {
	h := newHarness(t)
	if code := h.app().run(context.Background(), []string{"version"}); code != exitOK || strings.TrimSpace(h.stdout.String()) != version {
		t.Fatalf("version: %d %q", code, h.stdout.String())
	}
	for _, args := range [][]string{nil, {"sconosciuto"}, {"run", "-flag-inventato"}, {"run", "argomento-in-piu"}} {
		if code := h.app().run(context.Background(), args); code != exitUsage {
			t.Errorf("%v: uscita %d, attesa %d", args, code, exitUsage)
		}
	}
}

func TestMotivoDelRiavvio(t *testing.T) {
	cases := []struct {
		prev, cur         string
		rebooted, unclean bool
		want              string
	}{
		{"0.1.0", "0.2.0", true, true, "update"},
		{"0.2.0", "0.1.0", false, false, "rollback"},
		{"0.1.0-dev", "0.1.0", false, false, "update"},
		{"0.1.0", "0.1.0", true, true, "host-reboot"},
		{"0.1.0", "0.1.0", false, true, "crash"},
		{"0.1.0", "0.1.0", false, false, "service-restart"},
	}
	for _, c := range cases {
		if got := restartReason(c.prev, c.cur, c.rebooted, c.unclean); got != c.want {
			t.Errorf("%+v: %s", c, got)
		}
	}
	// Ogni motivo prodotto è nell'enum dello schema (protocol.RestartReasons è verificato contro lo schema).
	allowed := map[string]bool{}
	for _, r := range protocol.RestartReasons {
		allowed[r] = true
	}
	for _, c := range cases {
		if !allowed[c.want] {
			t.Errorf("motivo %s fuori dall'enum", c.want)
		}
	}
}

func TestConfrontoSemver(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0}, {"1.0.1", "1.0.0", 1}, {"1.10.0", "1.9.0", 1},
		{"1.0.0-alpha", "1.0.0", -1}, {"1.0.0-alpha.1", "1.0.0-alpha", 1},
		{"1.0.0-alpha.2", "1.0.0-alpha.10", -1}, {"1.0.0-1", "1.0.0-alpha", -1},
		{"1.0.0+build", "1.0.0", 0},
	} {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, atteso %d", c.a, c.b, got, c.want)
		}
	}
}

func TestBootTimeDiLinux(t *testing.T) {
	bt, err := linuxBootTime()
	if err != nil {
		t.Skip("/proc/stat non disponibile:", err)
	}
	if bt.After(time.Now()) || bt.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("btime implausibile: %v", bt)
	}
}

// Riproduce la prova dal vivo del 15/10: dopo la revoca si rimuove il file del token e si registra
// un host nuovo. Il giornale deve ripartire da zero: senza l'azzeramento il primo hello dichiara
// confermato l'evento 1 del vecchio host, il backend risponde ACK_AHEAD e il nuovo host riceve un
// journal.truncated per un evento che non ha mai avuto. Lo stesso vale per register -replace.
func TestRegistrazioneDiUnHostNuovoAzzeraIlGiornale(t *testing.T) {
	for _, viaReplace := range []bool{false, true} {
		name := "dopo la revoca"
		if viaReplace {
			name = "con -replace"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.s.AddInstallToken(installTok, time.Hour)
			h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
			h.register(t)
			oldID := hostIDFromStdout(t, h.stdout.String())
			// Due avvii: il secondo produce connector.restarted, confermato dal backend (seq 1).
			for i := 0; i < 2; i++ {
				done, stop := h.start(t)
				waitHello(t, h.s)
				if i == 1 {
					eventually(t, "evento 1 confermato", func() bool { return h.s.Bookmark(oldID) == 1 })
					time.Sleep(100 * time.Millisecond) // l'ack arriva al giornale
				}
				stop()
				waitExit(t, done)
			}

			h.s.AddInstallToken(installTok+"b", time.Hour)
			h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok + "b"
			h.stdout.mu.Lock()
			h.stdout.b.Reset()
			h.stdout.mu.Unlock()
			if viaReplace {
				if code := h.register(t, "-replace"); code != exitOK {
					t.Fatalf("register -replace: %d %s", code, h.stderr.String())
				}
			} else {
				h.s.Revoke(oldID)
				if err := os.Remove(h.tokenFile); err != nil {
					t.Fatal(err)
				}
				if code := h.register(t); code != exitOK {
					t.Fatalf("register: %d %s", code, h.stderr.String())
				}
			}
			newID := hostIDFromStdout(t, h.stdout.String())

			done, stop := h.start(t)
			if hl := waitHello(t, h.s); hl.LastAckedSeq != 0 {
				t.Fatalf("primo hello del nuovo host: lastAckedSeq %d, atteso 0", hl.LastAckedSeq)
			}
			eventually(t, "connector.restarted del nuovo host", func() bool { return len(h.s.Events(newID)) == 1 })
			ev := h.s.Events(newID)[0]
			if ev.Seq != 1 || ev.Kind != protocol.KindConnectorRestarted {
				t.Fatalf("primo evento del nuovo host: seq %d %s, atteso 1 connector.restarted", ev.Seq, ev.Kind)
			}
			stop()
			waitExit(t, done)
		})
	}
}

// minBackendVersion è MIN_CONNECTOR_VERSION di apps/backend/src/modules/terminal/connector/
// connector-hosts.service.ts (monorepo MonsterApp): la minima accettata alla registrazione.
const minBackendVersion = "0.1.0"

// backendCompareSemver riproduce compareSemver del backend (stesso file): il nucleo numerico, poi
// «con pre-release» < «senza», senza guardare il contenuto della pre-release.
func backendCompareSemver(a, b string) int {
	parse := func(v string) ([3]int, bool) {
		core, _, pre := strings.Cut(v, "-")
		var n [3]int
		for i, p := range strings.SplitN(core, ".", 3) {
			n[i], _ = strconv.Atoi(p)
		}
		return n, pre
	}
	na, pa := parse(a)
	nb, pb := parse(b)
	for i := 0; i < 3; i++ {
		if na[i] != nb[i] {
			return na[i] - nb[i]
		}
	}
	switch {
	case pa == pb:
		return 0
	case pa:
		return -1
	}
	return 1
}

// §1.15 e §1.20: una compilazione di sviluppo deve poter registrarsi sul backend vero. Il 15/10
// «0.1.0-dev» ha ricevuto 426 CONNECTOR_TOO_OLD.
func TestVersioneDiSviluppoAccettataDalBackend(t *testing.T) {
	if !protocol.IsSemver(devVersion) {
		t.Fatalf("%s non è una semver dello schema common.json", devVersion)
	}
	if backendCompareSemver(devVersion, minBackendVersion) < 0 {
		t.Fatalf("%s è sotto la minima del backend %s: 426 CONNECTOR_TOO_OLD", devVersion, minBackendVersion)
	}
	if compareSemver(devVersion, minBackendVersion) < 0 {
		t.Fatalf("%s precede %s anche in semver", devVersion, minBackendVersion)
	}
	// La controprova del motivo: la forma di prima è rifiutata.
	if backendCompareSemver("0.1.0-dev", minBackendVersion) >= 0 {
		t.Fatal("il confronto del backend riprodotto male: 0.1.0-dev dovrebbe essere sotto 0.1.0")
	}
}

// Il server finto rifiuta come il backend: una versione sotto la minima riceve 426 e il messaggio
// dice di aggiornare.
func TestRegisterVersioneTroppoVecchia(t *testing.T) {
	h := newHarness(t)
	h.s.MinConnectorVersion = minBackendVersion
	h.s.CompareVersion = backendCompareSemver
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	old := version
	version = "0.1.0-dev"
	defer func() { version = old }()
	if code := h.register(t); code != exitError || !strings.Contains(h.stderr.String(), "CONNECTOR_TOO_OLD") || !strings.Contains(h.stderr.String(), "aggiornarlo") {
		t.Fatalf("uscita %d, stderr %s", code, h.stderr.String())
	}
	if !h.s.InstallTokenUsable(installTok) {
		t.Fatal("426 non deve consumare il token")
	}
	version = devVersion
	if code := h.register(t); code != exitOK {
		t.Fatalf("versione di sviluppo rifiutata: %d %s", code, h.stderr.String())
	}
}

// La revoca si scrive nel log una volta sola (prima compariva due volte: dal trasporto e dal comando).
func TestRevocaUnSoloMessaggio(t *testing.T) {
	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	id := hostIDFromStdout(t, h.stdout.String())
	done, _ := h.start(t)
	waitHello(t, h.s)
	h.s.Revoke(id)
	if code := waitExit(t, done); code != exitRevoked {
		t.Fatalf("uscita %d", code)
	}
	if n := strings.Count(h.stderr.String(), "level=ERROR"); n != 1 {
		t.Fatalf("%d messaggi di errore alla revoca, atteso 1:\n%s", n, h.stderr.String())
	}
}

// Il comando run collega il terminale: un canale aperto dal backend arriva a tmux e l'output torna
// indietro (spec §1.7). Server tmux privato.
func TestRunCollegaIlTerminale(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux non installato")
	}
	dir, err := os.MkdirTemp("", "mt")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run(); _ = os.RemoveAll(dir) })

	h := newHarness(t)
	h.s.AddInstallToken(installTok, time.Hour)
	h.env["MONSTERTERMINAL_INSTALL_TOKEN"] = installTok
	h.register(t)
	id := hostIDFromStdout(t, h.stdout.String())
	a := h.app()
	a.tmuxSocket = sock
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- a.run(ctx, h.args("run")) }()
	defer func() { cancel(); waitExit(t, done) }()
	waitHello(t, h.s)

	mid, err := h.s.SendControl(id, protocol.TypeSessionOpen, protocol.SessionOpen{Channel: 1, Session: "prova", Create: true, Mode: protocol.ModeInteract, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "session.opened", func() bool { env, ok := h.s.Reply(id, mid); return ok && env.Type == protocol.TypeSessionOpened })
	_ = h.s.SendInput(id, 1, []byte("echo cablato-$((20+3))\r"))
	eventually(t, "output dal tmux dell'host", func() bool { return strings.Contains(string(h.s.Output(id, 1)), "cablato-23") })
}
