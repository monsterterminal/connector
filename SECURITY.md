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

## L'aggiornamento automatico e l'updater root

Dalla v0.7.0 il connettore si aggiorna da solo (ADR 0438). Il servizio gira come utente non
privilegiato e **non può** sostituire il proprio eseguibile (`root:root 0755`, `ProtectSystem=full`):
lo fa un'unità systemd separata, `monsterterminal-connector-updater.service`, come root, con il
sottocomando `apply-update` del binario **installato**. Il connettore prepara in
`/var/lib/monsterterminal/update/` (cartella sua) il binario scaricato, il manifest canonico, la
firma e `apply.request`; l'updater **non si fida di nulla di ciò che trova lì**.

- **Una cartella propria, fuori dall'albero dell'utente** (ADR 0438 A1): `/var/lib/monsterterminal-updater`,
  root `0700`, accanto a `/var/lib/monsterterminal` e non sotto. Se stesse sotto, chi scrive sul
  genitore (l'utente del connettore) potrebbe rinominarla e metterne una sua al suo posto fra un
  controllo e l'uso. Prima di usarla l'updater controlla che ogni antenato sia una cartella vera di
  root, non un symlink, non scrivibile da altri; la cartella stessa deve essere di root e `0700`.
- **L'utente del servizio è detto all'updater** (`-service-user`, scritto da `install.sh` nelle unità
  dall'`User=` del servizio, `root` compreso quando il connettore gira come root): `/var/lib/monsterterminal`
  e `update/` devono essere suoi, altrimenti l'updater non tocca niente. Se la cassetta non si apre,
  `apply.request` si toglie solo quando la cartella di stato è davvero dell'utente del servizio;
  in ogni altro caso il limite di avvio dell'unità (`StartLimitBurst=20` in 10 minuti) ferma il ciclo
  dell'unità path.
- **Mai più per percorso**: la cassetta dell'utente e la cartella dell'updater si aprono una volta
  sola come `os.Root` (descrittori di cartella) e ogni lettura, scrittura, cancellazione e rinomina
  passa da lì; i sorgenti si aprono senza seguire link e senza bloccarsi (`O_NOFOLLOW`,
  `O_NONBLOCK`: una FIFO al posto di un file si apre e si scopre con `fstat`, e le unità hanno
  comunque un `TimeoutStartSec`), solo se file regolari, per contenuto e col tetto di 64 MiB — mai
  un `rename` di un file dell'utente.
- **Riverifica sulla copia**: firma Ed25519 del manifest canonico con la chiave `mt-updates-2026`
  incorporata, SHA-256 del binario contro il manifest, piattaforma, `minProtocol`, versione
  strettamente maggiore della propria; e lo SHA-256 della **copia finale**, quella scritta in
  `monsterterminal-connector.new`, si confronta di nuovo col manifest prima della rinomina: in
  `/usr/local/bin` arriva solo il contenuto verificato, non una lettura precedente.
- **Il file dell'host lo legge anche root**: `auto_update = no` in `/etc/monsterterminal/connector.conf`
  ferma l'updater (`apply.rejected` con motivo `host-config`, riportato come `update.result skipped`),
  qualunque cosa ci sia nella cassetta. L'interruttore del cliente nella pagina Server (`autoUpdate`
  dal backend) lo conosce **solo il connettore**: un pacchetto in regola messo nella cassetta a mano
  con quell'interruttore spento verrebbe installato dall'updater — è un aggiornamento firmato e più
  recente, lo stesso che il cliente riceverebbe riaccendendo l'interruttore; il file locale resta il
  solo «no» assoluto.

Solo allora conserva `monsterterminal-connector.prev`, installa con una scrittura in `.new` e un
`rename` atomico, arma il timer di ritorno e riavvia il servizio. Un binario non firmato, firmato con
un'altra chiave (per esempio quella delle ricette) o con hash diverso, messo nella cartella
dall'utente del connettore, **non viene mai installato**: l'updater scrive `apply.rejected` col motivo
e il connettore lo riporta al backend (anche un guasto dell'updater stesso lascia `apply.rejected`,
così il backend non resta «in corso» senza esito). Finché un aggiornamento aspetta il collegamento
(`pending.json`) non se ne applica un altro.

Il backend non può fabbricare un aggiornamento: il manifest che spinge è quello firmato da chi
custodisce la chiave, e il connettore e l'updater lo verificano entrambi. Il contrassegno
`"security":true` sta dentro il manifest firmato e ignora solo l'interruttore del cliente nella pagina
Server; `auto_update = no` nel file dell'host resta assoluto, anche per la sicurezza. L'unico ritorno
indietro è locale, col binario `.prev` conservato, se la versione nuova non scrive il marcatore di
collegamento entro 5 minuti; il marcatore si scrive solo dopo `welcome` **e** 30 secondi di
connessione mantenuta, così un binario che si collega e cade subito torna indietro. Un downgrade da
manifest è sempre rifiutato (`not-newer`). Gli esiti nella cassetta si tolgono solo dopo che il
backend ha ricevuto il messaggio: un `rolled-back` non si perde.

**Limite residuo, dichiarato.** Il marcatore di collegamento sta nella cassetta dell'utente del
connettore: chi ha quell'utenza può scriverlo da sé e tenere installata una versione che non si
collega. È un auto-sabotaggio del proprio servizio, senza alcuna escalation: il binario installato
resta uno firmato con `mt-updates-2026` e più recente, lo stesso che riceverebbe comunque.

## Verificare un rilascio

Ogni release su GitHub porta `SHA256SUMS` (gli hash di tutti gli asset) e `SHA256SUMS.sig`, la firma
Ed25519 **grezza** (64 byte) fatta con la chiave degli aggiornamenti; lo stesso per `install.sh` e per
i manifest `update-manifest-linux-*.json`. Con OpenSSL 3.0 o successivo (la 1.1.1 non verifica firme Ed25519 grezze):

```
openssl pkeyutl -verify -pubin -inkey mt-updates.pub -rawin -in SHA256SUMS -sigfile SHA256SUMS.sig
sha256sum -c --ignore-missing SHA256SUMS
```

`mt-updates.pub` è la chiave pubblica PEM elencata qui sotto; `install.sh` la porta scritta dentro e fa
le stesse due verifiche prima di installare. La chiave privata non sta su GitHub né sui server di
MonsterTerminal: la CI crea solo release in bozza, la firma la mette chi custodisce la chiave
(`scripts/firma-rilascio.sh`, installato come `firma-rilascio`) dopo aver ricompilato in locale e
confrontato gli hash.

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
