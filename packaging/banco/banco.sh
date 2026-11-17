#!/usr/bin/env bash
# Banco 2 del collaudo di MonsterTerminal: install.sh end-to-end in un container con systemd, con un
# rilascio locale firmato da una chiave Ed25519 usa-e-getta, servito da un nginx di banco, e un
# backend finto (cmd/mt-fakeserver) per la registrazione. Niente tocca la produzione: il download va
# solo a releases.banco, la registrazione solo a connect.banco, entrambi sulla rete Docker mt-banco.
#
#   banco.sh up         chiavi, certificati, build, rilascio firmato, rete e i tre container
#   banco.sh install    il comando di una riga nel container host + asserzioni (B2, B3, B7, B8)
#   banco.sh negative   firma sbagliata, token malformato, token sconosciuto (B10) ⇒ nessuna installazione
#   banco.sh spazi      dopo install: un altro spazio di lavoro non prende il server (ADR 0409 A2)
#   banco.sh update     il backend finto spinge un rilascio firmato v0.0.1-banco: updater root, .prev,
#                       sessione tmux viva, update.result ok, connector.restarted update (ADR 0438 D3)
#   banco.sh rollback   v0.0.2-banco con hello rotto: dopo MT_ROLLBACK_AFTER il timer ripristina
#                       v0.0.1-banco, update.result rolled-back (ADR 0438 D3 punto 4)
#   banco.sh update-negative  firma errata, chiave delle ricette, downgrade, sha diverso, URL fuori
#                       repo, interruttore del backend, security, auto_update locale, binario non
#                       firmato messo a mano in update/, symlink in update/ ⇒ niente installato
#   banco.sh uninstall  --uninstall --purge ⇒ servizio, binario, configurazione e stato assenti
#   banco.sh status     lo status-file del backend finto
#   banco.sh all        up, negative, install, spazi, update, rollback, update-negative, uninstall
#                       (lascia il banco in piedi per guardarlo)
#   banco.sh down       rimuove container, rete, immagini mt-banco-* e i file di lavoro (non la cache go)
#
# Variabili: BANCO_HOME (default ~/mt-banco), BANCO_VERSION (0.0.0-banco), BANCO_ASSETS (cartella con
# gli asset di una release già compilata: binario e install.sh, al posto della build locale),
# BANCO_GO_IMAGE (golang:1.27), BANCO_GO_CACHE (~/.cache/go-banco), BANCO_CGROUPNS (private|host),
# BANCO_ROLLBACK_AFTER (20s: l'attesa del ritorno scritta nel timer da install.sh, solo banchi).
# Tutto ciò che il banco crea sulla macchina si chiama mt-banco-*.
#
# L'aggiornamento automatico vuole un connettore che riconosca la chiave del banco: `up` copia il
# sorgente in $BANCO_HOME/src e sostituisce in internal/signing/signing.go le due chiavi pubbliche
# (aggiornamenti e ricette) con quelle usa-e-getta del banco, poi compila da lì. Il codice del
# repository non ha nessuna via di banco: il prefisso dell'URL resta quello vero di GitHub, e il
# banco risponde come github.com (alias di rete e SAN del certificato di nginx).
#
# Le asserzioni passano a «sh -c» comandi fra apici singoli che girano NEL CONTAINER: per shellcheck
# le espansioni sembrano sbagliate (SC2016), ma è la forma voluta.
# shellcheck disable=SC2016
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SRC=$(cd "$here/../.." && pwd)
BANCO_HOME=${BANCO_HOME:-$HOME/mt-banco}
BANCO_VERSION=${BANCO_VERSION:-0.0.0-banco}
BANCO_ASSETS=${BANCO_ASSETS:-}
GO_IMAGE=${BANCO_GO_IMAGE:-golang:1.27}
GO_CACHE=${BANCO_GO_CACHE:-$HOME/.cache/go-banco}
CGROUPNS=${BANCO_CGROUPNS:-private}
ROLLBACK_AFTER=${BANCO_ROLLBACK_AFTER:-20s}
NGINX_IMAGE=nginx:1.27-alpine

NET=mt-banco
HOST_C=mt-banco-host
REL_C=mt-banco-releases
FAKE_C=mt-banco-fakeserver
HOST_IMG=mt-banco-host
FAKE_IMG=mt-banco-fakeserver
HOSTNAME_C=cliente-banco
RELEASES_URL=https://releases.banco/releases/download
SERVER_URL=https://connect.banco:8443

KEYS=$BANCO_HOME/keys
CERTS=$BANCO_HOME/certs
DIST=$BANCO_HOME/dist
WWW=$BANCO_HOME/www
STATUS=$BANCO_HOME/status
SRCCOPY=$BANCO_HOME/src
RELDIR=$WWW/releases/download/v$BANCO_VERSION
# Le release «di GitHub» del banco: lo stesso nginx, raggiunto come github.com (ADR 0438 D3).
GHDIR=$WWW/monsterterminal/connector/releases/download
GH_PREFIX=https://github.com/monsterterminal/connector/releases/download
# Versioni dei rilasci di aggiornamento: v1 buona, v2 con hello rotto (ritorno), v3 di sicurezza.
UPD1=0.0.1-banco
UPD2=0.0.2-banco
UPD3=0.0.3-banco
MARKER=$BANCO_HOME/.mt-banco

fails=0
msg() { printf '%s\n' "$*" >&2; }
die() { msg "banco.sh: $*"; exit 1; }
step() { msg ""; msg "== $*"; }
ok() { msg "ok   $*"; }
fail() { msg "FAIL $*"; fails=$((fails + 1)); }
# assert DESCRIZIONE COMANDO…: ok se il comando esce 0.
assert() {
	local d=$1
	shift
	if "$@" >/dev/null 2>&1; then ok "$d"; else fail "$d"; fi
}
# hx: un comando nel container host, come root.
hx() { docker exec "$HOST_C" "$@"; }
hx_sh() { docker exec "$HOST_C" sh -c "$1"; }
verdict() {
	msg ""
	if [ "$fails" -eq 0 ]; then
		msg "== $1: tutte le asserzioni verdi"
	else
		msg "== $1: $fails asserzioni ROSSE"
		exit 1
	fi
}
need() { command -v "$1" >/dev/null 2>&1 || die "serve $1"; }
load_note() { msg "carico della macchina: $(uptime | sed 's/.*load average/load average/')"; }

# ---------- up ----------

map_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "architettura non supportata dal rilascio: $(uname -m)" ;;
	esac
}

make_keys() {
	step "Chiave Ed25519 usa-e-getta del banco (firma dei rilasci)"
	mkdir -p "$KEYS" && chmod 0700 "$KEYS"
	openssl genpkey -algorithm ed25519 -out "$KEYS/updates.key"
	chmod 0600 "$KEYS/updates.key"
	openssl pkey -in "$KEYS/updates.key" -pubout -out "$KEYS/updates.pub"
	# una seconda chiave, mai usata per il rilascio: firma «perfetta» di un altro per la prova negativa
	openssl genpkey -algorithm ed25519 -out "$KEYS/impostore.key"
	chmod 0600 "$KEYS/impostore.key"
	# la chiave delle ricette del banco (§1.17: due chiavi, mai scambiate): entra nel binario al posto
	# di mt-recipes-2026 e serve a provare che un manifest firmato con lei non si installa
	openssl genpkey -algorithm ed25519 -out "$KEYS/recipes.key"
	chmod 0600 "$KEYS/recipes.key"
	openssl pkey -in "$KEYS/recipes.key" -pubout -out "$KEYS/recipes.pub"
	FINGERPRINT=$(openssl pkey -pubin -in "$KEYS/updates.pub" -outform DER | sha256sum | cut -d' ' -f1)
	msg "impronta della chiave del banco: $FINGERPRINT"
}

make_certs() {
	step "CA del banco e certificati per releases.banco e connect.banco"
	mkdir -p "$CERTS"
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3 \
		-subj "/CN=MonsterTerminal banco CA (usa-e-getta)" \
		-addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
		-keyout "$CERTS/ca.key" -out "$CERTS/ca.crt" 2>/dev/null
	local name san
	for name in releases.banco connect.banco; do
		san="DNS:$name"
		# releases.banco risponde anche come github.com: l'URL degli aggiornamenti resta quello vero
		[ "$name" = releases.banco ] && san="DNS:$name,DNS:github.com"
		openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
			-subj "/CN=$name" -addext "subjectAltName=$san" \
			-addext "extendedKeyUsage=serverAuth" \
			-keyout "$CERTS/$name.key" -out "$CERTS/$name.csr" 2>/dev/null
		openssl x509 -req -in "$CERTS/$name.csr" -CA "$CERTS/ca.crt" -CAkey "$CERTS/ca.key" \
			-CAcreateserial -days 3 -copy_extensions copy -out "$CERTS/$name.crt" 2>/dev/null
		rm -f "${CERTS:?}/${name:?}.csr"
	done
	# i container di nginx e del backend finto girano con altri uid: le chiavi dei server devono
	# essere leggibili (sono usa-e-getta e vivono solo qui)
	chmod 0644 "$CERTS"/*.key "$CERTS"/*.crt
	msg "certificati: $(openssl x509 -in "$CERTS/connect.banco.crt" -noout -subject -ext subjectAltName | tr '\n' ' ')"
}

go_in_docker() {
	# una build alla volta, come utente corrente (niente file di root nella cache), senza VCS
	# (il sorgente copiato non ha .git) e con la cache dei moduli e dei build riusata fra i giri;
	# il sorgente è la copia con le chiavi del banco (make_src)
	mkdir -p "$GO_CACHE/mod" "$GO_CACHE/build"
	docker run --rm --user "$(id -u):$(id -g)" \
		-e HOME=/tmp -e GOCACHE=/gocache -e GOMODCACHE=/go/pkg/mod -e GOFLAGS=-buildvcs=false \
		-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH="$ARCH" \
		-v "$SRCCOPY:/src:ro" -v "$GO_CACHE/mod:/go/pkg/mod" -v "$GO_CACHE/build:/gocache" \
		-v "$DIST:/out" -w /src "$GO_IMAGE" sh -c "$1"
}

# La copia del sorgente con le chiavi pubbliche del banco al posto di quelle di produzione, in modo
# coerente con install.sh del banco (make_install_sh). Solo i blocchi PEM cambiano; la controprova
# ricalcola l'impronta della chiave scritta nel file.
make_src() {
	step "Copia del sorgente con le chiavi del banco in $SRCCOPY"
	rm -rf "${SRCCOPY:?}"
	mkdir -p "$SRCCOPY"
	tar --exclude=.git -C "$SRC" -cf - . | tar -C "$SRCCOPY" -xf -
	local sg=$SRCCOPY/internal/signing/signing.go
	awk -v upd="$(cat "$KEYS/updates.pub")" -v rec="$(cat "$KEYS/recipes.pub")" '
		/^\tUpdatesPEM = `/ { print "\tUpdatesPEM = `" upd "`"; skipping = 1; next }
		/^\tRecipesPEM = `/ { print "\tRecipesPEM = `" rec "`"; skipping = 1; next }
		skipping && /-----END PUBLIC KEY-----`$/ { skipping = 0; next }
		skipping { next }
		{ print }
	' "$SRC/internal/signing/signing.go" >"$sg"
	local got
	got=$(sed -n '/^\tUpdatesPEM = `/,/-----END PUBLIC KEY-----`$/p' "$sg" | sed 's/^\tUpdatesPEM = `//; s/`$//' | openssl pkey -pubin -outform DER | sha256sum | cut -d' ' -f1)
	[ "$got" = "$FINGERPRINT" ] || die "signing.go del banco incoerente: impronta $got, attesa $FINGERPRINT"
	grep -q "$(sed -n 2p "$KEYS/recipes.pub")" "$sg" || die "chiave delle ricette del banco non sostituita"
	ok "signing.go del banco porta la chiave degli aggiornamenti $FINGERPRINT e quella delle ricette del banco"
}

build_dist() {
	mkdir -p "$DIST"
	ASSET="monsterterminal-connector_${BANCO_VERSION}_linux_${ARCH}"
	if [ -n "$BANCO_ASSETS" ]; then
		step "Asset del rilascio da $BANCO_ASSETS (niente build del connettore)"
		[ -f "$BANCO_ASSETS/$ASSET" ] || die "manca $BANCO_ASSETS/$ASSET (BANCO_VERSION=$BANCO_VERSION)"
		[ -f "$BANCO_ASSETS/install.sh" ] || die "manca $BANCO_ASSETS/install.sh"
		cp "$BANCO_ASSETS/$ASSET" "$DIST/$ASSET"
		cp "$BANCO_ASSETS/install.sh" "$DIST/install.sh.orig"
		step "Build del backend finto con $GO_IMAGE"
		go_in_docker "go build -trimpath -o /out/mt-fakeserver ./cmd/mt-fakeserver"
	else
		step "Build riproducibile del connettore (VERSION=$BANCO_VERSION, $ARCH), dei tre rilasci di aggiornamento e del backend finto con $GO_IMAGE"
		load_note
		# v0.0.2-banco è compilato con main.bancoBrokenHello=1: registra l'avvio e non si collega mai
		# (ADR 0438 D3 punto 4: il timer di ritorno lo toglie di mezzo). Un rilascio vero non lo imposta.
		go_in_docker "go build -trimpath -ldflags '-s -w -buildid= -X main.version=$BANCO_VERSION' -o /out/$ASSET ./cmd/monsterterminal-connector \
			&& go build -trimpath -ldflags '-s -w -buildid= -X main.version=$UPD1' -o /out/$(asset_name "$UPD1") ./cmd/monsterterminal-connector \
			&& go build -trimpath -ldflags '-s -w -buildid= -X main.version=$UPD2 -X main.bancoBrokenHello=1' -o /out/$(asset_name "$UPD2") ./cmd/monsterterminal-connector \
			&& go build -trimpath -ldflags '-s -w -buildid= -X main.version=$UPD3' -o /out/$(asset_name "$UPD3") ./cmd/monsterterminal-connector \
			&& go build -trimpath -o /out/mt-fakeserver ./cmd/mt-fakeserver"
		sed "s/@VERSION@/$BANCO_VERSION/" "$SRC/packaging/install.sh" >"$DIST/install.sh.orig"
		chmod 0755 "$DIST/$(asset_name "$UPD1")" "$DIST/$(asset_name "$UPD2")" "$DIST/$(asset_name "$UPD3")"
	fi
	chmod 0755 "$DIST/$ASSET" "$DIST/mt-fakeserver"
	msg "binario: $(sha256sum "$DIST/$ASSET")"
}

asset_name() { echo "monsterterminal-connector_${1}_linux_${ARCH}"; }

# install.sh del banco: la chiave pubblica e l'impronta di produzione sostituite IN MODO COERENTE con
# la chiave usa-e-getta, così lo script verifica la firma per davvero (check_tools confronta le due).
make_install_sh() {
	step "install.sh del banco con la chiave del banco"
	awk -v pem="$(cat "$KEYS/updates.pub")" -v fp="$FINGERPRINT" '
		/^MT_UPDATES_PUBKEY_PEM="/ { print "MT_UPDATES_PUBKEY_PEM=\"" pem "\""; skipping = 1; next }
		skipping && /^-----END PUBLIC KEY-----"$/ { skipping = 0; next }
		skipping { next }
		/^MT_UPDATES_PUBKEY_FINGERPRINT=/ { print "MT_UPDATES_PUBKEY_FINGERPRINT=" fp; next }
		{ print }
	' "$DIST/install.sh.orig" >"$DIST/install.sh"
	# controprova: la chiave scritta nello script ha l'impronta scritta nello script
	local got
	got=$(sed -n '/^MT_UPDATES_PUBKEY_PEM="/,/^-----END PUBLIC KEY-----"$/p' "$DIST/install.sh" \
		| sed 's/^MT_UPDATES_PUBKEY_PEM="//; s/"$//' | openssl pkey -pubin -outform DER | sha256sum | cut -d' ' -f1)
	[ "$got" = "$FINGERPRINT" ] || die "install.sh del banco incoerente: impronta $got, attesa $FINGERPRINT"
	grep -q "^MT_UPDATES_PUBKEY_FINGERPRINT=$FINGERPRINT$" "$DIST/install.sh" || die "impronta non sostituita"
	grep -q "^MT_VERSION=\"$BANCO_VERSION\"$" "$DIST/install.sh" || die "versione non sostituita in install.sh"
	ok "chiave e impronta coerenti nello script ($FINGERPRINT)"
}

make_release() {
	step "Rilascio v$BANCO_VERSION: SHA256SUMS firmata con la chiave del banco"
	rm -rf "${WWW:?}"
	mkdir -p "$RELDIR"
	cp "$DIST/$ASSET" "$RELDIR/"
	cp "$DIST/install.sh" "$RELDIR/install.sh"
	cp "$DIST/install.sh" "$WWW/install.sh"
	(cd "$RELDIR" && sha256sum "$ASSET" install.sh >SHA256SUMS)
	openssl pkeyutl -sign -inkey "$KEYS/updates.key" -rawin -in "$RELDIR/SHA256SUMS" -out "$RELDIR/SHA256SUMS.sig"
	openssl pkeyutl -verify -pubin -inkey "$KEYS/updates.pub" -rawin -in "$RELDIR/SHA256SUMS" \
		-sigfile "$RELDIR/SHA256SUMS.sig" >/dev/null || die "la firma appena fatta non si verifica"
	[ "$(wc -c <"$RELDIR/SHA256SUMS.sig")" = 64 ] || die "la firma non è di 64 byte"
	chmod -R a+rX "$WWW"
	ok "firma di 64 byte verificata con la chiave pubblica del banco"
	cat "$RELDIR/SHA256SUMS" >&2
	[ -z "$BANCO_ASSETS" ] || return 0
	step "Rilasci di aggiornamento «su GitHub» (ADR 0438 D2): manifest canonici firmati per v$UPD1, v$UPD2, v$UPD3 (security)"
	local v sec
	for v in "$UPD1" "$UPD2" "$UPD3"; do
		mkdir -p "$GHDIR/v$v"
		cp "$DIST/$(asset_name "$v")" "$GHDIR/v$v/"
		sec=""
		[ "$v" = "$UPD3" ] && sec='"security":true,'
		make_manifest "$v" "$(asset_name "$v")" "$(sha256sum "$DIST/$(asset_name "$v")" | cut -d' ' -f1)" "$sec" >"$GHDIR/v$v/update-manifest-linux-$ARCH.json"
		openssl pkeyutl -sign -inkey "$KEYS/updates.key" -rawin -in "$GHDIR/v$v/update-manifest-linux-$ARCH.json" -out "$GHDIR/v$v/update-manifest-linux-$ARCH.json.sig"
		msg "   v$v: $(cat "$GHDIR/v$v/update-manifest-linux-$ARCH.json")"
	done
	chmod -R a+rX "$WWW"
	ok "tre manifest firmati con la chiave del banco"
}

# make_manifest VERSIONE ASSET SHA [SECURITY-FRAMMENTO]: la forma canonica ESATTA di release.yml
# (chiavi in ordine alfabetico, senza spazi; security solo se presente).
make_manifest() {
	printf '{"arch":"%s","minProtocol":1,"os":"linux",%s"sha256":"%s","url":"%s/v%s/%s","version":"%s"}' \
		"$ARCH" "${4:-}" "$3" "$GH_PREFIX" "$1" "$2" "$1"
}

# sign_b64 FILE CHIAVE: la firma Ed25519 grezza del file, in base64 (common.json#signature).
sign_b64() { openssl pkeyutl -sign -inkey "$2" -rawin -in "$1" | base64 | tr -d '\n'; }

start_containers() {
	step "Rete $NET e container: nginx (releases.banco), backend finto (connect.banco), host systemd"
	docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
	docker rm -f "$REL_C" "$FAKE_C" "$HOST_C" >/dev/null 2>&1 || true
	docker run -d --name "$REL_C" --network "$NET" --network-alias releases.banco --network-alias github.com \
		-v "$WWW:/srv/www:ro" -v "$here/nginx-releases.conf:/etc/nginx/conf.d/default.conf:ro" \
		-v "$CERTS/releases.banco.crt:/etc/nginx/tls/releases.banco.crt:ro" \
		-v "$CERTS/releases.banco.key:/etc/nginx/tls/releases.banco.key:ro" \
		"$NGINX_IMAGE" >/dev/null

	docker build -q -t "$FAKE_IMG" -f "$here/Dockerfile.fakeserver" "$DIST" >/dev/null
	rm -rf "${STATUS:?}" && mkdir -p "$STATUS" && chmod 0700 "$STATUS"
	docker run -d --name "$FAKE_C" --network "$NET" --network-alias connect.banco \
		--user "$(id -u):$(id -g)" -v "$CERTS:/certs:ro" -v "$STATUS:/status" \
		"$FAKE_IMG" -listen :8443 -tls-cert /certs/connect.banco.crt -tls-key /certs/connect.banco.key \
		-tokens 3 -token-ttl 1h -token-file /status/install-tokens -status-file /status/status.json \
		-other-tokens 1 -other-token-file /status/other-tokens -command-dir /status/cmd >/dev/null

	# --privileged: ammesso SOLO perché il container è usa-e-getta, senza porte pubblicate, e systemd
	# come PID 1 lo richiede; --cgroupns=host monta anche il cgroup della macchina (fallback).
	local cg=(--cgroupns=private)
	[ "$CGROUPNS" = host ] && cg=(--cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw)
	docker build -q -t "$HOST_IMG" -f "$here/Dockerfile.host" "$here" >/dev/null
	docker run -d --name "$HOST_C" --hostname "$HOSTNAME_C" --network "$NET" \
		--privileged "${cg[@]}" --tmpfs /run --tmpfs /run/lock --stop-signal SIGRTMIN+3 \
		"$HOST_IMG" >/dev/null

	msg "attendo systemd nel container host…"
	local i=0 state=""
	while [ $i -lt 60 ]; do
		state=$(hx systemctl is-system-running 2>/dev/null || true)
		case "$state" in running | degraded) break ;; esac
		i=$((i + 1))
		sleep 1
	done
	case "$state" in
	running | degraded) ok "systemd nel container: $state (dopo ${i}s)" ;;
	*) die "systemd non è partito nel container (stato «$state»): docker logs $HOST_C" ;;
	esac

	step "CA del banco installata nel container host"
	docker cp "$CERTS/ca.crt" "$HOST_C:/usr/local/share/ca-certificates/mt-banco.crt"
	hx update-ca-certificates >/dev/null 2>&1
	assert "machine-id generato da systemd" hx_sh 'test "$(wc -c </etc/machine-id)" -eq 33'
	assert "releases.banco in https con la CA del banco" hx curl -fsS --proto '=https' --tlsv1.2 -o /dev/null "https://releases.banco/install.sh"
	if [ -z "$BANCO_ASSETS" ]; then
		assert "github.com (alias del banco) serve il manifest di v$UPD1 in https" hx curl -fsS --proto '=https' --tlsv1.2 -o /dev/null "$GH_PREFIX/v$UPD1/update-manifest-linux-$ARCH.json"
	fi
	assert "connect.banco:8443 risponde (405 a GET /v1/register)" hx_sh 'test "$(curl -s -o /dev/null -w "%{http_code}" https://connect.banco:8443/v1/register)" = 405'
	assert "status-file iniziale: 0 registrazioni, 3 token coniati" \
		sh -c "grep -q '\"tokensMinted\": 3' '$STATUS/status.json' && grep -q '\"registrations\": \\[\\]' '$STATUS/status.json'"
	assert "token-file del backend finto 0600 con 3 token mti_" \
		sh -c "test \"\$(stat -c %a '$STATUS/install-tokens')\" = 600 && test \"\$(grep -c '^mti_' '$STATUS/install-tokens')\" = 3"
}

cmd_up() {
	need docker
	need openssl
	need sha256sum
	load_note
	ARCH=$(map_arch)
	mkdir -p "$BANCO_HOME" && : >"$MARKER"
	make_keys
	make_certs
	make_src
	build_dist
	make_install_sh
	make_release
	start_containers
	verdict "up"
	msg "pronto: banco.sh install | negative | uninstall | status | down"
}

# ---------- install ----------

require_up() {
	[ -f "$MARKER" ] || die "banco non avviato: banco.sh up"
	docker inspect "$HOST_C" >/dev/null 2>&1 || die "container $HOST_C assente: banco.sh up"
	ARCH=$(map_arch)
	ASSET="monsterterminal-connector_${BANCO_VERSION}_linux_${ARCH}"
}

token_n() { sed -n "${1}p" "$STATUS/install-tokens"; }

# run_install TOKEN LOG ARGS…: il comando di una riga del cliente, col token passato al container
# SOLO come variabile d'ambiente (--env-file: mai in argv, né di docker né della shell nel
# container). Esce con il codice di install.sh; la trascrizione è in LOG.
run_install() {
	local tok=$1 log=$2
	shift 2
	local envf="$STATUS/env.$$"
	# MT_ROLLBACK_AFTER: l'attesa del ritorno scritta nel timer, accorciata SOLO qui (install.sh lo dice)
	(umask 077 && printf 'MONSTERTERMINAL_INSTALL_TOKEN=%s\nMT_ROLLBACK_AFTER=%s\n' "$tok" "$ROLLBACK_AFTER" >"$envf")
	local rc=0
	docker exec --env-file "$envf" "$HOST_C" sh -c \
		"curl -fsSL --proto '=https' --tlsv1.2 https://releases.banco/install.sh | sh -s -- --server $SERVER_URL --releases-url $RELEASES_URL $*" \
		>"$log" 2>&1 || rc=$?
	rm -f "$envf"
	sed 's/^/   | /' "$log" >&2
	return $rc
}

wait_status() {
	# wait_status CAMPO VALORE-MINIMO SECONDI: lo status-file del backend finto raggiunge il valore
	local field=$1 min=$2 secs=$3 i=0 v=0
	while [ $i -lt "$secs" ]; do
		v=$(status_field "$field")
		[ "${v:-0}" -ge "$min" ] 2>/dev/null && return 0
		i=$((i + 1))
		sleep 1
	done
	return 1
}
status_field() { python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); v=d[sys.argv[2]]; print(len(v) if isinstance(v,list) else v)' "$STATUS/status.json" "$1" 2>/dev/null; }
status_list() { python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(" ".join(d[sys.argv[2]]) or "(nessuno)")' "$STATUS/status.json" "$1" 2>/dev/null; }
status_reg() { python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d["registrations"][int(sys.argv[2])][sys.argv[3]])' "$STATUS/status.json" "$1" "$2" 2>/dev/null; }

cmd_install() {
	require_up
	need python3
	local tok
	tok=$(token_n 1)
	[ -n "$tok" ] || die "token 1 assente in $STATUS/install-tokens"

	step "B2 — il comando di una riga nel container host (token 1, firma verificata)"
	local rc=0
	run_install "$tok" "$STATUS/install-1.log" || rc=$?
	if [ "$rc" -eq 0 ]; then ok "install.sh è uscito con 0"; else fail "install.sh è uscito con $rc"; fi
	assert "firma del rilascio verificata (riga di install.sh)" grep -q "Firma del rilascio verificata" "$STATUS/install-1.log"
	assert "integrità del binario verificata" grep -q "Integrità del binario verificata" "$STATUS/install-1.log"
	assert "«Host registrato per Acme» stampato" grep -q "Host registrato per Acme" "$STATUS/install-1.log"
	assert "il token non compare nella trascrizione" sh -c "! grep -qF '$tok' '$STATUS/install-1.log'"

	step "B2 — file, permessi, utente"
	assert "utente monsterterminal creato (system, home /home/monsterterminal)" hx_sh 'getent passwd monsterterminal | grep -q ":/home/monsterterminal:"'
	assert "/usr/local/bin/monsterterminal-connector 0755 root" hx_sh 'test "$(stat -c "%a %U" /usr/local/bin/monsterterminal-connector)" = "755 root"'
	assert "binario = asset del rilascio (sha256)" hx_sh "echo \"$(sha256sum "$DIST/$ASSET" | cut -d' ' -f1)  /usr/local/bin/monsterterminal-connector\" | sha256sum -c --status"
	assert "«version» del binario = $BANCO_VERSION" hx_sh "test \"\$(/usr/local/bin/monsterterminal-connector version)\" = $BANCO_VERSION"
	assert "/etc/monsterterminal/connector.conf con server=$SERVER_URL" hx grep -q "^server=$SERVER_URL\$" /etc/monsterterminal/connector.conf
	assert "connector.conf con installed_version=$BANCO_VERSION" hx grep -q "^installed_version=$BANCO_VERSION\$" /etc/monsterterminal/connector.conf
	assert "connector.conf con user=monsterterminal, auto_update=yes, catalog_install=yes" hx_sh 'grep -q "^user=monsterterminal$" /etc/monsterterminal/connector.conf && grep -q "^auto_update=yes$" /etc/monsterterminal/connector.conf && grep -q "^catalog_install=yes$" /etc/monsterterminal/connector.conf'
	assert "/var/lib/monsterterminal 0700 monsterterminal" hx_sh 'test "$(stat -c "%a %U" /var/lib/monsterterminal)" = "700 monsterterminal"'
	assert "/var/lib/monsterterminal/host-token 0600 monsterterminal, mth_…" hx_sh 'test "$(stat -c "%a %U" /var/lib/monsterterminal/host-token)" = "600 monsterterminal" && grep -q "^mth_" /var/lib/monsterterminal/host-token'
	assert "unit /etc/systemd/system/monsterterminal-connector.service 0644 con User=monsterterminal" hx_sh 'test "$(stat -c %a /etc/systemd/system/monsterterminal-connector.service)" = 644 && grep -q "^User=monsterterminal$" /etc/systemd/system/monsterterminal-connector.service'

	step "B3 — servizio attivo, collegato al backend finto"
	assert "systemctl is-active monsterterminal-connector = active" hx_sh 'test "$(systemctl is-active monsterterminal-connector)" = active'
	assert "servizio abilitato all'avvio" hx_sh 'test "$(systemctl is-enabled monsterterminal-connector)" = enabled'
	assert "status-file: 1 registrazione entro 30 s" wait_status registrations 1 30
	assert "registrazione: hostname=$HOSTNAME_C arch=$ARCH versione=$BANCO_VERSION utente=monsterterminal non root, senza sostituzione" \
		test "$(status_reg 0 hostname)|$(status_reg 0 arch)|$(status_reg 0 connectorVersion)|$(status_reg 0 runAsUser)|$(status_reg 0 runAsRoot)|$(status_reg 0 replace)" \
		= "$HOSTNAME_C|$ARCH|$BANCO_VERSION|monsterterminal|False|False"
	msg "   X-MonsterTerminal-Schema dichiarati (register e upgrade, informativo): $(status_list declaredSchemas)"
	assert "status-file: ≥1 upgrade entro 30 s" wait_status upgrades 1 30
	assert "status-file: ≥1 hello entro 30 s" wait_status hellos 1 30
	assert "status-file: ≥1 collegamento autenticato" wait_status connects 1 5
	assert "journal: riga «collegato» con hostId e protocollo" hx_sh 'journalctl -u monsterterminal-connector --no-pager | grep -q "msg=collegato hostId=.* protocollo=1"'
	assert "journal: nessun token di installazione in chiaro" hx_sh "! journalctl -u monsterterminal-connector --no-pager | grep -qF '$tok'"
	assert "journal: nessun token dell'host (mth_) in chiaro" hx_sh '! journalctl -u monsterterminal-connector --no-pager | grep -q "mth_"'
	assert "ps: argv del servizio senza token e con -token-file" hx_sh 'a=$(ps -o args= -p "$(systemctl show -p MainPID --value monsterterminal-connector)"); case "$a" in *mti_*|*mth_*) exit 1;; esac; case "$a" in *"-token-file /var/lib/monsterterminal/host-token"*) exit 0;; esac; exit 1'
	assert "environ del servizio senza MONSTERTERMINAL_INSTALL_TOKEN" hx_sh '! tr "\0" "\n" </proc/"$(systemctl show -p MainPID --value monsterterminal-connector)"/environ | grep -q MONSTERTERMINAL_INSTALL_TOKEN'
	assert "il servizio gira come monsterterminal" hx_sh 'test "$(ps -o user= -p "$(systemctl show -p MainPID --value monsterterminal-connector)")" = monsterterminal'
	assert "nessun file temporaneo del download rimasto in /tmp" hx_sh 'test -z "$(ls -d /tmp/tmp.* 2>/dev/null)"'

	step "B7 — il token già usato non basta più per aggiornare (ADR 0409 A2: la verifica lo vuole valido)"
	local before_b7
	before_b7=$(snapshot_host)
	rc=0
	run_install "$tok" "$STATUS/install-2a.log" || rc=$?
	if [ "$rc" -ne 0 ]; then ok "install.sh si ferma (uscita $rc)"; else fail "install.sh è uscito con 0"; fi
	assert "spiega che il token è già usato" grep -q "INSTALL_TOKEN_INVALID" "$STATUS/install-2a.log"
	assert "macchina intatta" test "$(snapshot_host)" = "$before_b7"

	step "B7 — secondo install.sh senza --replace, token nuovo: registrazione saltata"
	local pid_before
	pid_before=$(hx systemctl show -p MainPID --value monsterterminal-connector)
	rc=0
	run_install "$(token_n 2)" "$STATUS/install-2.log" || rc=$?
	if [ "$rc" -eq 0 ]; then ok "install.sh è uscito con 0"; else fail "install.sh è uscito con $rc"; fi
	assert "«già registrato per questo spazio di lavoro» stampato" grep -q "già registrato per questo spazio di lavoro" "$STATUS/install-2.log"
	assert "status-file: ancora 1 registrazione" test "$(status_field registrations)" = 1
	assert "servizio ancora attivo" hx_sh 'test "$(systemctl is-active monsterterminal-connector)" = active'
	assert "servizio riavviato: gira il binario appena installato (MainPID cambiato)" \
		test "$(hx systemctl show -p MainPID --value monsterterminal-connector)" != "$pid_before"

	step "B8 — install.sh --replace con il token 2: host sostituito"
	local tok2 old_host_token
	tok2=$(token_n 2)
	old_host_token=$(hx cat /var/lib/monsterterminal/host-token)
	rc=0
	run_install "$tok2" "$STATUS/install-3.log" --replace || rc=$?
	if [ "$rc" -eq 0 ]; then ok "install.sh --replace è uscito con 0"; else fail "install.sh --replace è uscito con $rc"; fi
	assert "«Host sostituito» stampato" grep -q "Host sostituito" "$STATUS/install-3.log"
	assert "status-file: 2 registrazioni, la seconda con la prova di possesso" \
		test "$(status_field registrations)|$(status_reg 1 replace)" = "2|True"
	assert "host-token cambiato" hx_sh "test \"\$(cat /var/lib/monsterterminal/host-token)\" != '$old_host_token'"
	assert "status-file: ≥2 collegamenti entro 40 s (il servizio riparte col token nuovo)" wait_status connects 2 40
	assert "servizio attivo dopo la sostituzione" hx_sh 'test "$(systemctl is-active monsterterminal-connector)" = active'
	verdict "install"
}

# ---------- negative ----------

# ADR 0409 A2 (decisione BOSS 2026-10-29): dopo `install` l'host è registrato nello spazio Acme. Uno
# spazio diverso non lo prende né con il comando normale né con --replace; un --user diverso da quello
# dell'installazione si rifiuta; lo stesso spazio aggiorna senza registrare. Nessun tentativo tocca
# file, servizio o token. Usa il token 3, che negative non consuma (ogni sua prova fallisce prima).
cmd_spazi() {
	require_up
	need python3
	local other before after regs rc
	other=$(sed -n 1p "$STATUS/other-tokens")
	[ -n "$other" ] || die "token dell'altro spazio assente in $STATUS/other-tokens"
	hx test -s /var/lib/monsterterminal/host-token || die "host non registrato: prima banco.sh install"
	before=$(snapshot_host)
	regs=$(status_field registrations)

	step "S1 — un altro spazio lancia il comando di una riga sullo stesso server"
	rc=0
	run_install "$other" "$STATUS/spazi-1.log" || rc=$?
	if [ "$rc" -ne 0 ]; then ok "install.sh si ferma (uscita $rc)"; else fail "install.sh è uscito con 0"; fi
	assert "messaggio: già registrato per un altro spazio di lavoro" grep -q "già registrato per un altro spazio di lavoro" "$STATUS/spazi-1.log"

	step "S2 — lo stesso, con --replace"
	rc=0
	run_install "$other" "$STATUS/spazi-2.log" --replace || rc=$?
	if [ "$rc" -ne 0 ]; then ok "install.sh si ferma (uscita $rc)"; else fail "install.sh è uscito con 0"; fi
	assert "messaggio: già registrato per un altro spazio di lavoro" grep -q "già registrato per un altro spazio di lavoro" "$STATUS/spazi-2.log"
	assert "il token dell'altro spazio non compare nelle trascrizioni" sh -c "! grep -qF '$other' '$STATUS/spazi-1.log' '$STATUS/spazi-2.log'"

	step "S3 — lo stesso spazio con un --user diverso da quello installato"
	rc=0
	run_install "$(token_n 3)" "$STATUS/spazi-3.log" --user root --i-accept-root || rc=$?
	if [ "$rc" -ne 0 ]; then ok "install.sh si ferma (uscita $rc)"; else fail "install.sh è uscito con 0"; fi
	assert "messaggio: già installato per l'utente monsterterminal" grep -q "già installato per l'utente monsterterminal" "$STATUS/spazi-3.log"

	after=$(snapshot_host)
	if [ "$before" = "$after" ]; then ok "binario, configurazione, unit e token dell'host intatti"; else fail "la macchina è cambiata: $after"; fi
	assert "nessuna registrazione nuova dal backend" test "$(status_field registrations)" = "$regs"
	assert "servizio ancora attivo" hx systemctl is-active --quiet monsterterminal-connector

	step "S4 — lo stesso spazio, comando normale: aggiorna senza registrare"
	rc=0
	run_install "$(token_n 3)" "$STATUS/spazi-4.log" || rc=$?
	if [ "$rc" -eq 0 ]; then ok "install.sh è uscito con 0"; else fail "install.sh è uscito con $rc"; fi
	assert "messaggio: registrato per questo spazio, aggiorno" grep -q "già registrato per questo spazio di lavoro" "$STATUS/spazi-4.log"
	assert "nessuna registrazione nuova dal backend" test "$(status_field registrations)" = "$regs"
	assert "token dell'host invariato" test "$(hx sha256sum /var/lib/monsterterminal/host-token)" = "$(printf '%s\n' "$before" | grep host-token)"
	verdict "spazi"
}

snapshot_host() {
	hx_sh 'for f in /usr/local/bin/monsterterminal-connector /etc/monsterterminal/connector.conf /etc/systemd/system/monsterterminal-connector.service /var/lib/monsterterminal/host-token; do if [ -e "$f" ]; then sha256sum "$f"; else echo "assente $f"; fi; done'
}
# Le richieste a nginx, sia come releases.banco (/releases/download/) sia come github.com
# (/monsterterminal/connector/releases/download/).
downloads_count() { docker logs "$REL_C" 2>&1 | grep -cE 'GET (/monsterterminal/connector)?/releases/download/' || true; }
binary_downloads() { docker logs "$REL_C" 2>&1 | grep -cE "GET (/monsterterminal/connector)?/releases/download/.*/monsterterminal-connector_" || true; }

cmd_negative() {
	require_up
	need python3
	local tok3 before after rc bin_before
	tok3=$(token_n 3)
	local installed=no
	hx test -s /var/lib/monsterterminal/host-token 2>/dev/null && installed=yes
	msg "host già installato: $installed (le prove negative non devono cambiare nulla)"

	step "N1 — firma di un'altra chiave (perfetta ma non del banco) ⇒ «la firma del rilascio NON è valida»"
	# La verifica della firma precede la creazione dell'utente (PR #9): su un host pulito, dopo N1,
	# l'utente monsterterminal NON deve esistere. Un utente lasciato da un giro precedente si toglie
	# prima, così l'asserzione vale a ogni rilancio; se l'host è registrato l'utente serve e si salta.
	local user_check=yes
	if [ "$installed" = yes ]; then
		user_check=no
	elif hx getent passwd monsterterminal >/dev/null 2>&1; then
		msg "   utente monsterterminal di un giro precedente: lo tolgo (userdel -r) prima della prova"
		hx userdel -r monsterterminal >/dev/null 2>&1 || true
	fi
	cp "$RELDIR/SHA256SUMS.sig" "$STATUS/SHA256SUMS.sig.buona"
	openssl pkeyutl -sign -inkey "$KEYS/impostore.key" -rawin -in "$RELDIR/SHA256SUMS" -out "$RELDIR/SHA256SUMS.sig"
	before=$(snapshot_host)
	bin_before=$(binary_downloads)
	rc=0
	run_install "$tok3" "$STATUS/negative-firma.log" || rc=$?
	cp "$STATUS/SHA256SUMS.sig.buona" "$RELDIR/SHA256SUMS.sig"
	if [ "$rc" -ne 0 ]; then ok "install.sh è uscito con $rc (≠0)"; else fail "install.sh è uscito con 0 con la firma sbagliata"; fi
	assert "messaggio «la firma del rilascio NON è valida»" grep -q "la firma del rilascio NON è valida" "$STATUS/negative-firma.log"
	assert "il binario NON è stato scaricato (nginx: $bin_before download prima e dopo)" test "$(binary_downloads)" = "$bin_before"
	after=$(snapshot_host)
	assert "binario, conf, unit e token invariati" test "$before" = "$after"
	if [ "$user_check" = yes ]; then
		assert "utente monsterterminal NON creato (la firma si verifica prima di ensure_user)" hx_sh '! getent passwd monsterterminal >/dev/null 2>&1'
	else
		msg "   utente già presente (host registrato): la prova «utente non creato» si fa su un host pulito"
	fi
	assert "firma buona ripristinata e verificabile" openssl pkeyutl -verify -pubin -inkey "$KEYS/updates.pub" -rawin -in "$RELDIR/SHA256SUMS" -sigfile "$RELDIR/SHA256SUMS.sig"

	step "N2 — token malformato ⇒ fermo prima di scaricare"
	before=$(downloads_count)
	rc=0
	run_install "mti_corto" "$STATUS/negative-token.log" || rc=$?
	if [ "$rc" -ne 0 ]; then ok "install.sh è uscito con $rc (≠0)"; else fail "install.sh è uscito con 0 con un token malformato"; fi
	assert "messaggio «il token di installazione non ha la forma attesa»" grep -q "non ha la forma attesa" "$STATUS/negative-token.log"
	assert "nessun download del rilascio (nginx: $before richieste prima e dopo)" test "$(downloads_count)" = "$before"
	assert "binario, conf, unit e token invariati" test "$(snapshot_host)" = "$after"

	if [ "$installed" = no ]; then
		step "N3 (B10) — token ben formato ma sconosciuto al backend ⇒ 401 INSTALL_TOKEN_INVALID, servizio non installato"
		rc=0
		run_install "mti_$(printf 'A%.0s' $(seq 1 43))" "$STATUS/negative-401.log" || rc=$?
		if [ "$rc" -ne 0 ]; then ok "install.sh è uscito con $rc (≠0)"; else fail "install.sh è uscito con 0 con un token sconosciuto"; fi
		assert "messaggio con HTTP 401 INSTALL_TOKEN_INVALID e «registrazione fallita»" sh -c "grep -q 'INSTALL_TOKEN_INVALID' '$STATUS/negative-401.log' && grep -q 'registrazione fallita' '$STATUS/negative-401.log'"
		assert "nessun host-token, nessuna unit, servizio non attivo" hx_sh '! test -e /var/lib/monsterterminal/host-token && ! test -e /etc/systemd/system/monsterterminal-connector.service && ! systemctl is-active --quiet monsterterminal-connector'
		assert "status-file: 0 registrazioni" test "$(status_field registrations)" = 0
		msg "   nota: su 401 il binario e connector.conf restano installati (install_files precede register_host); nessun servizio"
	else
		msg "N3 (B10) saltata: l'host è già registrato, install.sh non invierebbe il token"
	fi
	verdict "negative"
}

# ---------- uninstall ----------

cmd_uninstall() {
	require_up
	step "--uninstall --purge nel container host"
	local rc=0
	docker exec "$HOST_C" sh -c \
		"curl -fsSL --proto '=https' --tlsv1.2 https://releases.banco/install.sh | sh -s -- --uninstall --purge" \
		>"$STATUS/uninstall.log" 2>&1 || rc=$?
	sed 's/^/   | /' "$STATUS/uninstall.log" >&2
	if [ "$rc" -eq 0 ]; then ok "install.sh --uninstall --purge è uscito con 0"; else fail "uscito con $rc"; fi
	assert "unit assente" hx_sh '! test -e /etc/systemd/system/monsterterminal-connector.service'
	assert "servizio non attivo e non abilitato" hx_sh '! systemctl is-active --quiet monsterterminal-connector && ! systemctl is-enabled --quiet monsterterminal-connector 2>/dev/null'
	assert "binario assente" hx_sh '! test -e /usr/local/bin/monsterterminal-connector'
	assert "/etc/monsterterminal assente" hx_sh '! test -e /etc/monsterterminal'
	assert "/var/lib/monsterterminal assente (--purge)" hx_sh '! test -e /var/lib/monsterterminal'
	assert "nessun processo del connettore" hx_sh '! pgrep -f "^/usr/local/bin/monsterterminal-connector run" >/dev/null'
	assert "l'utente monsterterminal resta e lo script lo dice con il comando userdel" \
		sh -c "grep -q 'L.utente monsterterminal resta.*userdel -r monsterterminal' '$STATUS/uninstall.log'"
	assert "utente monsterterminal ancora presente (non è compito dello script toglierlo)" hx getent passwd monsterterminal
	verdict "uninstall"
}

# ---------- aggiornamento automatico (ADR 0438 D3, D4, D6) ----------

# Il backend finto si comanda con file JSON in $STATUS/cmd (mt-fakeserver -command-dir): niente porte.
cmd_n=0
send_cmd() {
	cmd_n=$((cmd_n + 1))
	mkdir -p "$STATUS/cmd"
	local f="$STATUS/cmd/$(date +%s)-$$-$cmd_n.json"
	printf '%s\n' "$1" >"$f.tmp" && mv "$f.tmp" "$f"
	# il fakeserver li consuma ogni 250 ms
	local i=0
	while [ -e "$f" ] && [ $i -lt 40 ]; do i=$((i + 1)); sleep 0.25; done
	[ ! -e "$f" ] || die "il backend finto non ha consumato il comando $f"
}
# push_manifest FILE-MANIFEST CHIAVE: update.available spinto a tutti gli host collegati.
push_manifest() {
	send_cmd "{\"action\":\"push\",\"manifest\":$(cat "$1"),\"signature\":\"$(sign_b64 "$1" "$2")\"}"
}
backend_switch() {
	if [ "$1" = assente ]; then send_cmd '{"action":"auto-update"}'; else send_cmd "{\"action\":\"auto-update\",\"value\":$1}"; fi
}
installed_sha() { hx sha256sum /usr/local/bin/monsterterminal-connector | cut -d' ' -f1; }
asset_sha() { sha256sum "$DIST/$(asset_name "$1")" | cut -d' ' -f1; }
wait_binary() {
	# wait_binary SHA SECONDI: il binario installato ha questo sha
	local i=0
	while [ $i -lt "$2" ]; do
		[ "$(installed_sha)" = "$1" ] && return 0
		i=$((i + 1))
		sleep 1
	done
	return 1
}
wait_gone() {
	# wait_gone PERCORSO SECONDI: il file nel container non c'è più
	local i=0
	while [ $i -lt "$2" ]; do
		hx test -e "$1" || return 0
		i=$((i + 1))
		sleep 1
	done
	return 1
}
# wait_result VERSIONE ESITO MOTIVO SECONDI: nello status-file c'è un update.result così (motivo
# vuoto = qualunque).
wait_result() {
	local i=0
	while [ $i -lt "$4" ]; do
		python3 - "$STATUS/status.json" "$1" "$2" "$3" <<'PY' 2>/dev/null && return 0
import json, sys
d = json.load(open(sys.argv[1]))
v, o, r = sys.argv[2], sys.argv[3], sys.argv[4]
ok = any(x["version"] == v and x["outcome"] == o and (r == "" or x.get("reason", "") == r) for x in d["updateResults"])
sys.exit(0 if ok else 1)
PY
		i=$((i + 1))
		sleep 1
	done
	return 1
}
# wait_restart PRECEDENTE VERSIONE MOTIVO SECONDI: nello status-file c'è un connector.restarted così.
wait_restart() {
	local i=0
	while [ $i -lt "$4" ]; do
		python3 - "$STATUS/status.json" "$1" "$2" "$3" <<'PY' 2>/dev/null && return 0
import json, sys
d = json.load(open(sys.argv[1]))
ok = any(x["previousVersion"] == sys.argv[2] and x["version"] == sys.argv[3] and x["reason"] == sys.argv[4] for x in d["restarts"])
sys.exit(0 if ok else 1)
PY
		i=$((i + 1))
		sleep 1
	done
	return 1
}
results_json() { python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1]))["updateResults"]))' "$STATUS/status.json"; }
tmux_mt() { hx runuser -u monsterterminal -- env HOME=/home/monsterterminal tmux "$@"; }
# Il PID del server tmux lo dice tmux stesso (#{pid}): il processo si chiama «tmux: server» e un pgrep
# per nome è fragile.
tmux_server_cgroup() {
	local pid
	pid=$(tmux_mt display-message -p -t prima-del-rilascio '#{pid}' 2>/dev/null | tr -d '\r')
	[ -n "$pid" ] && hx cat "/proc/$pid/cgroup" 2>/dev/null
}
tmux_cgroup_ok() {
	local srv main
	srv=$(tmux_server_cgroup)
	main=$(hx_sh 'cat "/proc/$(systemctl show -p MainPID --value monsterterminal-connector)/cgroup"')
	[ -n "$srv" ] && [ "$srv" = "$main" ]
}
wait_tmux() {
	# wait_tmux SESSIONE SECONDI: la sessione tmux dell'utente del connettore esiste
	local i=0
	while [ $i -lt "$2" ]; do
		tmux_mt has-session -t "$1" >/dev/null 2>&1 && return 0
		i=$((i + 1))
		sleep 1
	done
	return 1
}
require_update_bench() {
	require_up
	need python3
	[ -z "$BANCO_ASSETS" ] || die "l'aggiornamento automatico vuole la build locale (il connettore con la chiave del banco), non BANCO_ASSETS"
	hx test -s /var/lib/monsterterminal/host-token || die "host non registrato: prima banco.sh install"
	hx systemctl is-active --quiet monsterterminal-connector || die "servizio non attivo: prima banco.sh install"
}
mailbox_clean() {
	# la cassetta delle lettere non deve trattenere niente dopo un esito
	hx_sh '! test -e /var/lib/monsterterminal/update/apply.request && ! test -e /var/lib/monsterterminal/update/applied.json && ! test -e /var/lib/monsterterminal/update/apply.rejected && ! test -e /var/lib/monsterterminal/update/rolled-back.json && ! test -e /var/lib/monsterterminal/updater/pending.json'
}

cmd_update() {
	require_update_bench
	local sha0 sha1
	sha0=$(asset_sha "$BANCO_VERSION")
	sha1=$(asset_sha "$UPD1")
	[ "$(installed_sha)" = "$sha0" ] || die "il binario installato non è v$BANCO_VERSION: banco.sh install prima di update"

	step "U0 — prima dell'aggiornamento: il connettore chiede, il backend finto risponde update.none"
	assert "status-file: ≥1 update.check entro 30 s (subito dopo welcome, ADR 0438 D4)" wait_status updateChecks 1 30
	assert "status-file: ≥1 update.none" wait_status updateNones 1 5
	assert "hello dichiara la capacità auto-update" sh -c "grep -q '\"auto-update\"' '$STATUS/status.json'"
	assert "unità updater.path attiva e abilitata" hx_sh 'test "$(systemctl is-active monsterterminal-connector-updater.path)" = active && test "$(systemctl is-enabled monsterterminal-connector-updater.path)" = enabled'
	assert "timer di ritorno non attivo, nessun .prev" hx_sh '! systemctl is-active --quiet monsterterminal-connector-rollback.timer && ! test -e /usr/local/bin/monsterterminal-connector.prev'
	assert "timer di ritorno con OnActiveSec=$ROLLBACK_AFTER (override di banco)" hx grep -q "^OnActiveSec=$ROLLBACK_AFTER\$" /etc/systemd/system/monsterterminal-connector-rollback.timer
	assert "cassetta update/ 0700 dell'utente del connettore" hx_sh 'test "$(stat -c "%a %U" /var/lib/monsterterminal/update)" = "700 monsterterminal"'

	step "U1 — una sessione tmux aperta DAL CONNETTORE prima dell'aggiornamento (session.open dal backend finto): deve sopravvivere al riavvio (ADR 0438 D3 punto 5, KillMode=process)"
	tmux_mt kill-server 2>/dev/null || true
	send_cmd '{"action":"session.open","session":"prima-del-rilascio"}'
	assert "sessione tmux «prima-del-rilascio» aperta dal connettore entro 15 s" wait_tmux prima-del-rilascio 15
	assert "il server tmux sta nello stesso cgroup del servizio (figlio del connettore, non del banco)" tmux_cgroup_ok
	msg "   cgroup del server tmux: $(tmux_server_cgroup | tr '\n' ' ') — del servizio: $(hx_sh 'cat "/proc/$(systemctl show -p MainPID --value monsterterminal-connector)/cgroup"' | tr '\n' ' ')"

	step "U2 — il backend finto spinge update.available v$UPD1 (firmato con la chiave del banco)"
	local pid_before
	pid_before=$(hx systemctl show -p MainPID --value monsterterminal-connector)
	push_manifest "$GHDIR/v$UPD1/update-manifest-linux-$ARCH.json" "$KEYS/updates.key"
	assert "binario sostituito con l'asset v$UPD1 entro 90 s (download, updater root, rename)" wait_binary "$sha1" 90
	assert ".prev presente = il binario di prima (v$BANCO_VERSION)" hx_sh "test \"\$(sha256sum /usr/local/bin/monsterterminal-connector.prev | cut -d' ' -f1)\" = $sha0"
	assert "binario installato root 0755" hx_sh 'test "$(stat -c "%a %U" /usr/local/bin/monsterterminal-connector)" = "755 root"'
	assert "cartella updater/ root 0700" hx_sh 'test "$(stat -c "%a %U" /var/lib/monsterterminal/updater)" = "700 root"'
	assert "il binario scaricato in update/ non è eseguibile" hx_sh 'f=/var/lib/monsterterminal/update/binary; ! test -e "$f" || test "$(stat -c %a "$f")" = 600'
	assert "download dal prefisso vero delle release (github.com alias del banco)" sh -c "docker logs $REL_C 2>&1 | grep -q 'GET /monsterterminal/connector/releases/download/v$UPD1/'"
	assert "update.result ok v$UPD1 entro 60 s (dal connettore nuovo, dopo welcome)" wait_result "$UPD1" ok "" 60
	assert "«version» del binario = $UPD1" hx_sh "test \"\$(/usr/local/bin/monsterterminal-connector version)\" = $UPD1"
	assert "servizio attivo con un MainPID nuovo" hx_sh "test \"\$(systemctl is-active monsterterminal-connector)\" = active && test \"\$(systemctl show -p MainPID --value monsterterminal-connector)\" != $pid_before"
	assert "connector.restarted con reason update ($BANCO_VERSION → $UPD1) entro 30 s" wait_restart "$BANCO_VERSION" "$UPD1" update 30
	assert ".prev rimosso dal timer di ritorno entro 60 s (marcatore connected-$UPD1 trovato)" wait_gone /usr/local/bin/monsterterminal-connector.prev 60
	assert "pending.json, applied.json, apply.request assenti dopo il disarmo" mailbox_clean
	assert "la sessione tmux creata prima è ancora viva" tmux_mt has-session -t prima-del-rilascio
	assert "il servizio gira ancora come monsterterminal" hx_sh 'test "$(ps -o user= -p "$(systemctl show -p MainPID --value monsterterminal-connector)")" = monsterterminal'
	assert "journal dell'updater: verificato e installato" hx_sh 'journalctl -u monsterterminal-connector-updater --no-pager | grep -q "aggiornamento verificato dall.updater"'
	assert "journal del ritorno: disarmato" hx_sh 'journalctl -u monsterterminal-connector-rollback --no-pager | grep -q "disarmo"'
	assert "nessun token (mti_/mth_) nei journal dell'updater e del ritorno" hx_sh '! journalctl -u monsterterminal-connector-updater -u monsterterminal-connector-rollback --no-pager | grep -q "mt[ih]_"'
	msg "   update.result finora: $(results_json)"
	verdict "update"
}

cmd_rollback() {
	require_update_bench
	local sha1 sha2
	sha1=$(asset_sha "$UPD1")
	sha2=$(asset_sha "$UPD2")
	[ "$(installed_sha)" = "$sha1" ] || die "il binario installato non è v$UPD1: banco.sh update prima di rollback"
	hx test -e /usr/local/bin/monsterterminal-connector.prev && die "un .prev è ancora presente: attendi il disarmo del giro precedente"

	step "R1 — il backend finto spinge v$UPD2 (hello volutamente rotto): si installa, non si collega"
	push_manifest "$GHDIR/v$UPD2/update-manifest-linux-$ARCH.json" "$KEYS/updates.key"
	assert "binario sostituito con v$UPD2 entro 90 s" wait_binary "$sha2" 90
	assert ".prev presente = v$UPD1" hx_sh "test \"\$(sha256sum /usr/local/bin/monsterterminal-connector.prev | cut -d' ' -f1)\" = $sha1"
	assert "timer di ritorno armato" hx systemctl is-active --quiet monsterterminal-connector-rollback.timer
	assert "journal: il binario di banco si rifiuta di collegarsi" hx_sh 'i=0; while [ $i -lt 20 ]; do journalctl -u monsterterminal-connector --no-pager | grep -q "hello volutamente rotto" && exit 0; i=$((i+1)); sleep 1; done; exit 1'

	step "R2 — nessun welcome entro $ROLLBACK_AFTER: rollback-check (binario .prev, root) ripristina v$UPD1"
	assert "binario tornato a v$UPD1 entro 90 s" wait_binary "$sha1" 90
	assert ".prev consumato dal ripristino" wait_gone /usr/local/bin/monsterterminal-connector.prev 10
	assert "update.result rolled-back v$UPD2 con reason no-welcome entro 60 s" wait_result "$UPD2" rolled-back no-welcome 60
	assert "«version» del binario = $UPD1 e servizio attivo" hx_sh "test \"\$(/usr/local/bin/monsterterminal-connector version)\" = $UPD1 && test \"\$(systemctl is-active monsterterminal-connector)\" = active"
	assert "connector.restarted con reason rollback ($UPD2 → $UPD1) entro 30 s" wait_restart "$UPD2" "$UPD1" rollback 30
	assert "pending.json, applied.json, rolled-back.json assenti dopo il riporto" mailbox_clean
	assert "journal del ritorno: ripristino la versione precedente" hx_sh 'journalctl -u monsterterminal-connector-rollback --no-pager | grep -q "ripristino la versione precedente"'
	assert "la sessione tmux creata prima dell'aggiornamento è ancora viva" tmux_mt has-session -t prima-del-rilascio
	msg "   update.result finora: $(results_json)"
	verdict "rollback"
}

# Un manifest firmato per una versione fittizia: fallisce prima del download (o al download, 404).
fake_manifest() {
	# fake_manifest VERSIONE [SHA] [SECURITY] [URL-ASSET]: in $STATUS/manifests/<versione>.json
	mkdir -p "$STATUS/manifests"
	local sha=${2:-$(asset_sha "$UPD1")}
	make_manifest "$1" "$(asset_name "$1")" "$sha" "${3:-}" >"$STATUS/manifests/$1.json"
	if [ -n "${4:-}" ]; then
		python3 - "$STATUS/manifests/$1.json" "$4" <<'PY'
import json, sys
p, url = sys.argv[1], sys.argv[2]
d = json.load(open(p))
d["url"] = url
open(p, "w").write(json.dumps(d, separators=(",", ":"), sort_keys=True))
PY
	fi
	echo "$STATUS/manifests/$1.json"
}

# Scrive nella cassetta update/ come l'utente del connettore: binario, manifest, firma (file locali
# copiati nel container) e infine apply.request. È ciò che un utente compromesso potrebbe fare.
user_plants() {
	# user_plants VERSIONE MANIFEST-LOCALE FIRMA-LOCALE SORGENTE-BINARIO-NEL-CONTAINER [symlink]
	local v=$1 mf=$2 sig=$3 src=$4 how=${5:-copy}
	docker cp "$mf" "$HOST_C:/tmp/mt-manifest.json"
	docker cp "$sig" "$HOST_C:/tmp/mt-manifest.sig"
	hx chown monsterterminal /tmp/mt-manifest.json /tmp/mt-manifest.sig
	if [ "$how" = symlink ]; then
		hx runuser -u monsterterminal -- sh -c "rm -f /var/lib/monsterterminal/update/binary && ln -s '$src' /var/lib/monsterterminal/update/binary"
	else
		hx runuser -u monsterterminal -- sh -c "cp '$src' /var/lib/monsterterminal/update/binary"
	fi
	hx runuser -u monsterterminal -- sh -c "mv /tmp/mt-manifest.json /var/lib/monsterterminal/update/manifest.json && mv /tmp/mt-manifest.sig /var/lib/monsterterminal/update/manifest.sig && printf '%s\n' '$v' >/var/lib/monsterterminal/update/apply.request"
}

cmd_update_negative() {
	require_update_bench
	local sha1 sha3 before mf sigf
	sha1=$(asset_sha "$UPD1")
	sha3=$(asset_sha "$UPD3")
	[ "$(installed_sha)" = "$sha1" ] || die "il binario installato non è v$UPD1: banco.sh update (e rollback) prima di update-negative"
	hx test -e /usr/local/bin/monsterterminal-connector.prev && die "un .prev è ancora presente: attendi il disarmo del giro precedente"
	before=$(snapshot_host)
	local dl_before
	dl_before=$(binary_downloads)

	step "N1 — firma di un'altra chiave (perfetta, ma non mt-updates del banco) ⇒ failed bad-signature, nessun download"
	push_manifest "$(fake_manifest 0.0.9-banco)" "$KEYS/impostore.key"
	assert "update.result failed bad-signature" wait_result 0.0.9-banco failed bad-signature 30

	step "N2 — firma con la chiave delle RICETTE (§1.17: le due chiavi non si scambiano) ⇒ failed bad-signature"
	push_manifest "$(fake_manifest 0.0.8-banco)" "$KEYS/recipes.key"
	assert "update.result failed bad-signature" wait_result 0.0.8-banco failed bad-signature 30

	step "N3 — downgrade e stessa versione (mai un downgrade da manifest, ADR 0438 D3 punto 5) ⇒ failed not-newer"
	push_manifest "$(fake_manifest "$BANCO_VERSION")" "$KEYS/updates.key"
	assert "v$BANCO_VERSION < v$UPD1: update.result failed not-newer" wait_result "$BANCO_VERSION" failed not-newer 30
	push_manifest "$GHDIR/v$UPD1/update-manifest-linux-$ARCH.json" "$KEYS/updates.key"
	assert "v$UPD1 = in esecuzione: update.result failed not-newer" wait_result "$UPD1" failed not-newer 30
	assert "nessun download finora" test "$(binary_downloads)" = "$dl_before"

	step "N4 — URL fuori dal repository (releases.banco) ⇒ failed bad-url, nessun download"
	push_manifest "$(fake_manifest 0.0.6-banco "" "" "https://releases.banco/releases/download/v$UPD1/$(asset_name "$UPD1")")" "$KEYS/updates.key"
	assert "update.result failed bad-url" wait_result 0.0.6-banco failed bad-url 30
	assert "nessun download" test "$(binary_downloads)" = "$dl_before"

	step "N5 — sha256 diverso dal binario scaricato ⇒ failed sha256-mismatch (il download c'è, l'installazione no)"
	push_manifest "$(fake_manifest 0.0.7-banco "$sha1" "" "$GH_PREFIX/v$UPD2/$(asset_name "$UPD2")")" "$KEYS/updates.key"
	assert "update.result failed sha256-mismatch" wait_result 0.0.7-banco failed sha256-mismatch 60
	assert "un download in più" test "$(binary_downloads)" -gt "$dl_before"
	assert "nessun apply.request lasciato" hx_sh '! test -e /var/lib/monsterterminal/update/apply.request'

	step "N6 — interruttore del backend spento (config.update autoUpdate=false, ADR 0438 D6) ⇒ skipped tenant-switch"
	backend_switch false
	sleep 1
	push_manifest "$(fake_manifest 0.0.5-banco)" "$KEYS/updates.key"
	assert "update.result skipped tenant-switch" wait_result 0.0.5-banco skipped tenant-switch 30
	dl_before=$(binary_downloads)

	step "N7 — manifest con security:true ignora l'interruttore del backend: v$UPD3 si installa davvero"
	push_manifest "$GHDIR/v$UPD3/update-manifest-linux-$ARCH.json" "$KEYS/updates.key"
	assert "binario sostituito con v$UPD3 entro 90 s" wait_binary "$sha3" 90
	assert "update.result ok v$UPD3 entro 60 s" wait_result "$UPD3" ok "" 60
	assert ".prev rimosso entro 60 s" wait_gone /usr/local/bin/monsterterminal-connector.prev 60

	step "N8 — auto_update=no nel file locale dell'host vince anche sulla sicurezza ⇒ skipped host-config, nessun download"
	hx sed -i 's/^auto_update=yes$/auto_update=no/' /etc/monsterterminal/connector.conf
	hx systemctl restart monsterterminal-connector
	assert "il connettore riparte e dichiara autoUpdateEnabled=false" sh -c "i=0; while [ \$i -lt 30 ]; do grep -q '\"autoUpdateEnabled\": false' '$STATUS/status.json' && exit 0; i=\$((i+1)); sleep 1; done; exit 1"
	dl_before=$(binary_downloads)
	push_manifest "$(fake_manifest 0.0.4-banco "" '"security":true,')" "$KEYS/updates.key"
	assert "update.result skipped host-config" wait_result 0.0.4-banco skipped host-config 30
	assert "nessun download" test "$(binary_downloads)" = "$dl_before"
	hx sed -i 's/^auto_update=no$/auto_update=yes/' /etc/monsterterminal/connector.conf
	backend_switch assente
	hx systemctl restart monsterterminal-connector
	assert "il connettore riparte con auto_update=yes" sh -c "i=0; while [ \$i -lt 30 ]; do grep -q '\"autoUpdateEnabled\": true' '$STATUS/status.json' && exit 0; i=\$((i+1)); sleep 1; done; exit 1"

	step "N9 — l'utente del connettore mette in update/ un binario NON firmato (firma di zeri) con un apply.request falso ⇒ l'updater scrive apply.rejected e NON installa (decisione BOSS 2026-11-17)"
	before=$(snapshot_host)
	mf=$(fake_manifest 0.1.0-banco "$(hx sha256sum /bin/true | cut -d' ' -f1)")
	sigf=$STATUS/manifests/zeri.sig
	head -c 64 /dev/zero >"$sigf"
	user_plants 0.1.0-banco "$mf" "$sigf" /bin/true
	assert "apply.rejected con reason bad-signature entro 30 s" hx_sh 'i=0; while [ $i -lt 30 ]; do grep -q "bad-signature" /var/lib/monsterterminal/update/apply.rejected 2>/dev/null && exit 0; i=$((i+1)); sleep 1; done; exit 1'
	assert "update.result failed bad-signature v0.1.0-banco riportato dal connettore entro 60 s" wait_result 0.1.0-banco failed bad-signature 60
	assert "binario, conf, unit e token intatti; nessun .prev" sh -c "test \"$(snapshot_host)\" = \"$before\" && ! docker exec $HOST_C test -e /usr/local/bin/monsterterminal-connector.prev"
	assert "journal dell'updater: rifiutato" hx_sh 'journalctl -u monsterterminal-connector-updater --no-pager | grep -q "aggiornamento rifiutato dall.updater"'

	step "N9b — lo stesso, firmato con la chiave delle ricette ⇒ apply.rejected bad-signature"
	mf=$(fake_manifest 0.1.1-banco "$(hx sha256sum /bin/true | cut -d' ' -f1)")
	sigf=$STATUS/manifests/ricette.sig
	openssl pkeyutl -sign -inkey "$KEYS/recipes.key" -rawin -in "$mf" -out "$sigf"
	user_plants 0.1.1-banco "$mf" "$sigf" /bin/true
	assert "update.result failed bad-signature v0.1.1-banco entro 60 s" wait_result 0.1.1-banco failed bad-signature 60
	assert "macchina intatta" test "$(snapshot_host)" = "$before"

	step "N10 — symlink in update/binary verso il binario di root, manifest firmato con lo sha di quel file ⇒ rifiutato senza seguirlo"
	mf=$(fake_manifest 0.1.2-banco "$(installed_sha)")
	sigf=$STATUS/manifests/symlink.sig
	openssl pkeyutl -sign -inkey "$KEYS/updates.key" -rawin -in "$mf" -out "$sigf"
	user_plants 0.1.2-banco "$mf" "$sigf" /usr/local/bin/monsterterminal-connector symlink
	assert "update.result failed download-failed v0.1.2-banco entro 60 s (symlink non seguito)" wait_result 0.1.2-banco failed download-failed 60
	assert "macchina intatta, nessun .prev, nessuna copia in updater/" sh -c "test \"$(snapshot_host)\" = \"$before\" && ! docker exec $HOST_C test -e /usr/local/bin/monsterterminal-connector.prev && ! docker exec $HOST_C test -e /var/lib/monsterterminal/updater/binary"
	assert "cassetta pulita e nessuna richiesta in attesa" mailbox_clean
	assert "servizio attivo, «version» = $UPD3" hx_sh "test \"\$(systemctl is-active monsterterminal-connector)\" = active && test \"\$(/usr/local/bin/monsterterminal-connector version)\" = $UPD3"
	msg "   update.result finora: $(results_json)"
	verdict "update-negative"
}

# ---------- status / down ----------

cmd_status() {
	[ -f "$STATUS/status.json" ] || die "nessuno status-file: banco.sh up"
	cat "$STATUS/status.json"
}

cmd_down() {
	need docker
	step "Rimuovo container, rete, immagini mt-banco-* e i file di lavoro del banco"
	docker rm -f "$REL_C" "$FAKE_C" "$HOST_C" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	docker rmi "$HOST_IMG" "$FAKE_IMG" >/dev/null 2>&1 || true
	if [ "${1:-}" = --images ]; then
		docker rmi ubuntu:24.04 alpine:3.20 "$NGINX_IMAGE" "$GO_IMAGE" >/dev/null 2>&1 || true
		msg "immagini di base rimosse (ubuntu, alpine, nginx, golang)"
	fi
	if [ -f "$MARKER" ]; then
		# solo le cartelle che «up» ha creato, sotto una BANCO_HOME marcata dal banco stesso
		rm -rf "${KEYS:?}" "${CERTS:?}" "${DIST:?}" "${WWW:?}" "${STATUS:?}" "${SRCCOPY:?}" "${MARKER:?}"
		rmdir "$BANCO_HOME" 2>/dev/null || true
	fi
	msg "fatto: resta la cache go in $GO_CACHE"
	if docker ps -a --format '{{.Names}}' | grep -q '^mt-banco-'; then
		die "container mt-banco-* ancora presenti"
	fi
	ok "nessun container mt-banco-*"
}

case "${1:-}" in
up) cmd_up ;;
install) cmd_install ;;
negative) cmd_negative ;;
spazi) cmd_spazi ;;
update) cmd_update ;;
rollback) cmd_rollback ;;
update-negative) cmd_update_negative ;;
uninstall) cmd_uninstall ;;
status) cmd_status ;;
all)
	cmd_up
	fails=0
	cmd_negative
	fails=0
	cmd_install
	fails=0
	cmd_spazi
	fails=0
	cmd_update
	fails=0
	cmd_rollback
	fails=0
	cmd_update_negative
	fails=0
	cmd_uninstall
	msg ""
	msg "== all: verde. Il banco è ancora in piedi: banco.sh down per smontarlo"
	;;
down) cmd_down "${2:-}" ;;
*)
	sed -n '2,28p' "$0" >&2
	exit 2
	;;
esac
