package catalog

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/monsterterminal/connector/internal/protocol"
)

// Subdir è la cartella del catalogo dentro la cartella di stato: file di stato per ricetta
// (<recipeId>.json) e cartelle di lavoro (work/<installId>/).
const Subdir = "catalog"

// Out è la connessione vista dall'installatore: messaggi di controllo verso il backend.
type Out interface {
	Control(typ, re string, body any) error
}

// Appender scrive un evento nel giornale dell'host.
type Appender interface {
	Append(kind string, data any) (protocol.Event, error)
}

// Config configura l'installatore.
type Config struct {
	// StateDir è la cartella di stato del connettore.
	StateDir string
	// Home è la cartella dell'utente del connettore.
	Home string
	// CatalogInstall è catalog_install del file dell'host (§1.11).
	CatalogInstall bool
	// Key è la chiave delle ricette; nil = mt-recipes-2026 incorporata (solo i test la cambiano).
	Key ed25519.PublicKey
	// Platform e Root dell'host.
	Platform HostPlatform
	Root     bool
	// Journal riceve catalog.installed a esito ok.
	Journal Appender
	// Detect rileva gli agenti installati (rifatto dopo un'installazione ok); nil = nessuno.
	Detect func() []protocol.Agent
	// LookupVersion cerca un comando e ne legge la versione, per requires.
	LookupVersion func(name string) (string, bool)
	// Env calcola l'ambiente dei passi run a ogni installazione (così vede le cartelle aggiunte al PATH
	// da quelle precedenti); nil = quello del processo.
	Env func() []string
	// HTTPClient dei passi fetch; nil = DefaultHTTPClient.
	HTTPClient *http.Client
	Logger     *slog.Logger
	Now        func() time.Time
	// Solo test: tetto di scarico e unità di timeoutSeconds.
	MaxFetch    int64
	TimeoutUnit time.Duration
}

// Installer riceve catalog.install, verifica, esegue e risponde. Un'installazione alla volta per host.
type Installer struct {
	cfg Config

	mu      sync.Mutex
	out     Out
	busy    bool
	pending []pendingResult // esiti non consegnati: si rimandano al prossimo collegamento
	wg      sync.WaitGroup
}

type pendingResult struct {
	re     string
	result protocol.CatalogInstallResult
}

// New crea l'installatore.
func New(cfg Config) *Installer {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Installer{cfg: cfg}
}

// Attach si chiama dopo welcome: rimanda gli esiti rimasti senza consegna.
func (in *Installer) Attach(out Out) {
	in.mu.Lock()
	in.out = out
	pending := in.pending
	in.pending = nil
	in.mu.Unlock()
	for _, p := range pending {
		in.sendResult(p.re, p.result)
	}
}

// Detach si chiama alla caduta: un'installazione in corso continua e il suo esito aspetta il
// prossimo collegamento.
func (in *Installer) Detach() {
	in.mu.Lock()
	in.out = nil
	in.mu.Unlock()
}

// Wait attende la fine delle installazioni in corso (test e spegnimento).
func (in *Installer) Wait() { in.wg.Wait() }

// Handle tratta catalog.install; gli altri tipi non sono suoi. La verifica e i passi girano fuori dal
// ciclo di lettura: un'installazione dura minuti e non deve fermare il terminale.
func (in *Installer) Handle(env protocol.Envelope) bool {
	if env.Type != protocol.TypeCatalogInstall {
		return false
	}
	ci, err := protocol.DecodeCatalogInstall(env.Body)
	if err != nil {
		msg := err.Error()
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		_ = in.send(protocol.TypeError, env.ID, protocol.ErrorBody{Code: protocol.CodeMessageInvalid, Message: msg})
		return true
	}
	in.wg.Add(1)
	go func() {
		defer in.wg.Done()
		in.Install(context.Background(), env.ID, ci)
	}()
	return true
}

func (in *Installer) send(typ, re string, body any) error {
	in.mu.Lock()
	out := in.out
	in.mu.Unlock()
	if out == nil {
		return errors.New("non collegato")
	}
	return out.Control(typ, re, body)
}

func (in *Installer) sendResult(re string, r protocol.CatalogInstallResult) {
	if err := in.send(protocol.TypeCatalogInstallResult, re, r); err != nil {
		in.cfg.Logger.Warn("catalogo: esito non consegnato, lo rimando al prossimo collegamento", "installazione", r.InstallID, "errore", err.Error())
		in.mu.Lock()
		in.pending = append(in.pending, pendingResult{re: re, result: r})
		in.mu.Unlock()
	}
}

func (in *Installer) acquire() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.busy {
		return false
	}
	in.busy = true
	return true
}

func (in *Installer) release() {
	in.mu.Lock()
	in.busy = false
	in.mu.Unlock()
}

func (in *Installer) catalogDir() string { return filepath.Join(in.cfg.StateDir, Subdir) }

// StatePath è il file di stato di una ricetta: state-dir/catalog/<recipeId>.json.
func StatePath(stateDir, recipeID string) string {
	return filepath.Join(stateDir, Subdir, recipeID+".json")
}

// State è il contenuto del file di stato di una ricetta: l'ultima versione installata sull'host.
type State struct {
	RecipeID      string `json:"recipeId"`
	RecipeVersion int    `json:"recipeVersion"`
	Agent         string `json:"agent"`
	InstallID     string `json:"installId"`
	InstalledAt   string `json:"installedAt"`
}

// ReadState legge il file di stato. ok=false se la ricetta non è mai stata installata qui.
func ReadState(stateDir, recipeID string) (State, bool, error) {
	raw, err := os.ReadFile(StatePath(stateDir, recipeID))
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return State{}, false, err
	}
	if s.RecipeVersion < 1 {
		return State{}, false, fmt.Errorf("recipeVersion %d nel file di stato", s.RecipeVersion)
	}
	return s, true, nil
}

func writeState(stateDir string, s State) error {
	if err := os.MkdirAll(filepath.Join(stateDir, Subdir), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	path := StatePath(stateDir, s.RecipeID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Install esegue un'installazione intera, in modo sincrono: verifiche, passi, file di stato, evento
// e rilevamento. Manda progresso ed esito (con re = id del catalog.install) e restituisce l'esito.
func (in *Installer) Install(ctx context.Context, re string, ci protocol.CatalogInstall) protocol.CatalogInstallResult {
	log := in.cfg.Logger.With("installazione", ci.InstallID)
	env := Env{
		CatalogInstall: in.cfg.CatalogInstall, Key: in.cfg.Key, Now: in.cfg.Now,
		Platform: in.cfg.Platform, Root: in.cfg.Root, LookupVersion: in.cfg.LookupVersion,
		Installed: func(id string) (int, bool, error) {
			s, ok, err := ReadState(in.cfg.StateDir, id)
			return s.RecipeVersion, ok, err
		},
		Acquire: in.acquire,
	}
	r, refusal := Check(env, ci.Recipe, ci.Signature)
	if refusal != nil {
		log.Warn("catalogo: ricetta rifiutata", "ricetta", r.RecipeID, "motivo", refusal.Reason, "dettaglio", refusal.Detail)
		res := protocol.CatalogInstallResult{InstallID: ci.InstallID, Outcome: protocol.CatalogRefused, Reason: refusal.Reason, Detail: refusal.Detail}
		in.sendResult(re, res)
		return res
	}
	defer in.release()
	// approvedBy e approvedAt sono dati del backend, fuori dalla firma: restano nel log dell'host.
	log.Info("catalogo: installazione approvata, eseguo", "ricetta", r.RecipeID, "versione", r.RecipeVersion,
		"agente", r.Agent, "canale", r.Source.Channel, "origine", r.Source.Ref, "approvataDa", ci.ApprovedBy, "approvataIl", ci.ApprovedAt)
	var stepEnv []string
	if in.cfg.Env != nil {
		stepEnv = in.cfg.Env()
	}
	failure := Run(ctx, r, RunConfig{
		WorkDir: filepath.Join(in.catalogDir(), "work", ci.InstallID), Home: in.cfg.Home, Env: stepEnv,
		HTTPClient: in.cfg.HTTPClient, MaxFetch: in.cfg.MaxFetch, TimeoutUnit: in.cfg.TimeoutUnit,
		Progress: func(step, of int, message string) {
			_ = in.send(protocol.TypeCatalogInstallProgress, re, protocol.CatalogInstallProgress{InstallID: ci.InstallID, Step: step, Of: of, Message: message})
		},
	})
	if failure != nil {
		log.Warn("catalogo: installazione non riuscita", "ricetta", r.RecipeID, "passo", failure.StepIndex, "motivo", failure.Reason, "dettaglio", failure.Detail)
		idx := failure.StepIndex
		res := protocol.CatalogInstallResult{InstallID: ci.InstallID, Outcome: protocol.CatalogFailed, Reason: failure.Reason,
			Detail: failure.Detail, StepIndex: &idx, LogTail: failure.LogTail}
		in.sendResult(re, res)
		return res
	}
	if err := writeState(in.cfg.StateDir, State{RecipeID: r.RecipeID, RecipeVersion: r.RecipeVersion, Agent: r.Agent,
		InstallID: ci.InstallID, InstalledAt: protocol.Timestamp(in.cfg.Now())}); err != nil {
		// I passi sono riusciti: l'esito resta ok, ma senza file di stato il downgrade non si vedrebbe.
		log.Error("catalogo: file di stato non scritto", "ricetta", r.RecipeID, "errore", err.Error())
	}
	version := ""
	if in.cfg.Detect != nil {
		for _, a := range in.cfg.Detect() {
			if a.Agent == r.Agent {
				version = a.Version
			}
		}
	}
	if in.cfg.Journal != nil {
		if _, err := in.cfg.Journal.Append(protocol.KindCatalogInstalled, protocol.CatalogInstalled{
			RecipeID: r.RecipeID, RecipeVersion: r.RecipeVersion, Agent: r.Agent, Version: version,
		}); err != nil {
			log.Error("catalogo: evento catalog.installed non scritto", "errore", err.Error())
		}
	}
	log.Info("catalogo: installazione riuscita", "ricetta", r.RecipeID, "versione", r.RecipeVersion, "agente", r.Agent, "versioneAgente", version)
	res := protocol.CatalogInstallResult{InstallID: ci.InstallID, Outcome: protocol.CatalogOK}
	in.sendResult(re, res)
	return res
}
