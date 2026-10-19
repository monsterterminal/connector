package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/monsterterminal/connector/internal/agents"
	"github.com/monsterterminal/connector/internal/hostinfo"
	"github.com/monsterterminal/connector/internal/journal"
	"github.com/monsterterminal/connector/internal/protocol"
	"github.com/monsterterminal/connector/internal/register"
	"github.com/monsterterminal/connector/internal/terminal"
	"github.com/monsterterminal/connector/internal/transport"
)

const (
	defaultServer    = "https://connect.monsterterminal.com"
	defaultTokenFile = "/etc/monsterterminal/host-token"
	defaultStateDir  = "/var/lib/monsterterminal"
)

// Codici di uscita (vedi il commento del package).
const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitRevoked = 3
)

// app raccoglie le dipendenze del comando, sostituibili nei test.
type app struct {
	getenv     func(string) string
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	httpClient *http.Client // nil = client di produzione con la verifica TLS di sistema
	collect    func() (hostinfo.Info, error)
	bootTime   func() (time.Time, error)
	now        func() time.Time
	// configure ritocca la configurazione del trasporto (solo test: attese più corte).
	configure func(*transport.Config)
	// tmuxSocket, se non vuoto, è il socket del server tmux (solo test: un server privato).
	tmuxSocket string
	// home e executable: cartella dell'utente e percorso del binario (per gli hook di Claude Code).
	home       func() (string, error)
	executable func() (string, error)
	// paneSession ricava la sessione tmux di un pannello (comando hook).
	paneSession func(pane string) (string, error)
	// sessionProbe legge se un client è collegato e attivo sulla sessione (finestra adattiva, §2.12);
	// nil = tmux sul socket tmuxSocket.
	sessionProbe agents.SessionProbe
}

func defaultApp() app {
	return app{
		getenv: os.Getenv, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		collect: hostinfo.Collect, bootTime: linuxBootTime, now: time.Now,
		home: os.UserHomeDir, executable: os.Executable, paneSession: tmuxPaneSession,
	}
}

func (a app) run(ctx context.Context, args []string) int {
	log := slog.New(slog.NewTextHandler(a.stderr, nil))
	if len(args) < 1 {
		a.usage()
		return exitUsage
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	server := fs.String("server", defaultServer, "origine https del servizio")
	tokenFile := fs.String("token-file", defaultTokenFile, "file del token dell'host")
	stateDir := fs.String("state-dir", defaultStateDir, "cartella del giornale degli eventi")
	replace := fs.Bool("replace", false, "register: sostituisce l'host registrato su questa macchina (serve il suo token attuale)")
	yes := fs.Bool("yes", false, "hooks install: risponde sì alla domanda di consenso senza terminale")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	positional := 0
	if cmd == "hook" || cmd == "hooks" {
		positional = 1 // l'evento di Claude Code, oppure install|remove
		// Il parser dei flag si ferma al primo argomento posizionale: `hooks install -yes` perderebbe
		// -yes. I flag dopo l'azione si rileggono.
		if fs.NArg() > 1 {
			action := fs.Arg(0)
			if err := fs.Parse(fs.Args()[1:]); err != nil {
				return exitUsage
			}
			if fs.NArg() > 0 {
				fmt.Fprintf(a.stderr, "argomenti non validi: %q\n", fs.Args())
				return exitUsage
			}
			_ = fs.Parse([]string{action})
		}
	}
	if cmd == "hook" {
		// Chiamato da Claude Code: non deve mai bloccarlo né sporcare la sua uscita (spec §1.21).
		if fs.NArg() == 1 {
			a.cmdHook(ctx, fs.Arg(0), *stateDir)
		}
		return exitOK
	}
	if fs.NArg() != positional {
		fmt.Fprintf(a.stderr, "argomenti non validi: %q\n", fs.Args())
		return exitUsage
	}

	var err error
	switch cmd {
	case "register":
		err = a.cmdRegister(ctx, *server, *tokenFile, *stateDir, *replace)
	case "run":
		err = a.cmdRun(ctx, log, *server, *tokenFile, *stateDir)
	case "version":
		fmt.Fprintln(a.stdout, version)
	case "hooks":
		err = a.cmdHooks(fs.Arg(0), *stateDir, *yes)
	default:
		a.usage()
		return exitUsage
	}
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return exitOK
	case errors.Is(err, transport.ErrRevoked), errors.Is(err, transport.ErrTokenInvalid):
		log.Error("collegamento terminato: serve una nuova registrazione", "motivo", err.Error())
		return exitRevoked
	default:
		log.Error("terminato con errore", "errore", err.Error())
		return exitError
	}
}

func (a app) usage() {
	fmt.Fprintln(a.stderr, "uso: monsterterminal-connector register [-replace]|run|version|hooks install [-yes]|hooks remove [-server URL] [-token-file PERCORSO] [-state-dir CARTELLA]")
}

func (a app) client(timeout time.Duration) *http.Client {
	if a.httpClient != nil {
		return a.httpClient
	}
	return &http.Client{Timeout: timeout}
}

func (a app) readInstallToken() (string, error) {
	if t := a.getenv("MONSTERTERMINAL_INSTALL_TOKEN"); t != "" {
		return t, nil
	}
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("token di installazione mancante: impostare MONSTERTERMINAL_INSTALL_TOKEN o passarlo sullo standard input")
	}
	return strings.TrimSpace(line), nil
}

func (a app) cmdRegister(ctx context.Context, server, tokenFile, stateDir string, replace bool) error {
	current := ""
	switch exists := register.TokenExists(tokenFile); {
	case exists && !replace:
		return fmt.Errorf("l'host risulta già registrato (%s esiste): per sostituirlo usare register -replace", tokenFile)
	case !exists && replace:
		return fmt.Errorf("-replace: nessun host da sostituire (%s non esiste)", tokenFile)
	case replace:
		// Prova di possesso (§1.3): il token attuale va al backend, che revoca il vecchio host.
		t, err := register.LoadToken(tokenFile)
		if err != nil {
			return err
		}
		current = t
	}
	tok, err := a.readInstallToken()
	if err != nil {
		return err
	}
	hi, err := a.collect()
	if err != nil {
		return err
	}
	c := register.Client{BaseURL: server, HTTPClient: a.client(30 * time.Second)}
	resp, err := c.Replace(ctx, tok, current, register.Info{
		MachineID: hi.MachineID, Hostname: hi.Hostname, OS: "linux", Distro: hi.Distro,
		DistroVersion: hi.DistroVersion, Arch: hi.Arch, ConnectorVersion: version,
		RunAsUser: hi.User, RunAsRoot: hi.Root,
	})
	if err != nil {
		return err
	}
	if err := register.SaveToken(tokenFile, resp.HostToken); err != nil {
		return err
	}
	// Host nuovo, giornale nuovo: il segnalibro del backend per lui è 0. Prima il token, poi il
	// giornale: con l'ordine inverso un errore sul token lascerebbe il VECCHIO host con un giornale
	// ripartito da 1, i cui eventi il backend scarterebbe come già ricevuti.
	j, err := journal.Open(filepath.Join(stateDir, "journal"), journal.Options{Now: a.now})
	if err != nil {
		return fmt.Errorf("host registrato, ma il giornale degli eventi non si apre (%w): svuotare %s prima di run", err, stateDir)
	}
	lost, err := j.Reset()
	if err != nil {
		return fmt.Errorf("host registrato, ma il giornale degli eventi non si azzera (%w): svuotare %s prima di run", err, stateDir)
	}
	if lost > 0 {
		fmt.Fprintf(a.stderr, "Giornale azzerato per il nuovo host: %d eventi del precedente non consegnati.\n", lost)
	}
	if current != "" {
		fmt.Fprintf(a.stdout, "Host sostituito per %s (nuovo id %s); il precedente è revocato.\n", resp.TenantLabel, resp.HostID)
	} else {
		fmt.Fprintf(a.stdout, "Host registrato per %s (id %s).\n", resp.TenantLabel, resp.HostID)
	}
	return nil
}

func (a app) cmdRun(ctx context.Context, log *slog.Logger, server, tokenFile, stateDir string) error {
	if !strings.HasPrefix(server, "https://") {
		return errors.New("il servizio deve essere https")
	}
	tok, err := register.LoadToken(tokenFile)
	if err != nil {
		return err
	}
	hi, err := a.collect()
	if err != nil {
		return err
	}
	lock, err := lockStateDir(stateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	j, err := journal.Open(filepath.Join(stateDir, "journal"), journal.Options{Now: a.now})
	if err != nil {
		return fmt.Errorf("giornale degli eventi: %w", err)
	}
	if err := a.recordStart(log, j, stateDir); err != nil {
		return err
	}
	// Stati degli agenti (spec §1.21): socket degli hook di Claude Code, timer di inattività e
	// richieste di permesso (§2.12).
	thresholds, requests := a.startAgents(ctx, log, j, stateDir)
	cfg := transport.Config{
		URL:        "wss://" + strings.TrimPrefix(server, "https://") + "/v1",
		Token:      tok,
		HTTPClient: a.client(0), // il WebSocket resta aperto: niente timeout complessivo
		Logger:     log,
		Events:     j,
		Terminal:   terminal.New(terminal.Config{Socket: a.tmuxSocket, Logger: log}),
		Requests:   requests,
		Now:        a.now,
		Hello: func() protocol.Hello {
			return protocol.Hello{
				ConnectorVersion: version, Protocol: protocol.Version, OS: "linux", Arch: hi.Arch,
				RunAsUser: hi.User, RunAsRoot: hi.Root, Capabilities: a.capabilities(),
				Agents: []protocol.Agent{},
			}
		},
	}
	// Le soglie di inattività vengono dalla configurazione dell'host (§1.11).
	cfg.OnWelcome = func(w protocol.Welcome) { thresholds.Set(w.Config) }
	cfg.OnConfig = func(c protocol.HostConfig) { thresholds.Set(c) }
	if a.configure != nil {
		a.configure(&cfg)
	}
	err = transport.Run(ctx, cfg)
	// Uscita regolare (segnale): la prossima partenza non è un crash.
	if errors.Is(err, context.Canceled) || errors.Is(err, transport.ErrRevoked) || errors.Is(err, transport.ErrTokenInvalid) {
		if merr := markStopped(stateDir); merr != nil {
			log.Warn("stato di arresto non scritto", "errore", merr.Error())
		}
	}
	return err
}

// runState è il segno lasciato da ogni avvio: quando è partito e se è ancora in corsa. Un avvio
// che trova running=true sa che il precedente non è uscito in modo regolare.
type runState struct {
	StartedAt string `json:"startedAt"`
	Running   bool   `json:"running"`
}

func runStatePath(stateDir string) string { return filepath.Join(stateDir, "run.json") }

// recordStart scrive connector.restarted (§1.8) se c'è stato un avvio precedente, e segna questo.
func (a app) recordStart(log *slog.Logger, j *journal.Journal, stateDir string) error {
	now := a.now()
	var prev runState
	hadPrev := false
	if raw, err := os.ReadFile(runStatePath(stateDir)); err == nil {
		hadPrev = json.Unmarshal(raw, &prev) == nil
	}
	prevVersion := j.LastVersion()
	if prevVersion != "" && protocol.IsSemver(prevVersion) && protocol.IsSemver(version) {
		rebooted := false
		if hadPrev && a.bootTime != nil {
			if boot, err := a.bootTime(); err == nil {
				if started, err := time.Parse(time.RFC3339Nano, prev.StartedAt); err == nil {
					rebooted = boot.After(started)
				}
			}
		}
		reason := restartReason(prevVersion, version, rebooted, hadPrev && prev.Running)
		if _, err := j.Append(protocol.KindConnectorRestarted, protocol.ConnectorRestarted{
			PreviousVersion: prevVersion, Version: version, Reason: reason,
		}); err != nil {
			return fmt.Errorf("connector.restarted: %w", err)
		}
		log.Info("riavvio registrato", "motivo", reason, "versionePrecedente", prevVersion)
	}
	if err := j.SetLastVersion(version); err != nil {
		return err
	}
	return writeRunState(stateDir, runState{StartedAt: protocol.Timestamp(now), Running: true})
}

func markStopped(stateDir string) error {
	var st runState
	if raw, err := os.ReadFile(runStatePath(stateDir)); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	st.Running = false
	return writeRunState(stateDir, st)
}

func writeRunState(stateDir string, st runState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := runStatePath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, runStatePath(stateDir))
}

// restartReason sceglie il motivo di connector.restarted (enum dello schema 0.1.0): il cambio di
// versione prima di tutto, poi il riavvio della macchina, poi l'uscita irregolare.
func restartReason(prevVersion, curVersion string, hostRebooted, unclean bool) string {
	switch c := compareSemver(curVersion, prevVersion); {
	case c > 0:
		return "update"
	case c < 0:
		return "rollback"
	}
	switch {
	case hostRebooted:
		return "host-reboot"
	case unclean:
		return "crash"
	default:
		return "service-restart"
	}
}

// compareSemver confronta due versioni semver 2.0 (senza i metadati di build): -1, 0, 1.
func compareSemver(a, b string) int {
	a, _, _ = strings.Cut(a, "+")
	b, _, _ = strings.Cut(b, "+")
	ac, apre, _ := strings.Cut(a, "-")
	bc, bpre, _ := strings.Cut(b, "-")
	ap, bp := strings.Split(ac, "."), strings.Split(bc, ".")
	for i := 0; i < 3 && i < len(ap) && i < len(bp); i++ {
		x, _ := strconv.Atoi(ap[i])
		y, _ := strconv.Atoi(bp[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "": // una release precede le sue pre-release
		return 1
	case bpre == "":
		return -1
	}
	ai, bi := strings.Split(apre, "."), strings.Split(bpre, ".")
	for i := 0; i < len(ai) && i < len(bi); i++ {
		if ai[i] == bi[i] {
			continue
		}
		x, xerr := strconv.Atoi(ai[i])
		y, yerr := strconv.Atoi(bi[i])
		switch {
		case xerr == nil && yerr == nil:
			if x < y {
				return -1
			}
			return 1
		case xerr == nil: // numerico < alfanumerico
			return -1
		case yerr == nil:
			return 1
		case ai[i] < bi[i]:
			return -1
		default:
			return 1
		}
	}
	switch {
	case len(ai) < len(bi):
		return -1
	case len(ai) > len(bi):
		return 1
	}
	return 0
}

// linuxBootTime legge l'istante di avvio della macchina da /proc/stat (btime).
func linuxBootTime() (time.Time, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(n, 0), nil
		}
	}
	return time.Time{}, errors.New("btime assente in /proc/stat")
}
