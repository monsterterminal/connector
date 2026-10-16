# MonsterTerminal Connector

Il connettore di MonsterTerminal: un piccolo programma installato sull'host del cliente, che apre una
connessione in uscita verso MonsterTerminal, registra l'host e lascia lavorare tmux e gli agenti AI
anche quando il collegamento cade.

> Stato: **seconda fetta**, allineata al gateway vero (ADR 0409, verbale 2026-10-13). Registrazione e
> sostituzione dell'host, collegamento in uscita con heartbeat, ricollegamento, revoca e kill-switch,
> giornale degli eventi con conferme del backend, canali del terminale sulle sessioni tmux, stati degli
> agenti AI (Claude Code con hook, livello generico con timer di inattività). Eventi prodotti:
> `connector.restarted` e `agent.state`. Non ci sono ancora richieste con risposta dalla card, catalogo né
> aggiornamenti. Schemi del protocollo 0.2.0.

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
  Claude Code e non scrive nulla. Le sessioni senza adattatore da 30 minuti passano sotto il timer di
  inattività (soglia della configurazione dell'host, 5 minuti di default), letto dall'attività di tmux.
- **Rifiuti transitori**: `429 RATE_LIMITED` e `503 UNAVAILABLE` all'upgrade non si ritentano prima del
  `Retry-After`; la chiusura `1011` (errore interno del backend) si ritenta col backoff normale.

## Riferimenti

- ADR 0409 — il connettore; ADR 0410 — agenti AI, stato comune, catalogo di ricette (monorepo MonsterApp).
- Spec `docs/spec/monsterterminal-saas.md`, capitolo 1: il protocollo tra connettore e backend.
- Schemi del protocollo: [`schemas/0.1.0/`](schemas/0.1.0/README.md).

## Requisiti

- Go 1.27 o superiore per la compilazione. Dipendenze: `github.com/coder/websocket`,
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
| `packaging/` | servizio systemd e script di installazione |

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

## Licenza

Apache License 2.0: vedi [`LICENSE`](LICENSE) e [`NOTICE`](NOTICE).
