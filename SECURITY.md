# Sicurezza

## Segnalare una vulnerabilità

Scrivi a **support@monsterterminal.com**. Non aprire una issue pubblica per una vulnerabilità:
confermiamo la ricezione entro pochi giorni lavorativi e concordiamo con te tempi e modo della
pubblicazione.

Nella segnalazione metti, se puoi: la versione del connettore (`monsterterminal-connector version`),
il sistema operativo dell'host, i passi per riprodurre e l'impatto che vedi. Non inviare token di
installazione o dell'host: se ne hai esposto uno, revocalo dalla UI di MonsterTerminal.

Lo stesso indirizzo vale per il servizio `monsterterminal.com` e per il protocollo descritto in
`schemas/`.

## Versioni supportate

| Versione | Supporto |
|---|---|
| ultimo rilascio pubblicato | sì, correzioni di sicurezza |
| rilasci precedenti | no: il connettore si aggiorna da solo, se gli aggiornamenti non sono stati disattivati sull'host |
| `main` | codice di sviluppo, non un rilascio |

## Cosa fa il connettore per la sicurezza dell'host

- Apre solo connessioni in uscita verso MonsterTerminal: sull'host non si apre nessuna porta e
  nessuna chiave SSH lascia l'host.
- Il tenant è legato al token dell'host, custodito nel solo file `0600` indicato da `-token-file`; il
  token di installazione si passa per variabile d'ambiente o standard input, mai come argomento.
- Il vocabolario del protocollo è chiuso: non esiste un messaggio che esegua un comando arbitrario.
  Un backend compromesso può aprire terminali sulle sessioni tmux dell'host, non installare nulla né
  sostituire il connettore senza una firma valida.
- Nei log non finiscono mai token, contenuto del terminale, prompt o output degli agenti.
- Gli aggiornamenti e le ricette del catalogo sono firmati con Ed25519 da chiavi che non stanno sui
  server di MonsterTerminal; le chiavi pubbliche sono incluse nel binario. Un file con firma non
  valida non si installa. Le chiavi pubbliche sono elencate in fondo a questo file.

## Verificare un rilascio

Ogni release su GitHub porta `SHA256SUMS` (gli hash di tutti gli asset) e `SHA256SUMS.sig`, la firma
Ed25519 **grezza** (64 byte) fatta con la chiave degli aggiornamenti; lo stesso per `install.sh` e per
i manifest `update-manifest-linux-*.json`. Con OpenSSL 1.1.1 o successivo:

```
openssl pkeyutl -verify -pubin -inkey mt-updates.pub -rawin -in SHA256SUMS -sigfile SHA256SUMS.sig
sha256sum -c --ignore-missing SHA256SUMS
```

`mt-updates.pub` è la chiave pubblica PEM elencata qui sotto; `install.sh` la porta scritta dentro e fa
le stesse due verifiche prima di installare. La chiave privata non sta su GitHub né sui server di
MonsterTerminal: la CI crea solo release in bozza, la firma la mette chi custodisce la chiave
(`scripts/firma-rilascio.sh`) dopo aver ricompilato in locale e confrontato gli hash.

### Chiavi pubbliche

Generate il 2026-10-19 su una macchina fuori dai server di MonsterTerminal; le private non ne sono
mai uscite. Le stesse chiavi stanno nel binario (`internal/signing`) e, quella degli aggiornamenti,
in `packaging/install.sh`: un test confronta i tre posti. L'impronta è lo SHA-256 della chiave in
forma DER (`openssl pkey -pubin -in FILE -outform DER | sha256sum`).

**`mt-updates-2026`** — aggiornamenti e rilasci (`SHA256SUMS`, `install.sh`, manifest §1.10):

```
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAkb+An530Ndo71tCM0i6DByEV+XWkb4xo4/EC3wDWKlk=
-----END PUBLIC KEY-----
```

Impronta: `90efb21f2321b1a530efff3973b5d8f4f354675af509958f3f08422cd1c43b0d`

**`mt-recipes-2026`** — ricette del catalogo (ADR 0409 D7):

```
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAzdEkzqbCqbjnd5kDmEzdCtjuXXbEJZNXDZXQbsHS6Zs=
-----END PUBLIC KEY-----
```

Impronta: `12fd1e92f867a787abb5a18d4b07d8cbaaac3363c10bd921cadce06bb85bbd54`

Un cambio di chiave si annuncia qui e nel CHANGELOG con l'impronta nuova, e il rilascio che lo porta
è firmato ancora con la chiave vecchia.
