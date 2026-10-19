# MonsterTerminal Connector

Il connettore di MonsterTerminal: un piccolo programma installato sull'host del cliente, che apre una
connessione in uscita verso MonsterTerminal, registra l'host e lascia lavorare tmux e gli agenti AI
anche quando il collegamento cade.

> Stato: **seconda fetta**, allineata al gateway vero (ADR 0409, verbale 2026-10-13). Registrazione e
> sostituzione dell'host, collegamento in uscita con heartbeat, ricollegamento, revoca e kill-switch,
> giornale degli eventi con conferme del backend, canali del terminale sulle sessioni tmux, stati degli
> agenti AI (Claude Code con hook, livello generico con timer di inattività), richieste di permesso con
> risposta dalla card (§2.12). Eventi prodotti: `connector.restarted` e `agent.state`; messaggi fuori dal
> giornale: `request.context`, `request.respond.result`. Non ci sono ancora catalogo né aggiornamenti. Schemi del protocollo 0.3.0.

## Uso

```
# registrazione: il token di installazione si passa per variabile d'ambiente o standard input,
# mai come argomento (comparirebbe nell'elenco dei processi)
MONSTERTERMINAL_INSTALL_TOKEN=mti_… monsterterminal-connector register
# sostituzione dell'host registrato su questa macchina (reinstallazione): il token attuale va al
# backend nell'header X-MonsterTerminal-Host-Token, il vecchio host viene revocato
MONSTERTERMINAL_INSTALL_TOKEN=mti_… monsterterminal-connector register -replace
# collegamento
monsterterminal-connector run
# notifiche di Claude Code: chiede «Ho trovato Claude Code: attivo le notifiche? [S/n]»
# e aggiunge i propri hook nelle impostazioni dell'utente (~/.claude/settings.json); -yes senza terminale
monsterterminal-connector hooks install
monsterterminal-connector hooks remove
```

Opzioni comuni: `-server` (default `https://connect.monsterterminal.com`), `-token-file` (default
`/etc/monsterterminal/host-token`, scritto con permessi `0600`) e `-state-dir` (default
`/var/lib/monsterterminal`: giornale degli eventi e stato dell'ultimo avvio, cartelle `0700`).

Codici di uscita: `0` fine regolare, `1` errore, `2` uso sbagliato, `3` host revocato o token dell'host
non valido — ricollegarsi non serve, occorre una nuova registrazione, e il servizio non va riavviato.

## Comportamento verso il gateway

- **Registrazione** (§1.3): un rifiuto `400 BODY_INVALID` o `403 HOST_DISABLED` (macchina fermata dal
  kill-switch) non consuma il token di installazione; il messaggio d'errore dice cosa fare.
- **Giornale** (§1.12): ogni evento si scrive sul disco prima di partire, con seq crescente; il
  backend conferma con `events.ack` e fa fede il suo segnalibro. Se il backend è indietro (`ACK_AHEAD`
  o un `events.ack` più basso) il giornale riallinea e dichiara gli eventi persi con un
  `journal.truncated` coerente. Tetto 10.000 eventi o 7 giorni: oltre, i più vecchi diventano un
  `journal.truncated`. `SEQ_GAP` e `TRUNCATION_INCOHERENT` chiudono la sessione: il ricollegamento
  riparte da `hello` e riallinea.
- **Invio dosato** (§1.17): al più 40 messaggi al secondo con raffica di 5 (45 nel secondo peggiore,
  il backend chiude a 50) e 32 eventi in volo (la coda del backend è 64).
- **Terminale** (§1.6-1.7): ogni canale è un `tmux attach` in uno pseudo-terminale, sulla sessione
  indicata dal backend (bersaglio esatto, regola dei nomi tmux), creata se richiesto. In sola lettura
  l'attach è `-r` e l'input del canale si scarta sull'host. Chiudere un canale, o perdere la
  connessione, stacca il client tmux: la sessione resta viva con gli agenti dentro. Output in frame
  da al massimo 64 KiB, accorpato per 5 ms e dosato col resto dei messaggi; oltre 4 MiB in coda i
  canali smettono di leggere da tmux finché la coda non scende sotto 1 MiB. Allo spegnimento il
  backend riceve `session.closed` con `connector-shutdown`. Richiede `tmux` sull'host. Limiti della
  prima fetta: niente specchio largo, riallineamento dello schermo, schermate e trascrizioni.
- **Stati degli agenti** (§1.21, ADR 0410): gli hook di Claude Code eseguono `monsterterminal-connector
  hook <evento>`, che ricava la sessione tmux da `TMUX_PANE` e la passa al connettore su un socket locale
  (`hook.sock` nella cartella di stato, `0600`); il connettore scrive `agent.state` nel giornale solo
  quando lo stato cambia. Solo metadati: mai prompt, output o comandi. Il comando `hook` non blocca mai
  Claude Code e non scrive nulla, con una sola eccezione voluta: l'hook di permesso.
- **Richieste di permesso** (§2.9, §2.12; §1.21 «Hook di permesso»): `hook PermissionRequest` porta al
  connettore lo strumento e il comando esatto; il connettore manda `request.context` al backend fuori
  dal giornale, scrive `attende-input`/`permesso` col `requestId`, e tiene l'hook in attesa della
  risposta dalla card per la finestra adattiva: 2 minuti se un client è collegato e attivo sulla
  sessione tmux, 60 minuti altrimenti. Il prompt nel terminale compare comunque, in parallelo: chi
  risponde prima vince. `request.respond` dalla card diventa la decisione dell'hook (`approva` →
  allow; `nega` e `testo` → deny, col testo come motivo) con esito `consegnata`; una risposta tardiva o
  su una richiesta chiusa ha esito `superata`. La risposta data nel terminale chiude la richiesta al
  cambio di stato (`PostToolUse` → `lavora`). Una sola richiesta aperta per sessione; dopo ogni
  `welcome` si rimanda il contesto delle richieste aperte.
- **Un solo connettore per cartella di stato**: `run` prende un lock (`run.lock`); un secondo avvio sulla
  stessa cartella esce con codice `1` e un messaggio. Le sessioni senza adattatore da 30 minuti passano sotto il timer di
  inattività (soglia della configurazione dell'host, 5 minuti di default), letto dall'attività di tmux.
- **Rifiuti transitori**: `429 RATE_LIMITED` e `503 UNAVAILABLE` all'upgrade non si ritentano prima del
  `Retry-After`; la chiusura `1011` (errore interno del backend) si ritenta col backoff normale.

## Riferimenti

- Schemi del protocollo: [`schemas/0.3.0/`](schemas/0.3.0/README.md) (fonte unica del contratto; le
  versioni precedenti restano nelle loro cartelle).
- **Documentazione interna.** Il codice e i commenti citano per numero le decisioni architetturali
  (ADR 0409 il connettore, ADR 0410 gli agenti AI e il catalogo) e per sezione (`§1.x`, `§2.x`) la
  specifica `monsterterminal-saas.md` di MonsterTerminal. Vivono nel repository privato di MonsterApp:
  sono riferimenti per chi sviluppa il backend, non prerequisiti per leggere questo codice. Ciò che il
  connettore fa è descritto qui e negli schemi.

## Requisiti

- Go 1.27 o superiore per la compilazione (modulo `github.com/monsterterminal/connector`). Dipendenze: `github.com/coder/websocket`,
  `github.com/creack/pty`.
- Sull'host: `tmux` per il terminale.
- Destinazione: Linux delle famiglie Debian/Ubuntu e Red Hat, x86-64 e ARM64. Eseguibile unico, senza
  dipendenze sull'host.

## Struttura prevista

| Percorso | Contenuto |
|---|---|
| `cmd/monsterterminal-connector/` | punto d'ingresso dell'eseguibile |
| `internal/protocol/` | busta, messaggi e frame binari, validati contro `schemas/` |
| `internal/transport/` | WebSocket in uscita, heartbeat, ricollegamento |
| `internal/register/` | registrazione con il token di installazione |
| `internal/terminal/` | canali del terminale sulle sessioni tmux |
| `internal/journal/` | giornale degli eventi sul disco dell'host |
| `internal/agents/` | adattatore di Claude Code (hook), timer di inattività, installazione degli hook |
| `internal/catalog/` | esecuzione delle ricette firmate |
| `internal/update/` | aggiornamento firmato e ritorno alla versione precedente |
| `internal/signing/` | verifica Ed25519 con le chiavi pubbliche incluse nel binario |
| `schemas/<versione>/` | JSON Schema del protocollo, fonte unica del contratto |
| `packaging/` | script di installazione (`install.sh`) e il suo banco di prova |

Le cartelle si creano quando arriva il codice che ci va dentro. Oggi esistono `cmd/monsterterminal-connector`,
`internal/protocol`, `internal/register`, `internal/transport`, `internal/journal`, `internal/terminal`,
`internal/agents`, `internal/hostinfo` e
`internal/fakeserver`, un backend finto che parla il protocollo 0.1.0 come il gateway vero e serve solo
ai test.

## Test

```
go test -race ./...
```

I test degli schemi controllano che ogni schema sia JSON valido, chiuso, con un `$id` coerente col
percorso e riferimenti risolti. I test del codice girano contro il backend finto su TLS: registrazione e
sostituzione, collegamento, caduta e ricollegamento, revoca, kill-switch, heartbeat, certificato non
fidato, consegna e riallineamento degli eventi, troncamenti, limiti di invio, 429/503/1011, il
terminale su un server tmux privato (non tocca le sessioni dell'utente; i test si saltano senza
`tmux`), e il cablaggio del comando in `cmd/`.

## Installazione

Il comando lo mostra la UI di MonsterTerminal, con un token di installazione monouso (§1.14):

```
curl -fsSL https://monsterterminal.com/install.sh | sudo env MONSTERTERMINAL_INSTALL_TOKEN=mti_… sh
```

Lo script (`packaging/install.sh`, POSIX sh) scarica il rilascio da GitHub, **verifica la firma
Ed25519** di `SHA256SUMS` con la chiave pubblica scritta nello script e lo SHA-256 del binario, installa
`/usr/local/bin/monsterterminal-connector`, crea l'utente dedicato `monsterterminal` (o usa quello
indicato con `--user`, oppure `root` dopo conferma esplicita: ADR 0409 D5), registra l'host e installa
il servizio `systemd` `monsterterminal-connector`. Il token dell'host finisce in
`/var/lib/monsterterminal/host-token` (`0600`); le impostazioni decise dal cliente stanno in
`/etc/monsterterminal/connector.conf` (`auto_update`, `catalog_install`: ADR 0409 D6, D7). Opzioni:
`sh install.sh --help`; `--uninstall` rimuove tutto (l'host va poi revocato dalla UI).

Lo script si ferma senza installare nulla se la firma del rilascio non è valida o se non porta la
chiave pubblica: la verifica non è opzionale. `--insecure-skip-signature` esiste solo per istanze di
prova e lo dichiara a voce alta.

Fino al primo rilascio firmato il connettore si compila dai sorgenti con
`go build ./cmd/monsterterminal-connector`.

### Rilasci

Al tag `vX.Y.Z` la CI compila in modo riproducibile, produce `SHA256SUMS`, i manifest degli
aggiornamenti (§1.10) e `install.sh`, e crea una release **in bozza**. La CI non firma mai: la chiave
privata non sta su GitHub né sui server di MonsterTerminal (§1.17). Chi custodisce la chiave ricompila
in locale, confronta gli hash, firma e pubblica con `scripts/firma-rilascio.sh`. Come verificare un
rilascio a mano: [`SECURITY.md`](SECURITY.md).

## Sicurezza e contributi

Per segnalare una vulnerabilità: [`SECURITY.md`](SECURITY.md) (support@monsterterminal.com).
Per contribuire: [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Licenza

Apache License 2.0: vedi [`LICENSE`](LICENSE) e [`NOTICE`](NOTICE).
