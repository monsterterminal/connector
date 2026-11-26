#!/bin/sh
# Validazione di una ricetta del catalogo prima della firma (spec §1.9, ADR 0410 A4, lotto L4): la
# stessa che la CI esegue su ogni recipes/**/*.json e che scripts/firma-ricetta.sh chiama prima di
# chiedere la passphrase. Il lavoro vero lo fa cmd/mt-valida-ricetta, che riusa il tipo e la Validate
# del connettore (così «valida qui» e «accettata sull'host» sono la stessa cosa):
#   - schema 0.11.0 (campi sconosciuti rifiutati, enum, pattern, tetti);
#   - forma canonica: i byte del file sono quelli di `jq -cjS .` (chiavi ordinate, senza spazi, senza a
#     capo finale), perché la firma copre i byte del file; se jq c'è, lo si confronta anche qui;
#   - recipes/<recipeId>/<recipeVersion>.json coerente col contenuto;
#   - expiresAt dopo adesso e non oltre 180 giorni;
#   - mai http://, mai «latest»; source coerente coi passi (npm → npm install -g pacchetto@versione;
#     release → fetch dell'URL del vendor); adapter.kind hooks → ultimo passo hooks install -yes.
#
#   valida-ricetta FILE...
#   valida-ricetta --adesso 2026-11-26T12:00:00Z FILE...   (orologio fissato, solo prove)
#
# Serve go (compila il validatore al volo da questo repository); jq è facoltativo. Esce 0 se tutte le
# ricette sono valide, 1 altrimenti, 2 per uso sbagliato.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$here")
ADESSO=""

die() { printf 'valida-ricetta: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--adesso) [ $# -ge 2 ] || die "--adesso vuole un istante RFC 3339"; ADESSO=$2; shift 2 ;;
	-h | --help) sed -n '2,19p' "$0" >&2; exit 0 ;;
	--) shift; break ;;
	-*) die "opzione sconosciuta: $1" ;;
	*) break ;;
	esac
done
[ $# -gt 0 ] || die "uso: valida-ricetta [--adesso ISTANTE] FILE..."
command -v go >/dev/null 2>&1 || die "serve go: il validatore si compila da $root/cmd/mt-valida-ricetta"
[ -f "$root/cmd/mt-valida-ricetta/main.go" ] || die "non trovo cmd/mt-valida-ricetta sotto $root"

# Percorsi assoluti: il validatore gira dalla radice del repository (go run vuole il modulo).
abs=""
rc=0
for f in "$@"; do
	[ -r "$f" ] || { printf 'ERR  %s: file non leggibile\n' "$f" >&2; rc=1; continue; }
	case "$f" in /*) a=$f ;; *) a=$(cd "$(dirname "$f")" && pwd)/$(basename "$f") ;; esac
	# Controprova indipendente della forma canonica con jq, quando c'è (-j: senza a capo finale).
	if command -v jq >/dev/null 2>&1; then
		if ! jq -cjS . "$a" 2>/dev/null | cmp -s - "$a"; then
			printf 'ERR  %s: non è in forma canonica secondo jq (rigenerare con: jq -cjS . FILE > FILE.canon && mv FILE.canon FILE)\n' "$f" >&2
			rc=1
		fi
	fi
	abs="$abs
$a"
done
[ -n "$abs" ] || exit "$rc"

set --
if [ -n "$ADESSO" ]; then set -- -adesso "$ADESSO"; fi
if ! (cd "$root" && printf '%s\n' "$abs" | sed '/^$/d' | xargs -d '\n' go run ./cmd/mt-valida-ricetta "$@" --); then
	rc=1
fi
exit "$rc"
