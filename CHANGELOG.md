# Changelog

## v0.3.0 — 2026-10-25

Secondo rilascio firmato (chiave `mt-updates-2026`). Protocollo sempre 1; il connettore implementa
gli schemi 0.5.0 (la v0.2.0 implementava la 0.3.0) e lo dichiara con `X-MonsterTerminal-Schema`.
Raccoglie le voci «non rilasciato» qui sotto entrate dopo la v0.2.0:

- `install.sh` e `scripts/firma-rilascio.sh` chiedono OpenSSL 3.0 o successivo e lo sondano: con la
  1.1.1 (Ubuntu 20.04, Debian 11) la v0.2.0 si fermava con un falso «firma non valida», ora dice cosa
  serve.
- `install.sh` verifica il rilascio prima di creare l'utente; `--uninstall` dice che l'utente resta e
  come toglierlo; `--releases-url` per i banchi di prova.
- Le domande dell'agente dalla card (schemi 0.5.0: `questions`, `answers`, `request.closed`) e l'Esc
  nel terminale che chiude la richiesta.
- `BILLING_REQUIRED` (schemi 0.4.0): alla registrazione rimanda a `/admin/billing`, a `run` riprova
  ogni 15 minuti finché l'abbonamento non è attivo.
- Banco end-to-end di `install.sh` in un container systemd (`packaging/banco`).

## Schemi 0.5.0 — 2026-10-19

- Solo aggiunte rispetto a 0.4.0 (protocollo sempre 1), ADR 0410 A2, spec §2.14: `questions`
  opzionale in `request.context` (al più 4 domande da 200 caratteri, intestazione facoltativa, al più
  10 opzioni con etichetta da 200 e descrizione da 500, `multiSelect`); `answers` opzionale in
  `request.respond` (una risposta per domanda, `question` e `text` ≤ 2000, solo con `testo`); il
  messaggio nuovo `request.closed` connettore → backend (`requestId`, `reason` ∈ `annullata`).

## Fetta 3b — 2026-10-19 (non rilasciato)

- Le domande dell'agente dalla card (spec §2.14, §1.21; ADR 0410 A2). Claude Code fa le domande con
  lo strumento `AskUserQuestion` ed emette per quello lo stesso `PermissionRequest` dei permessi:
  l'adattatore lo riconosce e apre una richiesta `kind: domanda` con `action: AskUserQuestion`,
  `detail` = la prima domanda (più «(+N domande)»), le `questions` nei tetti dello schema 0.5.0
  (testi e opzioni oltre i tetti troncati con avviso nei log; nessuna domanda, più di quattro, una
  domanda senza testo o senza opzioni → `questions` assente, la card resta al testo libero), la stessa
  finestra adattiva e lo stesso `deadline`; nel giornale `attende-input`/`domanda` col `requestId`
  (riga nuova della tabella di §1.21). La `Notification permission_prompt` che segue di 6 s non chiude
  la domanda.
- La risposta: `request.respond` con `testo` e `answers` diventa `decision: {behavior: allow,
  updatedInput: tool_input + answers}` nel formato di Claude Code (`{"<domanda>": "<risposta>"}`, la
  chiave è il testo originale della domanda, le risposte si abbinano per posizione), esito
  `consegnata`. `testo` senza `answers` risponde alla sola domanda che c'è; con più domande, con
  `questions` assenti, con un numero di risposte diverso dalle domande, e con `approva`/`nega` l'esito è
  `non-supportata` e la richiesta resta aperta. Per i permessi tutto invariato.
- Annullata nel terminale: l'Esc sul prompt fa terminare l'hook (SIGTERM); l'hook esce subito
  chiudendo la connessione, e il connettore, che vede la connessione chiusa prima di una decisione e
  prima della fine della finestra, chiude la richiesta e manda `request.closed` `{requestId, reason:
  annullata}`. Vale anche per un permesso. Le chiusure del connettore stesso (decisione, finestra
  scaduta, cambio di stato, richiesta nuova sulla stessa sessione) non producono `request.closed`.
  Prima di questa fetta il comando `hook` ignorava il SIGTERM e restava in attesa.
- Il tetto della riga sul socket degli hook sale a 64 KiB per le domande nei tetti dello schema.
- `hooks install` non cambia: `PermissionRequest` senza matcher, timeout 3660 s (verificato).
- Mai il testo delle domande o delle risposte nei log e nel giornale: solo `requestId`, sessione e
  numero di domande (§1.8, §2.12).

## Connettore — 2026-10-19 (non rilasciato)

- Repository pubblico `github.com/monsterterminal/connector` (Apache 2.0): module path rinominato,
  SECURITY.md, CONTRIBUTING.md, CI.
- `packaging/install.sh` (§1.14, ADR 0409 D5-D7): utente dedicato/esistente/root con conferma, verifica
  della firma Ed25519 di `SHA256SUMS` e dello SHA-256 del binario, registrazione, servizio systemd,
  `--uninstall`, `--dry-run`; banco di prova `packaging/install_test.sh`.
- Rilasci: `release.yml` crea una bozza con build riproducibili, `SHA256SUMS`, manifest degli
  aggiornamenti e `install.sh`; `scripts/firma-rilascio.sh` ricompila, confronta, firma e pubblica dal PC
  di chi custodisce la chiave.
- Chiavi pubbliche `mt-updates-2026` e `mt-recipes-2026` (§1.17, generate il 2026-10-19 fuori dai
  server) in `internal/signing` (verifica Ed25519 grezza e lettura dei manifest di aggiornamento
  firmati, §1.10), in `packaging/install.sh` e in SECURITY.md con le impronte; un test confronta i tre
  posti. `scripts/firma-rilascio.sh` rifiuta una chiave diversa da `mt-updates-2026`. Il primo
  rilascio firmato è `v0.2.0`.
- `run -config FILE` (default `/etc/monsterterminal/connector.conf`): legge le impostazioni decise dal
  cliente sull'host (`auto_update`, `catalog_install`) e le riporta in `hello` (§1.11), che prima
  dichiarava sempre `false`. File assente = default accesi; file malformato = il servizio non parte.
- `install.sh --releases-url URL` (solo https): base da cui scaricare il rilascio al posto di GitHub,
  per i banchi di prova; la firma resta verificata con la chiave scritta nello script.
- Banco 2 (`packaging/banco`, mt3): install.sh end-to-end in un container systemd con rilascio locale
  firmato e backend finto (`cmd/mt-fakeserver`). Rilievi recepiti: il rilascio si scarica e si
  verifica prima di creare l'utente; `--uninstall` dice che l'utente resta e come toglierlo; il 401
  alla registrazione è documentato nel README.
- `install.sh` e `scripts/firma-rilascio.sh` chiedono OpenSSL 3.0 o successivo, sondando
  `pkeyutl -rawin`: la 1.1.1 (Ubuntu 20.04, Debian 11) conosce Ed25519 ma non verifica firme grezze, e
  install.sh si fermava con un falso «firma non valida». Ora dice cosa serve (Ubuntu 22.04+, Debian
  12+, RHEL/Alma/Rocky 9+). Il rilascio `v0.2.0` ha ancora il messaggio vecchio.

## Schemi 0.4.0 — 2026-10-18

- Solo aggiunte rispetto a 0.3.0 (protocollo sempre 1): `BILLING_REQUIRED` in `http/error.json`
  (ADR 0418 D5, spec §3.13): HTTP 402 alla registrazione e al rifiuto dell'upgrade, chiusura `4402`,
  per lo spazio di lavoro senza abbonamento attivo. `HOST_DISABLED` resta al solo kill-switch.
- Header `X-MonsterTerminal-Schema` su `POST /v1/register` e sull'upgrade (§1.15): il backend decide
  da lì, chiamata per chiamata, se la barriera del billing si presenta come `BILLING_REQUIRED`
  (da 0.4.0) o come `HOST_DISABLED` (connettori fino a 0.3.0).

## Schemi 0.3.0 — 2026-10-17

- Solo aggiunte rispetto a 0.2.0 (protocollo sempre 1): `deadline` opzionale in `request.context`
  (spec §2.12, finestra adattiva).

## Connettore — 2026-10-17 (non rilasciato)

- Richieste dell'agente (spec §2.9, §2.12; §1.21 «Hook di permesso»): l'hook `PermissionRequest` di
  Claude Code apre una richiesta, manda `request.context` fuori dal giornale (strumento e comando
  esatto, al più 16 KiB) e aspetta la risposta dalla card per la finestra adattiva (2 minuti con un
  client collegato e attivo sulla sessione tmux, 60 minuti altrimenti); `request.respond` diventa la
  decisione dell'hook (`approva` → allow, `nega` → deny, `testo` → deny col testo come motivo) e il
  connettore risponde `request.respond.result` (`consegnata`, `superata`). Il prompt nel terminale
  resta visibile in parallelo: una risposta data lì chiude la richiesta come superata al cambio di
  stato (`PostToolUse` → `lavora`, nuovo hook installato). Una sola richiesta aperta per sessione; il
  contesto delle richieste aperte si rimanda dopo ogni `welcome`.
- `hooks install` scrive anche `PermissionRequest` (timeout 3660 s) e `PostToolUse`.
- `run` prende un lock sulla cartella di stato: un secondo connettore sulla stessa cartella si ferma
  subito con un messaggio (TD-MT-CONNECTOR-SOCKET-LOCK).
- Schemi 0.4.0 nel codice (`protocol.SchemaVersion`), dichiarati con l'header
  `X-MonsterTerminal-Schema` a ogni registrazione e upgrade.
- `BILLING_REQUIRED` (ADR 0418 D5): alla registrazione il messaggio manda l'owner a `/admin/billing`
  e il token di installazione resta valido; a `run` il rifiuto 402 e la chiusura 4402 si trattano
  come un kill-switch (nuovo tentativo ogni 15 minuti, nessuna uscita), così quando il tenant attiva
  l'abbonamento il connettore si riaggancia da solo. Il testo di `HOST_DISABLED` torna a parlare del
  solo kill-switch.

## Schemi 0.2.0 — 2026-10-16

- Solo aggiunte rispetto a 0.1.0 (protocollo sempre 1): `waitingFor`/`requestId` in `agent.state`,
  `request.context`, `request.respond`, `request.respond.result`, `RATE_LIMITED` e `UNAVAILABLE` in
  `http/error.json`, tetto 2^53 − 1 sui contatori.

## Connettore — 2026-10-16 (non rilasciato)

- Stati degli agenti: adattatore di Claude Code con hook (`hook`, `hooks install`/`remove` col
  consenso), livello generico col timer di inattività, capacità `hooks` in `hello`.

## Connettore — 2026-10-15 (non rilasciato)

- Allineamento al gateway del backend (ADR 0409, verbale 2026-10-13): sostituzione con
  `X-MonsterTerminal-Host-Token` (`register -replace`), giornale degli eventi con `events.ack`,
  riallineamento al segnalibro del backend (`ACK_AHEAD`), `journal.truncated` nella forma coerente,
  invio dosato sotto i limiti per host, `429`/`503` con `Retry-After`, chiusura `1011`.
- Primo evento: `connector.restarted` a ogni avvio dopo il primo (update, rollback, host-reboot,
  service-restart, crash).
- `cmd/` testabile; codice di uscita `3` per host revocato.
- Terminale: canali sulle sessioni tmux (`session.open/resize/close`, `sessions.list`, frame binari,
  sola lettura, contropressione, `connector-shutdown`). Nuova dipendenza `github.com/creack/pty`.
- Versione di sviluppo `0.1.1-dev`; il giornale si azzera quando si registra un host nuovo.

## Schemi 0.1.0 — 2026-10-12

- Prima trascrizione degli schemi dal capitolo 1 della spec `monsterterminal-saas.md`, protocollo 1
  approvato dal BOSS il 2026-10-12.
