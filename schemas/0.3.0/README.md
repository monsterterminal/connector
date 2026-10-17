# Schemi del protocollo — versione 0.3.0

Trascrizione in JSON Schema (draft 2020-12) del capitolo 1 della spec `docs/spec/monsterterminal-saas.md`
del monorepo MonsterApp, approvato il 2026-10-12, con le aggiunte aperte dal BOSS il 2026-10-16 (§1.15) e, dalla 0.3.0 (2026-10-17), il campo opzionale `deadline` di `request.context` (§2.12, finestra adattiva). Il numero di versione di questi schemi è distinto dal
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
