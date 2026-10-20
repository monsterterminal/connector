# Changelog

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
