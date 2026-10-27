# Schemi del protocollo — versione 0.7.0

Trascrizione in JSON Schema (draft 2020-12) del capitolo 1 della spec `docs/spec/monsterterminal-saas.md`
del monorepo MonsterApp, approvato il 2026-10-12, con le aggiunte aperte dal BOSS il 2026-10-16 (§1.15) e, dalla 0.3.0 (2026-10-17), il campo opzionale `deadline` di `request.context` (§2.12, finestra adattiva) e, dalla 0.4.0 (2026-10-18), il codice `BILLING_REQUIRED` con l'header `X-MonsterTerminal-Schema` (ADR 0418 D5, §1.15, §3.13) e, dalla 0.5.0 (2026-10-19, ADR 0410 A2, §2.14), le domande dell'agente: `questions` in `request.context`, `answers` in `request.respond` e il messaggio `request.closed` e, dalla 0.6.0 (2026-10-27, ADR 0409 A1.2), i comandi del terminale unico: `fitToPane` opzionale in `session.open`, i messaggi per canale `session.refresh`, `session.interrupt`, `session.buffer` e per sessione `session.kill`, `session.rename`, `session.zoom`, `session.capture`, ognuno con la risposta `<tipo>.result` per `re`. Sono verbi tmux fissi su una sessione dal nome validato: nessuno esegue un comando arbitrario (§1.6). Codici di `error` nuovi (non sono un enum): `CAPTURE_TOO_LARGE`, `SESSION_EXISTS`, `TMUX_UNAVAILABLE`. Dalla 0.7.0 (2026-10-27, ADR 0409 A1.5) DragMeHere sugli host: `file.put` (blocchi in sequenza verso `~/dragmehere/` o una sua sottocartella di un solo livello, creata se manca; scrittura in un temporaneo e rinomina atomica dopo lo sha256, mai bit di esecuzione) e `file.cancel`, ognuno con la risposta per `re`; definizioni `pathSegment`, `dragmehereDir` e `fileName` in `common.json`; codici `PATH_INVALID`, `FILE_EXISTS`, `FILE_TOO_LARGE`, `DISK_FULL`, `TRANSFER_LIMIT`, `TRANSFER_UNKNOWN`, `CHECKSUM_MISMATCH`, `PROTOCOL_ERROR`. Il numero di versione di questi schemi è distinto dal
numero intero del protocollo (`/v1`): gli schemi 0.x.y descrivono il protocollo 1 mentre il contratto si
assesta. Il backend usa sempre una versione precisa degli schemi, mai «l'ultima» (spec §1.19).

| Cartella | Contenuto | Spec |
|---|---|---|
| `common.json` | tipi condivisi: UUID, istanti, canale, nome di sessione, stato dell'agente, firma | §1.5-§1.9 |
| `envelope.json` | busta comune dei frame di testo | §1.5 |
| `messages/connector-to-backend/` | un file per tipo di messaggio inviato dal connettore | §1.6 |
| `messages/backend-to-connector/` | un file per tipo di messaggio inviato dal backend | §1.6 |
| `events/` | schema del campo `data` del messaggio `event`, un file per `kind` | §1.8, §1.12 |
| `http/` | corpo e risposta di `POST /v1/register`, corpo degli errori HTTP | §1.3, §1.4 |
| `objects/` | configurazione dell'host, ricetta del catalogo, manifest di aggiornamento | §1.9-§1.11 |

Tutti gli oggetti sono chiusi (`additionalProperties: false`): un campo sconosciuto rende il messaggio non
valido (§1.5).

## Frame binari del terminale (§1.7)

I dati dei terminali non sono JSON e non hanno uno schema. Ogni frame binario ha un'intestazione di 5 byte:

| Byte | Contenuto |
|---|---|
| 0 | tipo: `0x01` output del terminale (connettore → backend), `0x02` input dell'utente (backend → connettore) |
| 1-4 | canale, intero senza segno big-endian |
| 5… | byte grezzi del terminale, al massimo 64 KiB per frame |

## Firme

Ricette (`objects/recipe.json`) e manifest di aggiornamento (`objects/update-manifest.json`) si firmano con
Ed25519 sulla forma canonica: JSON con chiavi ordinate e senza spazi. Le chiavi sono due e separate, una per
le ricette e una per gli aggiornamenti (spec §1.19).

## Novità rispetto a 0.1.0 (2026-10-16)

Solo aggiunte, protocollo sempre `1`: un messaggio valido per 0.1.0 è valido anche per 0.2.0.

- `events/agent.state.json`: `waitingFor` e `requestId` opzionali, ammessi solo con `attende-input` (§1.8, §2.9).
- `messages/connector-to-backend/request.context.json`, `request.respond.result.json` e
  `messages/backend-to-connector/request.respond.json` (§2.9).
- `http/error.json`: `RATE_LIMITED` e `UNAVAILABLE` (§1.16).
- Tetto `maximum` 2^53 − 1 su `seq`, `lastAckedSeq` e `droppedCount` (§1.20).

## Novità rispetto a 0.3.0 (2026-10-18, ADR 0418 D5)

Solo aggiunte, protocollo sempre `1`: un messaggio valido per 0.3.0 è valido anche per 0.4.0.

- `http/error.json`: `BILLING_REQUIRED`. Il backend lo usa quando lo spazio di lavoro non ha un
  abbonamento attivo (spec §3.13), con due soglie diverse: alla REGISTRAZIONE (HTTP 402) in tutti gli
  stati che la barriera rifiuta (conto `none`, `free_expired`, `read_only`, `closed`); all'UPGRADE
  WebSocket (402) e nel giro periodico (chiusura `4402`) SOLO con conto `closed`. Con `free_expired` e
  `read_only` l'host registrato resta collegato: sessioni e input li ferma già la barriera del browser,
  e `free_expired` nasce dall'orologio senza un evento che lo accompagni. `HOST_DISABLED` (403, `4403`)
  resta per l'host fermato dal kill-switch del super o del tenant.
- Header `X-MonsterTerminal-Schema: 0.4.0` su `POST /v1/register` e sull'upgrade di `GET /v1`: dichiara
  la versione degli schemi che il connettore implementa (§1.15). Regola del backend: header assente o
  sotto 0.4.0 → la barriera del billing si presenta ancora come `HOST_DISABLED`/`4403`; da 0.4.0 →
  `BILLING_REQUIRED`/402/`4402`. Decide sempre l'header della chiamata corrente, mai un valore
  persistito.
- Comportamento del connettore davanti a `BILLING_REQUIRED`: come per `HOST_DISABLED`, nuovo tentativo
  ogni 15 minuti, così quando il tenant attiva l'abbonamento il connettore si riaggancia da solo. Alla
  registrazione il token di installazione NON è consumato.
