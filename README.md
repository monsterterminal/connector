# MonsterTerminal Connector

Il connettore di MonsterTerminal: un piccolo programma installato sull'host del cliente, che apre una
connessione in uscita verso MonsterTerminal, registra l'host e lascia lavorare tmux e gli agenti AI
anche quando il collegamento cade.

> Stato: **v0.5.1**, schemi del protocollo **0.7.0** (sul ramo principale 0.8.0, verso la v0.6.0), in uso con il gateway di MonsterTerminal (ADR
> 0409). Cosa fa oggi: registrazione e sostituzione dell'host; collegamento in uscita con heartbeat,
> ricollegamento, revoca e kill-switch; giornale degli eventi con conferme del backend; terminale sulle
> sessioni tmux con i sette comandi di sessione (ridisegno, interruzione, appunti, chiusura, rinomina,
> zoom, cattura) e l'adattamento alla pane; **DragMeHere**, cioè i file trascinati nel browser che arrivano
> in `~/dragmehere/` sull'host; stati degli agenti AI (Claude Code con gli hook, livello generico con
> timer d'inattività); richieste di permesso e domande di Claude Code con risposta dalla card. Rilasci
> firmati con Ed25519 e installazione verificata da `install.sh`.
>
> Cosa non c'è ancora: l'aggiornamento automatico (oggi si aggiorna rilanciando `install.sh`, vedi
> [Aggiornare](#aggiornare)), il catalogo delle ricette di installazione degli agenti e gli adattatori per
> agenti diversi da Claude Code (Codex CLI è il prossimo).

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

- **Registrazione** (§1.3): un rifiuto `400 BODY_INVALID`, `403 HOST_DISABLED` (macchina fermata dal
  kill-switch) o `402 BILLING_REQUIRED` (spazio di lavoro senza abbonamento attivo, ADR 0418 D5) non
  consuma il token di installazione; il messaggio d'errore dice cosa fare.
- **Billing** (§3.13, ADR 0418 D5): ogni registrazione e ogni upgrade dichiarano la versione degli
  schemi con `X-MonsterTerminal-Schema`; davanti a `402 BILLING_REQUIRED` o alla chiusura `4402` il
  connettore ritenta ogni 15 minuti come dopo un kill-switch, e si riaggancia da solo quando il
  tenant attiva l'abbonamento.
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
  connessione, stacca il client tmux: la sessione resta viva con gli agenti dentro. Dagli schemi 0.6.0 (ADR
  0409 A1.2) il backend può aprire un canale con `fitToPane` (la finestra tmux non si ridimensiona,
  `session.opened` dichiara le dimensioni vere) e chiedere sette comandi, ognuno un verbo tmux fisso
  sulla sessione dal nome validato: `session.refresh` (ridisegno del solo client del canale),
  `session.interrupt` (esce dalla copy-mode e manda C-c; mai da un canale in sola lettura),
  `session.buffer` (l'ultimo paste-buffer se ha al più 15 secondi, altrimenti `null`), `session.kill`,
  `session.rename` (`SESSION_EXISTS` se il nome è in uso), `session.zoom` (imposta lo stato o, senza
  `on`, lo inverte; con un solo pane non fa nulla) e `session.capture` (`screen`, `view`, `last200`,
  `last1000`, al più 256 KiB, oltre `CAPTURE_TOO_LARGE`). Nessuno esegue un comando arbitrario (§1.6).
- **DragMeHere** (schemi 0.7.0, ADR 0409 A1.5): i file dal browser arrivano a blocchi (`file.put`,
  al più 192 KiB per blocco, 50 MiB per file) sempre in `~/dragmehere/` nella home dell'utente di
  servizio, o in una sua sottocartella di un solo livello scritta dall'utente; il connettore le crea
  (0755) se mancano. Niente navigazione delle cartelle. Se al posto di `~/dragmehere` o della
  sottocartella c'è un file o un symlink la risposta è `PATH_INVALID`: i link non si seguono mai. Si
  scrive in `.mt-<transferId>.part` con modo 0644 (mai bit di esecuzione) e si rinomina in modo atomico
  solo dopo lo sha256 del file intero; senza `overwrite` un file già presente, anche comparso durante il
  trasferimento, non si sovrascrive. Al più 4 trasferimenti alla volta, 60 secondi d'inattività,
  `file.cancel` dal backend, spazio libero controllato prima di scrivere; alla caduta della connessione
  i trasferimenti aperti si annullano. Output in frame
  da al massimo 64 KiB, accorpato per 5 ms e dosato col resto dei messaggi; oltre 4 MiB in coda i
  canali smettono di leggere da tmux finché la coda non scende sotto 1 MiB. Allo spegnimento il
  backend riceve `session.closed` con `connector-shutdown`. Richiede `tmux` sull'host. Non ci sono
  ancora lo specchio largo né la trascrizione continua delle sessioni.
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
- **Domande dell'agente** (§2.14, §1.21; ADR 0410 A2): Claude Code fa le domande con lo strumento
  `AskUserQuestion` ed emette per quello lo stesso `PermissionRequest`. L'adattatore lo riconosce e
  apre una richiesta `domanda` con le `questions` (testo, intestazione, opzioni, `multiSelect`, nei
  tetti degli schemi 0.5.0) e scrive `attende-input`/`domanda` col `requestId`. La risposta dalla card
  (`request.respond` con `testo` e `answers`, una per domanda) torna a Claude Code come
  `decision.behavior: allow` con `updatedInput` = `tool_input` più `answers`; `testo` senza `answers`
  risponde alla sola domanda che c'è, altrimenti (più domande, `approva`, `nega`) l'esito è
  `non-supportata` e la card lascia «Apri il terminale». L'Esc sul prompt nel terminale termina l'hook
  (SIGTERM): il connettore vede la connessione chiusa prima di una decisione, chiude la richiesta e
  manda `request.closed` con motivo `annullata` (vale anche per un permesso).

  | Evento di Claude Code | `state` | `waitingFor` |
  |---|---|---|
  | `UserPromptSubmit` | `lavora` | — |
  | `Notification` con `notification_type: permission_prompt` | `attende-input` | `permesso` |
  | `Notification` con `notification_type: idle_prompt` | `attende-input` | `sconosciuto` |
  | `PermissionRequest` | `attende-input` | `permesso`, col `requestId` |
  | `PermissionRequest` con `tool_name: AskUserQuestion` | `attende-input` | `domanda`, col `requestId` |
  | `PostToolUse` | `lavora` | — |
  | `Stop` | `ha-finito` | — |
  | `StopFailure` | `errore` | — |
  | `SessionEnd` | `inattivo` | — |

  La `Notification permission_prompt` che segue di 6 s un `PermissionRequest` (permesso o domanda) non
  è una transizione: non chiude la richiesta aperta e non scrive un secondo `agent.state`.
- **Un solo connettore per cartella di stato**: `run` prende un lock (`run.lock`); un secondo avvio sulla
  stessa cartella esce con codice `1` e un messaggio. Le sessioni senza adattatore da 30 minuti passano sotto il timer di
  inattività (soglia della configurazione dell'host, 5 minuti di default), letto dall'attività di tmux.
- **Rifiuti transitori**: `429 RATE_LIMITED` e `503 UNAVAILABLE` all'upgrade non si ritentano prima del
  `Retry-After`; la chiusura `1011` (errore interno del backend) si ritenta col backoff normale.

## Riferimenti

- Schemi del protocollo: [`schemas/0.7.0/`](schemas/0.7.0/README.md) (fonte unica del contratto; le
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
| `internal/files/` | DragMeHere: ricezione dei file a blocchi in `~/dragmehere/` |
| `internal/hostinfo/` | dati dell'host dichiarati alla registrazione |
| `internal/hostconf/` | lettura di `connector.conf` (scelte del cliente) |
| `internal/signing/` | verifica Ed25519 con le chiavi pubbliche incluse nel binario |
| `internal/catalog/` | esecuzione delle ricette firmate (prevista, non ancora presente) |
| `internal/update/` | aggiornamento firmato e ritorno alla versione precedente (previsto, non ancora presente) |
| `internal/fakeserver/` | backend finto che parla il protocollo come il gateway vero, solo per i test |
| `schemas/<versione>/` | JSON Schema del protocollo, fonte unica del contratto |
| `packaging/` | script di installazione (`install.sh`) e il suo banco di prova |
| `scripts/` | firma e pubblicazione dei rilasci |

Le cartelle si creano quando arriva il codice che ci va dentro: oggi mancano solo `internal/catalog` e
`internal/update`.

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

Il comando lo mostra la UI di MonsterTerminal, con un token di installazione monouso (§1.14). Per non
lasciare il token nella cronologia della shell, leggilo prima senza eco:

```
read -rs MT_TOK    # incolla il token e premi Invio
curl -fsSL https://monsterterminal.com/install.sh | sudo env MONSTERTERMINAL_INSTALL_TOKEN="$MT_TOK" sh; unset MT_TOK
```

Lo script (`packaging/install.sh`, POSIX sh) scarica il rilascio da GitHub, **verifica la firma
Ed25519** di `SHA256SUMS` con la chiave pubblica scritta nello script e lo SHA-256 del binario, installa
`/usr/local/bin/monsterterminal-connector`, crea l'utente dedicato `monsterterminal` (o usa quello
indicato con `--user`, oppure `root` dopo conferma esplicita: ADR 0409 D5), registra l'host e installa
il servizio `systemd` `monsterterminal-connector`. Il token dell'host finisce in
`/var/lib/monsterterminal/host-token` (`0600`); le impostazioni decise dal cliente stanno in
`/etc/monsterterminal/connector.conf` (`auto_update`, `catalog_install`: ADR 0409 D6, D7). Opzioni:
`sh install.sh --help`; `--uninstall` rimuove servizio, binario e configurazione, `--purge` anche
stato e token (l'host va poi revocato dalla UI); l'utente resta, con la sua home: `userdel -r
monsterterminal` a mano se l'aveva creato lo script. Se la registrazione fallisce (token scaduto,
revocato o già usato: 401) il servizio non viene installato e restano binario e configurazione:
basta rilanciare il comando con un token nuovo. Con una firma non valida non resta nulla.

`connector.conf` è una riga `chiave=valore` per impostazione (`auto_update`, `catalog_install` con
`yes`/`no`; `user`, `server`, `installed_version` informative), commenti con `#`. `run` lo legge
all'avvio (`-config FILE`, default quel percorso) e riporta le due scelte in `hello` (§1.11): il
backend le vede, non le può cambiare. Senza file valgono i default (tutto acceso); con una chiave
sconosciuta o un valore non interpretabile il servizio **non parte** e il log dice riga e chiave.
Cambiata un'impostazione, `systemctl restart monsterterminal-connector`.

Lo script si ferma senza installare nulla se la firma del rilascio non è valida o se non porta la
chiave pubblica: la verifica non è opzionale. `--insecure-skip-signature` esiste solo per istanze di
prova e lo dichiara a voce alta.

### Aggiornare

L'aggiornamento automatico non c'è ancora. Per passare all'ultima versione si rilancia il comando di
installazione con un token **nuovo** preso dalla UI dello stesso spazio di lavoro, **senza** `--replace`.
Lo script verifica la firma del nuovo rilascio, poi chiede al servizio, senza consumare il token, se il
server registrato qui appartiene a quello spazio. Se sì, non registra di nuovo, sostituisce il binario e
riavvia il servizio, e le sessioni tmux restano vive. Un token già usato non basta: senza un token
valido lo script non sa per quale spazio sta lavorando. Che serva un token anche solo per aggiornare è
un limite noto, da togliere con l'aggiornamento automatico.

Un server appartiene a un solo spazio di lavoro e ha un solo connettore (ADR 0409 A2). Se il token è
di un altro spazio, lo script si ferma senza toccare nulla: per spostare il server, revocalo dallo
spazio di prima e rilancia con `--replace`. Anche un `--user` diverso da quello dell'installazione
esistente ferma lo script.

`--replace` (con un token nuovo) serve solo per registrare di nuovo la macchina: l'host precedente viene
revocato e quello nuovo ha un altro identificativo. Per compilare dai sorgenti:
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
