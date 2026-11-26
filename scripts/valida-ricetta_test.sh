#!/bin/sh
# Banco di scripts/valida-ricetta.sh (spec §1.9, ADR 0410 A4, lotto L4). Costruisce in una cartella
# temporanea ricette buone e ricette con UN difetto ciascuna e verifica che lo script dica sì alle
# prime e no alle seconde, col motivo. Corrispondenza (#26): §1.9 «forma canonica», «tetto di 180
# giorni», «versioni fissate: niente latest», «solo https», «source coerente»; piano di Fase 3
# decisione 7 (hooks install -yes ultimo passo di Claude Code).
#
#   sh scripts/valida-ricetta_test.sh
#
# Serve go (il validatore si compila al volo), jq, GNU date.
# shellcheck disable=SC2016
set -eu
here=$(cd "$(dirname "$0")" && pwd)
fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; fails=$((fails + 1)); }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

for t in go jq date; do command -v $t >/dev/null 2>&1 || { echo "serve $t" >&2; exit 1; }; done
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

in30=$(date -u -d '+30 days' +%Y-%m-%dT00:00:00Z)
in200=$(date -u -d '+200 days' +%Y-%m-%dT00:00:00Z)
ieri=$(date -u -d '-1 day' +%Y-%m-%dT00:00:00Z)
CONN=/usr/local/bin/monsterterminal-connector
URL=https://github.com/anomalyco/opencode/releases/download/v2.4.2/opencode-linux-x64.tar.gz
SHA=$(printf x | sha256sum | cut -d' ' -f1)

# ricetta NOME ID VER FILTRO-JQ: scrive recipes/ID/VER.json in forma canonica partendo dalla ricetta
# release buona e applicando un filtro jq (`.` = nessuna modifica).
ricetta() {
	name=$1 id=$2 ver=$3 filter=$4
	mkdir -p "$tmp/$name/recipes/$id"
	jq -cjS "$filter" >"$tmp/$name/recipes/$id/$ver.json" <<EOF
{"recipeId":"opencode-amd64","recipeVersion":1,"expiresAt":"$in30",
 "source":{"channel":"release","ref":"$URL"},"agent":"opencode",
 "platforms":[{"os":"linux","arch":["amd64"],"distro":["debian","ubuntu","rhel"]}],"requires":[],
 "steps":[{"fetch":{"url":"$URL","sha256":"$SHA","dest":"opencode-linux-x64.tar.gz"},"timeoutSeconds":900},
          {"run":["$CONN","catalog","extract","{work}/opencode-linux-x64.tar.gz",".local/opencode"],"timeoutSeconds":300},
          {"run":["$CONN","catalog","path-add",".local/opencode"],"timeoutSeconds":30}],
 "auth":{"instructions":"Open a session and run: opencode auth login"},"start":["opencode"],
 "adapter":{"kind":"generic","id":"opencode"}}
EOF
	printf '%s\n' "$tmp/$name/recipes/$id/$ver.json"
}
# esito NOME FILE...: lancia lo script e salva codice e uscita.
esito() {
	n=$1
	shift
	if sh "$here/valida-ricetta.sh" "$@" >"$tmp/$n.out" 2>&1; then echo 0 >"$tmp/$n.rc"; else echo $? >"$tmp/$n.rc"; fi
}
rc() { cat "$tmp/$1.rc"; }
out() { cat "$tmp/$1.out"; }

buona=$(ricetta buona opencode-amd64 1 .)
esito buona "$buona"
check "ricetta release buona: esce 0 e dice ok" '[ "$(rc buona)" = 0 ] && out buona | grep -q "^ok "'

npm=$(ricetta npm claude-code 1 '.recipeId="claude-code" | .agent="claude-code" | .adapter={"kind":"hooks","id":"claude-code"}
	| .source={"channel":"npm","ref":"@anthropic-ai/claude-code@2.1.332"}
	| .requires=[{"check":"command","name":"node","minVersion":"22.0.0"},{"check":"command","name":"npm"}]
	| .steps=[{"run":["npm","install","-g","@anthropic-ai/claude-code@2.1.332","--prefix",".npm-global"],"timeoutSeconds":900},
	          {"run":["'"$CONN"'","catalog","path-add",".npm-global/bin"],"timeoutSeconds":30},
	          {"run":["'"$CONN"'","hooks","install","-yes"],"timeoutSeconds":60}]')
esito npm "$npm"
check "ricetta npm con hooks buona: esce 0" '[ "$(rc npm)" = 0 ]'

# Le ricette vere del repository passano (è ciò che la CI fa).
esito vere "$here/../recipes/claude-code/1.json"
check "la ricetta vera di Claude Code passa" '[ "$(rc vere)" = 0 ]'
if sh "$here/valida-ricetta.sh" "$here"/../recipes/*/*.json >"$tmp/tutte.out" 2>&1; then echo 0 >"$tmp/tutte.rc"; else echo $? >"$tmp/tutte.rc"; fi
check "tutte le ricette del repository passano in una chiamata sola" '[ "$(cat "$tmp/tutte.rc")" = 0 ] && [ "$(grep -c "^ok " "$tmp/tutte.out")" -ge 3 ]'

# Un difetto per volta.
pretty=$(ricetta pretty opencode-amd64 1 .)
jq . "$pretty" >"$pretty.tmp" && mv "$pretty.tmp" "$pretty"
esito pretty "$pretty"
check "non canonica (indentata): rifiutata, con il rimedio jq" '[ "$(rc pretty)" != 0 ] && out pretty | grep -q "forma canonica"'

acapo=$(ricetta acapo opencode-amd64 1 .)
printf '\n' >>"$acapo"
esito acapo "$acapo"
check "a capo finale: rifiutata (la firma copre i byte del file)" '[ "$(rc acapo)" != 0 ] && out acapo | grep -q "forma canonica"'

esito scaduta "$(ricetta scaduta opencode-amd64 1 ".expiresAt=\"$ieri\"")"
check "scaduta ieri: rifiutata" '[ "$(rc scaduta)" != 0 ] && out scaduta | grep -q "già passato"'

esito lontana "$(ricetta lontana opencode-amd64 1 ".expiresAt=\"$in200\"")"
check "scadenza a 200 giorni: rifiutata (tetto 180)" '[ "$(rc lontana)" != 0 ] && out lontana | grep -q "oltre 180 giorni"'

esito adesso "$(ricetta adesso opencode-amd64 1 .)"
if sh "$here/valida-ricetta.sh" --adesso "$(date -u -d '+40 days' +%Y-%m-%dT00:00:00Z)" "$tmp/adesso/recipes/opencode-amd64/1.json" >"$tmp/adesso2.out" 2>&1; then echo 0 >"$tmp/adesso2.rc"; else echo $? >"$tmp/adesso2.rc"; fi
check "--adesso oltre expiresAt: la stessa ricetta risulta scaduta" '[ "$(cat "$tmp/adesso2.rc")" != 0 ] && grep -q "già passato" "$tmp/adesso2.out"'

esito http "$(ricetta http opencode-amd64 1 '.auth.instructions="see http://example.com"')"
check "http:// in una stringa: rifiutata" '[ "$(rc http)" != 0 ] && out http | grep -q "http://"'

esito latest "$(ricetta latest opencode-amd64 1 '.source.ref="https://example.com/latest/x.tar.gz" | .steps[0].fetch.url="https://example.com/latest/x.tar.gz"')"
check "latest nell'URL: rifiutata" '[ "$(rc latest)" != 0 ] && out latest | grep -q "latest"'

esito ref "$(ricetta ref opencode-amd64 1 '.source.ref="https://example.com/v1/altro.tar.gz"')"
check "source.ref non scaricato da nessun fetch: rifiutata" '[ "$(rc ref)" != 0 ] && out ref | grep -q "nessun passo fetch"'

esito npmfetch "$(ricetta npmfetch opencode-amd64 1 '.source={"channel":"npm","ref":"opencode-ai@1.0.0"}')"
check "source npm senza npm install: rifiutata" '[ "$(rc npmfetch)" != 0 ] && out npmfetch | grep -q "npm install"'

esito percorso "$(ricetta percorso opencode 1 .)"
check "cartella recipes/opencode con recipeId opencode-amd64: rifiutata" '[ "$(rc percorso)" != 0 ] && out percorso | grep -q "recipeId"'

esito versione "$(ricetta versione opencode-amd64 2 .)"
check "file 2.json con recipeVersion 1: rifiutata" '[ "$(rc versione)" != 0 ] && out versione | grep -q "recipeVersion"'

esito extra "$(ricetta extra opencode-amd64 1 '.extra=1')"
check "campo sconosciuto: rifiutata" '[ "$(rc extra)" != 0 ] && out extra | grep -q "non è una ricetta"'

esito arch "$(ricetta arch opencode-amd64 1 '.platforms[0].arch=["mips"]')"
check "arch fuori enum: rifiutata" '[ "$(rc arch)" != 0 ] && out arch | grep -q "schema"'

esito hooks "$(ricetta hooks opencode-amd64 1 '.adapter.kind="hooks"')"
check "adapter hooks senza hooks install -yes in coda: rifiutata" '[ "$(rc hooks)" != 0 ] && out hooks | grep -q "hooks install -yes"'

esito mista "$buona" "$pretty"
check "una buona e una brutta nella stessa chiamata: esce 1, la buona resta ok" '[ "$(rc mista)" = 1 ] && out mista | grep -q "^ok " && out mista | grep -q "forma canonica"'

check "senza argomenti: uso sbagliato (2)" 'sh "$here/valida-ricetta.sh" >/dev/null 2>&1; [ $? = 2 ]'
check "file inesistente: esce 1" '! sh "$here/valida-ricetta.sh" "$tmp/manca.json" >/dev/null 2>&1'

if [ "$fails" -gt 0 ]; then
	printf '%d prove ROSSE\n' "$fails"
	exit 1
fi
printf 'valida-ricetta: tutte le prove verdi\n'
