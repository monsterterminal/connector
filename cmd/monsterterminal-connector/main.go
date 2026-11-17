// Comando monsterterminal-connector: registra l'host e mantiene il collegamento in uscita.
//
//	monsterterminal-connector register [-replace]   legge il token di installazione da
//	                                                MONSTERTERMINAL_INSTALL_TOKEN o dalla prima
//	                                                riga dello standard input
//	monsterterminal-connector register -check      verifica, senza consumare il token, se l'host
//	                                                registrato qui è dello stesso spazio di lavoro
//	monsterterminal-connector run [-config FILE]    si collega e resta collegato; legge le impostazioni
//	                                                del cliente da /etc/monsterterminal/connector.conf
//	monsterterminal-connector version
//	monsterterminal-connector apply-update -state-dir DIR   (root, dall'unità updater: installa
//	                                                ciò che il connettore ha preparato, dopo
//	                                                averlo riverificato da sé; ADR 0438 D3)
//	monsterterminal-connector rollback-check -state-dir DIR (root, dal timer, col binario .prev:
//	                                                ripristina la versione precedente se la nuova
//	                                                non si è collegata)
//
// Il token di installazione non si passa mai come argomento: comparirebbe nell'elenco dei processi.
//
// Codici di uscita: 0 fine regolare, 1 errore, 2 uso sbagliato, 3 host revocato o token dell'host
// non valido (ricollegarsi non serve: occorre una nuova registrazione; il servizio non va riavviato),
// 4 (register -check) host registrato per un altro spazio di lavoro, 5 (register -check) verifica non
// disponibile sul servizio.
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

// bancoBrokenHello si imposta SOLO nei banchi di prova con -ldflags "-X main.bancoBrokenHello=1":
// run esce prima di collegarsi, così il timer di ritorno (ADR 0438 D3 punto 4) trova un binario che
// non arriva mai a welcome. Un rilascio vero lo lascia vuoto: release.yml non lo imposta.
var bancoBrokenHello = ""

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := defaultApp().run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
