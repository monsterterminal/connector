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
#   banco.sh uninstall  --uninstall --purge ⇒ servizio, binario, configurazione e stato assenti
#   banco.sh status     lo status-file del backend finto
#   banco.sh all        up, negative, install, spazi, uninstall (lascia il banco in piedi per guardarlo)
#   banco.sh down       rimuove container, rete, immagini mt-banco-* e i file di lavoro (non la cache go)
#
# Variabili: BANCO_HOME (default ~/mt-banco), BANCO_VERSION (0.0.0-banco), BANCO_ASSETS (cartella con
# gli asset di una release già compilata: binario e install.sh, al posto della build locale),
# BANCO_GO_IMAGE (golang:1.27), BANCO_GO_CACHE (~/.cache/go-banco), BANCO_CGROUPNS (private|host).
# Tutto ciò che il banco crea sulla macchina si chiama mt-banco-*.
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
RELDIR=$WWW/releases/download/v$BANCO_VERSION
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
	local name
	for name in releases.banco connect.banco; do
		openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
			-subj "/CN=$name" -addext "subjectAltName=DNS:$name" \
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
	# (il sorgente copiato non ha .git) e con la cache dei moduli e dei build riusata fra i giri
	mkdir -p "$GO_CACHE/mod" "$GO_CACHE/build"
	docker run --rm --user "$(id -u):$(id -g)" \
		-e HOME=/tmp -e GOCACHE=/gocache -e GOMODCACHE=/go/pkg/mod -e GOFLAGS=-buildvcs=false \
		-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH="$ARCH" \
		-v "$SRC:/src:ro" -v "$GO_CACHE/mod:/go/pkg/mod" -v "$GO_CACHE/build:/gocache" \
		-v "$DIST:/out" -w /src "$GO_IMAGE" sh -c "$1"
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
		step "Build riproducibile del connettore (VERSION=$BANCO_VERSION, $ARCH) e del backend finto con $GO_IMAGE"
		load_note
		go_in_docker "go build -trimpath -ldflags '-s -w -buildid= -X main.version=$BANCO_VERSION' -o /out/$ASSET ./cmd/monsterterminal-connector \
			&& go build -trimpath -o /out/mt-fakeserver ./cmd/mt-fakeserver"
		sed "s/@VERSION@/$BANCO_VERSION/" "$SRC/packaging/install.sh" >"$DIST/install.sh.orig"
	fi
	chmod 0755 "$DIST/$ASSET" "$DIST/mt-fakeserver"
	msg "binario: $(sha256sum "$DIST/$ASSET")"
}

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
}

start_containers() {
	step "Rete $NET e container: nginx (releases.banco), backend finto (connect.banco), host systemd"
	docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
	docker rm -f "$REL_C" "$FAKE_C" "$HOST_C" >/dev/null 2>&1 || true
	docker run -d --name "$REL_C" --network "$NET" --network-alias releases.banco \
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
		-other-tokens 1 -other-token-file /status/other-tokens >/dev/null

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
	(umask 077 && printf 'MONSTERTERMINAL_INSTALL_TOKEN=%s\n' "$tok" >"$envf")
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

	step "B7 — secondo install.sh senza --replace: registrazione saltata"
	rc=0
	run_install "$tok" "$STATUS/install-2.log" || rc=$?
	if [ "$rc" -eq 0 ]; then ok "install.sh è uscito con 0"; else fail "install.sh è uscito con $rc"; fi
	assert "«Host già registrato» stampato" grep -q "Host già registrato" "$STATUS/install-2.log"
	assert "status-file: ancora 1 registrazione" test "$(status_field registrations)" = 1
	assert "servizio ancora attivo" hx_sh 'test "$(systemctl is-active monsterterminal-connector)" = active'

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
# file, servizio o token.
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
	run_install "$(token_n 2)" "$STATUS/spazi-3.log" --user root --i-accept-root || rc=$?
	if [ "$rc" -ne 0 ]; then ok "install.sh si ferma (uscita $rc)"; else fail "install.sh è uscito con 0"; fi
	assert "messaggio: già installato per l'utente monsterterminal" grep -q "già installato per l'utente monsterterminal" "$STATUS/spazi-3.log"

	after=$(snapshot_host)
	if [ "$before" = "$after" ]; then ok "binario, configurazione, unit e token dell'host intatti"; else fail "la macchina è cambiata: $after"; fi
	assert "nessuna registrazione nuova dal backend" test "$(status_field registrations)" = "$regs"
	assert "servizio ancora attivo" hx systemctl is-active --quiet monsterterminal-connector

	step "S4 — lo stesso spazio, comando normale: aggiorna senza registrare"
	rc=0
	run_install "$(token_n 2)" "$STATUS/spazi-4.log" || rc=$?
	if [ "$rc" -eq 0 ]; then ok "install.sh è uscito con 0"; else fail "install.sh è uscito con $rc"; fi
	assert "messaggio: registrato per questo spazio, aggiorno" grep -q "già registrato per questo spazio di lavoro" "$STATUS/spazi-4.log"
	assert "nessuna registrazione nuova dal backend" test "$(status_field registrations)" = "$regs"
	assert "token dell'host invariato" test "$(hx sha256sum /var/lib/monsterterminal/host-token)" = "$(printf '%s\n' "$before" | grep host-token)"
	verdict "spazi"
}

snapshot_host() {
	hx_sh 'for f in /usr/local/bin/monsterterminal-connector /etc/monsterterminal/connector.conf /etc/systemd/system/monsterterminal-connector.service /var/lib/monsterterminal/host-token; do if [ -e "$f" ]; then sha256sum "$f"; else echo "assente $f"; fi; done'
}
downloads_count() { docker logs "$REL_C" 2>&1 | grep -c 'GET /releases/download/' || true; }
binary_downloads() { docker logs "$REL_C" 2>&1 | grep -c "GET /releases/download/.*/monsterterminal-connector_" || true; }

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
		rm -rf "${KEYS:?}" "${CERTS:?}" "${DIST:?}" "${WWW:?}" "${STATUS:?}" "${MARKER:?}"
		rmdir "$BANCO_HOME" 2>/dev/null || true
	fi
	msg "fatto: restano la cache go in $GO_CACHE e, se presente, $BANCO_HOME/src"
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
	cmd_uninstall
	msg ""
	msg "== all: verde. Il banco è ancora in piedi: banco.sh down per smontarlo"
	;;
down) cmd_down "${2:-}" ;;
*)
	sed -n '2,20p' "$0" >&2
	exit 2
	;;
esac
