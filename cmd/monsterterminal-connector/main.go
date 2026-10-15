// Comando monsterterminal-connector: registra l'host e mantiene il collegamento in uscita.
//
//	monsterterminal-connector register [-replace]   legge il token di installazione da
//	                                                MONSTERTERMINAL_INSTALL_TOKEN o dalla prima
//	                                                riga dello standard input
//	monsterterminal-connector run                   si collega e resta collegato
//	monsterterminal-connector version
//
// Il token di installazione non si passa mai come argomento: comparirebbe nell'elenco dei processi.
//
// Codici di uscita: 0 fine regolare, 1 errore, 2 uso sbagliato, 3 host revocato o token dell'host
// non valido (ricollegarsi non serve: occorre una nuova registrazione; il servizio non va riavviato).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// devVersion è la versione delle compilazioni senza -ldflags: una pre-release della patch che segue
// la minima accettata dal backend (MIN_CONNECTOR_VERSION = 0.1.0, §1.15). «0.1.0-dev» sarebbe la
// forma naturale ma in semver precede 0.1.0, e il backend la rifiuta con 426 CONNECTOR_TOO_OLD
// (prova dal vivo del 15/10); «0.1.0+dev» non passa lo schema common.json#semver, che non ammette
// metadati di build. Quando il backend alza la minima, devVersion la segue (lo verifica un test).
const devVersion = "0.1.1-dev"

// version si imposta in compilazione con -ldflags "-X main.version=…".
var version = devVersion

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := defaultApp().run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
