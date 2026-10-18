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
  valida non si installa. Le chiavi pubbliche verranno elencate in questo file dal primo rilascio
  firmato.

## Verificare un rilascio

Dal primo rilascio firmato, ogni release su GitHub porta `SHA256SUMS` e la sua firma staccata. La
procedura di verifica sarà descritta qui e nel README.
