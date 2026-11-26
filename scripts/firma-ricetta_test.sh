#!/bin/sh
# Banco di scripts/firma-ricetta.sh, senza la chiave vera: una coppia Ed25519 cifrata nasce al momento e
# la chiave pubblica e l'impronta attese dallo script sono sostituite, in una copia, con le sue. Il
# terminale lo dà python3 (pty), come in firma-rilascio_test.sh. Corrispondenza (#26): spec §1.9 e
# §1.17 (la firma copre i byte esatti della ricetta canonica; si firma solo con mt-recipes-2026; una
# ricetta non valida non si firma), ADR 0410 A4 decisione 2 (firma offline del BOSS, .sig in base64).
#
#   sh scripts/firma-ricetta_test.sh
#
# Serve go (validazione), openssl ≥ 3, python3, jq, GNU date.
# shellcheck disable=SC2016
set -eu
here=$(cd "$(dirname "$0")" && pwd)
fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; fails=$((fails + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

for t in go openssl python3 jq date; do command -v $t >/dev/null 2>&1 || { echo "serve $t" >&2; exit 1; }; done
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
PASSPHRASE='prova di banco: 12 parole'
openssl genpkey -algorithm ed25519 -aes-256-cbc -pass "pass:$PASSPHRASE" -out "$tmp/k.key" 2>/dev/null
openssl genpkey -algorithm ed25519 -aes-256-cbc -pass "pass:$PASSPHRASE" -out "$tmp/altra.key" 2>/dev/null
openssl pkey -in "$tmp/k.key" -passin "pass:$PASSPHRASE" -pubout -out "$tmp/k.pub"
fp=$(openssl pkey -pubin -in "$tmp/k.pub" -outform DER | sha256sum | cut -d' ' -f1)
b64=$(sed -n '2p' "$tmp/k.pub")
# La copia dello script punta alla chiave del banco e chiama il valida-ricetta.sh VERO (here fissato
# alla cartella degli script del repository).
mkdir -p "$tmp/scripts"
sed -e "s|^RECIPES_PUB_B64=.*|RECIPES_PUB_B64=$b64|" -e "s/^RECIPES_FINGERPRINT=.*/RECIPES_FINGERPRINT=$fp/" \
	-e "s|^here=.*|here=$here|" "$here/firma-ricetta.sh" >"$tmp/scripts/firma.sh"

in30=$(date -u -d '+30 days' +%Y-%m-%dT00:00:00Z)
CONN=/usr/local/bin/monsterterminal-connector
URL=https://github.com/anomalyco/opencode/releases/download/v2.4.2/opencode-linux-x64.tar.gz
SHA=$(printf x | sha256sum | cut -d' ' -f1)
mkdir -p "$tmp/recipes/opencode-amd64" "$tmp/recipes/brutta"
jq -cjS . >"$tmp/recipes/opencode-amd64/1.json" <<EOF
{"recipeId":"opencode-amd64","recipeVersion":1,"expiresAt":"$in30",
 "source":{"channel":"release","ref":"$URL"},"agent":"opencode",
 "platforms":[{"os":"linux","arch":["amd64"],"distro":["debian","ubuntu","rhel"]}],"requires":[],
 "steps":[{"fetch":{"url":"$URL","sha256":"$SHA","dest":"opencode-linux-x64.tar.gz"},"timeoutSeconds":900},
          {"run":["$CONN","catalog","extract","{work}/opencode-linux-x64.tar.gz",".local/opencode"],"timeoutSeconds":300},
          {"run":["$CONN","catalog","path-add",".local/opencode"],"timeoutSeconds":30}],
 "auth":{"instructions":"Open a session and run: opencode auth login"},"start":["opencode"],
 "adapter":{"kind":"generic","id":"opencode"}}
EOF
jq . "$tmp/recipes/opencode-amd64/1.json" | sed 's/opencode-amd64/brutta/' >"$tmp/recipes/brutta/1.json" # indentata: non canonica

# corsa NOME RISPOSTE ARGS…: lancia la copia dello script in un terminale vero; RISPOSTE sono le righe
# date ai prompt (qui solo la passphrase). Scrive uscita, trascrizione e numero di prompt.
corsa() {
	name=$1 answers=$2
	shift 2
	st="$tmp/st-$name"
	mkdir -p "$st"
	HOME="$tmp/home" python3 - "$st" "$answers" sh "$tmp/scripts/firma.sh" "$@" <<'PY'
import os, pty, sys, select
st, answers, argv = sys.argv[1], sys.argv[2].split("|") if sys.argv[2] else [], sys.argv[3:]
pid, fd = pty.fork()
if pid == 0:
    os.execvp(argv[0], argv)
out, seen = b"", {b"Passphrase di ": 0}
while True:
    r, _, _ = select.select([fd], [], [], 120)
    if not r:
        os.kill(pid, 9)  # fermo su un prompt che il banco non conosce: meglio rosso che appeso
        break
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        break
    if not chunk:
        break
    out += chunk
    for marker in seen:
        while out.count(marker) > seen[marker]:
            seen[marker] += 1
            os.write(fd, ((answers.pop(0) if answers else "") + "\n").encode())
prompts = sum(seen.values())
_, status = os.waitpid(pid, 0)
open(os.path.join(st, "out"), "wb").write(out)
open(os.path.join(st, "rc"), "w").write(str(os.waitstatus_to_exitcode(status)))
open(os.path.join(st, "prompts"), "w").write(str(prompts))
PY
}
rc() { cat "$tmp/st-$1/rc"; }
out() { cat "$tmp/st-$1/out"; }
prompts() { cat "$tmp/st-$1/prompts"; }
mkdir -p "$tmp/home"
# shellcheck disable=SC2034  # usata dentro le stringhe passate a eval in check
SIG=$tmp/recipes/opencode-amd64/1.json.sig

corsa ok "$PASSPHRASE" opencode-amd64 1 --key "$tmp/k.key" --recipes-dir "$tmp/recipes"
check "firma: uscita 0, .sig scritto" '[ "$(rc ok)" = 0 ] && [ -s "$SIG" ]'
check "la passphrase è chiesta UNA volta" '[ "$(prompts ok)" = 1 ] && [ "$(out ok | grep -c "Passphrase di ")" = 1 ]'
check "la passphrase non compare nella trascrizione" '! out ok | grep -qF "$PASSPHRASE"'
check ".sig è una riga di base64 di 64 byte" '[ "$(wc -l <"$SIG")" = 1 ] && [ "$(tr -d "\n" <"$SIG" | openssl base64 -d -A | wc -c)" = 64 ]'
check "la firma si verifica con openssl e la chiave pubblica del banco sui byte esatti del file" 'tr -d "\n" <"$SIG" | openssl base64 -d -A >"$tmp/sig.raw" && openssl pkeyutl -verify -pubin -inkey "$tmp/k.pub" -rawin -in "$tmp/recipes/opencode-amd64/1.json" -sigfile "$tmp/sig.raw" >/dev/null'

check "--verifica sulla firma appena fatta: valida, senza passphrase" 'sh "$tmp/scripts/firma.sh" opencode-amd64 1 --verifica --recipes-dir "$tmp/recipes" </dev/null >"$tmp/ver.out" 2>&1 && grep -q "firma valida" "$tmp/ver.out"'

corsa doppia "$PASSPHRASE" opencode-amd64 1 --key "$tmp/k.key" --recipes-dir "$tmp/recipes"
check "un .sig esistente non si sovrascrive senza --sovrascrivi, e la passphrase non viene chiesta" '[ "$(rc doppia)" != 0 ] && [ "$(prompts doppia)" = 0 ] && out doppia | grep -q "esiste già"'

corsa sovrascrivi "$PASSPHRASE" opencode-amd64 1 --key "$tmp/k.key" --recipes-dir "$tmp/recipes" --sovrascrivi
check "--sovrascrivi rifirma" '[ "$(rc sovrascrivi)" = 0 ]'

# Ricetta cambiata di un byte dopo la firma: la firma non vale più.
cp "$tmp/recipes/opencode-amd64/1.json" "$tmp/orig.json"
sed 's/"timeoutSeconds":30}/"timeoutSeconds":31}/' "$tmp/orig.json" >"$tmp/recipes/opencode-amd64/1.json"
check "--verifica dopo un byte cambiato: NON valida" '! sh "$tmp/scripts/firma.sh" opencode-amd64 1 --verifica --recipes-dir "$tmp/recipes" </dev/null >"$tmp/ver2.out" 2>&1 && grep -q "firma NON valida" "$tmp/ver2.out"'
cp "$tmp/orig.json" "$tmp/recipes/opencode-amd64/1.json"

printf 'non base64!\n' >"$tmp/recipes/opencode-amd64/1.json.sig"
check "--verifica con .sig malformato: rifiutata" '! sh "$tmp/scripts/firma.sh" opencode-amd64 1 --verifica --recipes-dir "$tmp/recipes" </dev/null >"$tmp/ver3.out" 2>&1 && grep -q "non è una firma" "$tmp/ver3.out"'
rm -f "$tmp/recipes/opencode-amd64/1.json.sig"

corsa sbagliata "non è questa" opencode-amd64 1 --key "$tmp/k.key" --recipes-dir "$tmp/recipes"
check "passphrase sbagliata: si ferma, niente .sig" '[ "$(rc sbagliata)" != 0 ] && [ ! -e "$SIG" ] && out sbagliata | grep -q "passphrase sbagliata"'

corsa altra "$PASSPHRASE" opencode-amd64 1 --key "$tmp/altra.key" --recipes-dir "$tmp/recipes"
check "chiave che non è mt-recipes-2026: si ferma, niente .sig" '[ "$(rc altra)" != 0 ] && [ ! -e "$SIG" ] && out altra | grep -q "non è mt-recipes-2026"'

corsa brutta "$PASSPHRASE" brutta 1 --key "$tmp/k.key" --recipes-dir "$tmp/recipes"
check "ricetta non canonica: rifiutata PRIMA della passphrase, niente .sig" '[ "$(rc brutta)" != 0 ] && [ "$(prompts brutta)" = 0 ] && [ ! -e "$tmp/recipes/brutta/1.json.sig" ] && out brutta | grep -q "non è valida"'

corsa manca "$PASSPHRASE" codex-amd64 1 --key "$tmp/k.key" --recipes-dir "$tmp/recipes"
check "ricetta inesistente: si ferma" '[ "$(rc manca)" != 0 ] && out manca | grep -q "non trovata"'

corsa versione "$PASSPHRASE" opencode-amd64 0 --key "$tmp/k.key" --recipes-dir "$tmp/recipes"
check "recipeVersion 0: rifiutata" '[ "$(rc versione)" != 0 ] && out versione | grep -q "recipeVersion non valida"'

check "senza terminale rifiuta (la passphrase va chiesta a video)" '! sh "$tmp/scripts/firma.sh" opencode-amd64 1 --key "$tmp/k.key" --recipes-dir "$tmp/recipes" </dev/null >/dev/null 2>&1'

check "lo script vero dichiara la chiave pubblica di internal/signing/signing.go" 'grep -q "RECIPES_PUB_B64=$(sed -n "/RecipesPEM/,/END PUBLIC/p" "$here/../internal/signing/signing.go" | sed -n "s/^\(MCow[A-Za-z0-9+/=]*\)$/\1/p")$" "$here/firma-ricetta.sh"'
check "lo script vero dichiara l'impronta della sua chiave pubblica" 'b=$(sed -n "s/^RECIPES_PUB_B64=//p" "$here/firma-ricetta.sh"); printf -- "-----BEGIN PUBLIC KEY-----\n%s\n-----END PUBLIC KEY-----\n" "$b" | openssl pkey -pubin -outform DER | sha256sum | cut -d" " -f1 | grep -qx "$(sed -n "s/^RECIPES_FINGERPRINT=//p" "$here/firma-ricetta.sh")"'

if [ "$fails" -gt 0 ]; then
	printf '%d prove ROSSE\n' "$fails"
	exit 1
fi
printf 'firma-ricetta: tutte le prove verdi\n'
