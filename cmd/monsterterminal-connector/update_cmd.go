package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/monsterterminal/connector/internal/update"
)

// I due sottocomandi dell'aggiornamento automatico (ADR 0438 D3 punti 2-4), eseguiti come ROOT dalle
// unità systemd scritte da install.sh, mai dal connettore:
//
//   - apply-update: dall'unità monsterterminal-connector-updater.service, quando il connettore ha
//     lasciato apply.request in <state>/update/. Gira col binario INSTALLATO, cioè quello vecchio e
//     noto buono: `version` qui è la versione da superare. Non si fida di nulla di ciò che il
//     connettore (utente non privilegiato) ha preparato: copia, riverifica, poi installa.
//   - rollback-check: dal timer monsterterminal-connector-rollback.timer, col binario .prev: se la
//     versione nuova non ha scritto il marcatore di collegamento, la toglie di mezzo.
//
// Entrambi vogliono solo -state-dir: niente token, niente rete.

// updateClient è il client HTTP dei download: verifica TLS di sistema, nessun timeout complessivo
// (ce l'ha il download stesso), niente redirect fuori da https.
func (a app) updateClient() *http.Client {
	if a.updateHTTP != nil {
		return a.updateHTTP
	}
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("troppi redirect")
			}
			if req.URL.Scheme != "https" {
				return errors.New("redirect fuori da https")
			}
			return nil
		},
	}
}

func (a app) applyConfig(log *slog.Logger, stateDir string) (update.ApplyConfig, error) {
	bin, err := a.executable()
	if err != nil {
		return update.ApplyConfig{}, fmt.Errorf("percorso del binario: %w", err)
	}
	// Il binario .prev esegue rollback-check: il binario installato è quello senza suffisso.
	bin = strings.TrimSuffix(bin, update.PrevSuffix)
	uid := -1
	if a.getuid != nil {
		uid = a.getuid()
	}
	return update.ApplyConfig{
		StateDir: stateDir, Binary: bin, Running: version, Key: a.updateKey,
		Systemctl: a.systemctl, UID: uid, Logger: log, Now: a.now,
	}, nil
}

func (a app) cmdApplyUpdate(log *slog.Logger, stateDir string) error {
	cfg, err := a.applyConfig(log, stateDir)
	if err != nil {
		return err
	}
	log.Info("apply-update", "binarioInstallato", cfg.Binary, "versioneInEsecuzione", version, "stato", stateDir)
	started := time.Now()
	if err := update.Apply(cfg); err != nil {
		var rej *update.Rejection
		if errors.As(err, &rej) {
			// Il rifiuto è già in apply.rejected: il connettore lo riporta come update.result failed.
			return fmt.Errorf("aggiornamento rifiutato (%s): %w", rej.Reason, rej.Err)
		}
		return err
	}
	log.Info("apply-update completato", "durata", time.Since(started).String())
	return nil
}

func (a app) cmdRollbackCheck(log *slog.Logger, stateDir string) error {
	cfg, err := a.applyConfig(log, stateDir)
	if err != nil {
		return err
	}
	action, err := update.RollbackCheck(cfg)
	log.Info("rollback-check", "azione", action.String(), "binario", cfg.Binary)
	if err != nil {
		return err
	}
	if action == update.RollbackRestore {
		fmt.Fprintln(a.stderr, "versione precedente ripristinata: il connettore riporterà update.result rolled-back")
	}
	return nil
}
