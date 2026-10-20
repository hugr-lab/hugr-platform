#!/usr/bin/env bash
# The console end to end (spec 003), without Kubernetes: Keycloak over https (a realm with the console's public
# client, an operator and an analyst, passwords made for this run), a duckdb-acl node trusting it by discovery and
# serving Flight over TLS under its group's name, the console's own server over a static fleet (test/console/serve)
# with the web build, then Playwright (web/console/e2e) signing both people in with a real browser. One CA of this run
# signs Keycloak's, the node's and the console's certificates. Screenshots land in E2E_SHOTS when it is set.
#
#   DUCKDB_ACL=<a duckdb-acl checkout, built> test/console/e2e/run.sh
#
# E2E_SERVE=<dir> keeps the stand up for a person instead of Playwright: the console at https://127.0.0.1:18644, its
# people's sign-in (admin: passthrough; ops: observe and operate; analyst: nothing) and the run's CA written to <dir> (mode 0700, the passwords 0600, never printed),
# until the run is stopped (Ctrl-C, or a kill of its process).
set -euo pipefail
unset AI_AGENT CLAUDECODE # duckdb's CLI answers an agent in another format
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
WEB="$ROOT/web/console"
ACL="${DUCKDB_ACL:?DUCKDB_ACL is a duckdb-acl checkout, built}"
DUCKDB="$ACL/build/release/duckdb"
ACL_EXT="$ACL/build/release/extension/acl/acl.duckdb_extension"
KC_PORT="${E2E_KC_PORT:-18691}"
NODE_PORT="${E2E_NODE_PORT:-41102}"
CONSOLE_PORT="${E2E_CONSOLE_PORT:-18644}"
KC="https://127.0.0.1:$KC_PORT"
REALM="$KC/realms/hugr"
CONSOLE="https://127.0.0.1:$CONSOLE_PORT"
WORK="$(mktemp -d)"
NAME=hugr-console-e2e-kc
PIDS=()
cleanup() {
	exec 3>&- 2>/dev/null || true
	for p in ${PIDS[@]+"${PIDS[@]}"}; do pkill -P "$p" 2>/dev/null || true; kill "$p" 2>/dev/null || true; done
	[ -z "${E2E_KEEP:-}" ] && docker rm -f "$NAME" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT
fail() {
	echo "FAIL: $*" >&2
	for f in "$WORK"/*.log; do [ -f "$f" ] && { echo "--- $(basename "$f")" >&2; tail -25 "$f" >&2; }; done
	exit 1
}
for port in $KC_PORT $NODE_PORT $CONSOLE_PORT; do
	! (echo >"/dev/tcp/127.0.0.1/$port") 2>/dev/null || fail "port $port is taken"
done

# one CA of this run, three certificates
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=e2e CA" \
	-keyout "$WORK/ca.key" -out "$WORK/ca.crt" 2>/dev/null
cert() { # <name> <subjectAltName>
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$1" \
		-keyout "$WORK/$1.key" -out "$WORK/$1.csr" 2>/dev/null
	printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\n' "$2" >"$WORK/$1.ext"
	openssl x509 -req -in "$WORK/$1.csr" -CA "$WORK/ca.crt" -CAkey "$WORK/ca.key" -CAcreateserial -days 1 \
		-extfile "$WORK/$1.ext" -out "$WORK/$1.crt" 2>/dev/null
}
cert kc "IP:127.0.0.1"
cert console "IP:127.0.0.1"
cert node "DNS:hugr-node-etl.hugr.svc"
# Keycloak's own, readable by its container's user (uid 1000): a directory of its own, the run's (mode 0700) holding
# the rest
mkdir -m 755 "$WORK/kc"
cp "$WORK/kc.crt" "$WORK/kc.key" "$WORK/kc/"
chmod 644 "$WORK/kc/kc.crt" "$WORK/kc/kc.key"

# passwords for this run only: never printed
KC_ADMIN_PW="$(openssl rand -hex 16)"
OPS_PW="$(openssl rand -hex 16)"
ANALYST_PW="$(openssl rand -hex 16)"
ADMIN_PW="$(openssl rand -hex 16)"

echo "e2e: Keycloak"
CONSOLE="$CONSOLE" OPS_PW="$OPS_PW" ANALYST_PW="$ANALYST_PW" ADMIN_PW="$ADMIN_PW" python3 - "$WORK/realm.json" <<'PY'
import json, os, sys
console = os.environ["CONSOLE"]
user = lambda name, pw, role: {"username": name, "enabled": True, "email": f"{name}@example.test", "emailVerified": True,
    "firstName": name.capitalize(), "lastName": "E2e", "credentials": [{"type": "password", "value": pw, "temporary": False}],
    "realmRoles": [role, "offline_access"]}  # the console asks offline_access for its refresh token
realm = {
    "realm": "hugr", "enabled": True,
    "roles": {"realm": [{"name": "platform_ops"}, {"name": "analyst"}, {"name": "platform_admin"}]},
    "clients": [{
        "clientId": "hugr-console", "publicClient": True, "standardFlowEnabled": True, "directAccessGrantsEnabled": False,
        "redirectUris": [f"{console}/*"], "webOrigins": [console],
        "attributes": {"pkce.code.challenge.method": "S256", "post.logout.redirect.uris": f"{console}/*"},
        "protocolMappers": [{"name": "aud", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
            "config": {"included.custom.audience": "hugr-nodes", "access.token.claim": "true", "id.token.claim": "false"}}],
    }, {
        # the run's own client: the analyst's token for a session of its own on the node, by the password grant
        "clientId": "e2e-probe", "publicClient": True, "standardFlowEnabled": False, "directAccessGrantsEnabled": True,
        "protocolMappers": [{"name": "aud", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
            "config": {"included.custom.audience": "hugr-nodes", "access.token.claim": "true", "id.token.claim": "false"}}],
    }],
    "users": [user("ops", os.environ["OPS_PW"], "platform_ops"), user("analyst", os.environ["ANALYST_PW"], "analyst"),
              # the stand's third person: passthrough, so every page (scale, restart, logs) - Playwright signs in the two
              user("admin", os.environ["ADMIN_PW"], "platform_admin")],
}
json.dump(realm, open(sys.argv[1], "w"))
PY
mv "$WORK/realm.json" "$WORK/kc/realm.json" && chmod 644 "$WORK/kc/realm.json"
# the admin's password by a file, never on a command line
(umask 077 && printf 'KC_BOOTSTRAP_ADMIN_USERNAME=admin\nKC_BOOTSTRAP_ADMIN_PASSWORD=%s\n' "$KC_ADMIN_PW" >"$WORK/kc.env")
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" -p "127.0.0.1:$KC_PORT:8443" --env-file "$WORK/kc.env" \
	-v "$WORK/kc/realm.json:/opt/keycloak/data/import/realm.json:ro" -v "$WORK/kc:/certs:ro" \
	quay.io/keycloak/keycloak:26.4 start-dev --import-realm \
	--https-certificate-file=/certs/kc.crt --https-certificate-key-file=/certs/kc.key >/dev/null
for _ in $(seq 90); do curl -sf --cacert "$WORK/ca.crt" "$REALM/.well-known/openid-configuration" >/dev/null && break; sleep 2; done
curl -sf --cacert "$WORK/ca.crt" "$REALM/.well-known/openid-configuration" >/dev/null || { docker logs "$NAME" | tail -30; fail "Keycloak"; }

echo "e2e: the node"
cat >"$WORK/node.sql" <<SQL
LOAD '$ACL_EXT';
SET GLOBAL ca_cert_file = '$WORK/ca.crt';
ATTACH ':memory:' AS phys;
CREATE TABLE phys.main.t AS SELECT 1 AS v;
ATTACH ':memory:' AS store;
SELECT acl_use_db('store', 'acl', true);
SET GLOBAL acl_allow_anonymous_admin = true;
SET GLOBAL acl_jwks_locations = '$KC/';
ACL ADMIN CREATE ISSUER '$REALM' AUDIENCES ('hugr-nodes') ROLE CLAIM 'realm_access.roles';
ACL ADMIN CREATE VIRTUAL CATALOG c;
ACL ADMIN CREATE VIRTUAL TABLE c.t AS phys.main.t;
ACL ADMIN CREATE ROLE analyst;
ACL ADMIN CREATE ROLE ops;
ACL ADMIN GRANT CATALOG c TO ROLE analyst WITH (select) MAIN;
ACL ADMIN MAP CLAIM 'platform_ops' FROM CLIENT '$REALM' TO ROLE ops;
ACL ADMIN GRANT ADMIN observe TO ROLE ops;
ACL ADMIN GRANT ADMIN operate TO ROLE ops;
ACL ADMIN GRANT FUNCTION CATEGORY meta TO ROLE ops;
ACL ADMIN GRANT FUNCTION CATEGORY environment TO ROLE ops;
ACL ADMIN CREATE ROLE admin;
ACL ADMIN MAP CLAIM 'platform_admin' FROM CLIENT '$REALM' TO ROLE admin;
ACL ADMIN GRANT ADMIN passthrough TO ROLE admin;
ACL ADMIN GRANT FUNCTION CATEGORY meta TO ROLE admin;
ACL ADMIN GRANT FUNCTION CATEGORY environment TO ROLE admin;
SET GLOBAL acl_allow_anonymous_admin = false;
SET GLOBAL acl_node_label = 'hugr-node-etl-a';
SELECT acl_flight_serve('grpc+tls://127.0.0.1:$NODE_PORT', '$WORK/node.crt', '$WORK/node.key');
SQL
mkfifo "$WORK/node.in"
"$DUCKDB" -unsigned <"$WORK/node.in" >"$WORK/node.log" 2>&1 &
PIDS+=($!)
exec 3>"$WORK/node.in"
cat "$WORK/node.sql" >&3
for _ in $(seq 100); do (echo >"/dev/tcp/127.0.0.1/$NODE_PORT") 2>/dev/null && break; sleep 0.1; done
(echo >"/dev/tcp/127.0.0.1/$NODE_PORT") 2>/dev/null || fail "the node does not serve"

echo "e2e: the console"
(cd "$WEB" && npm run build >/dev/null) || fail "the web build"
(cd "$ROOT" && GOWORK=off go build -o "$WORK/serve" ./test/console/serve) || fail "the console does not build"
"$WORK/serve" -listen "127.0.0.1:$CONSOLE_PORT" -cert "$WORK/console.crt" -key "$WORK/console.key" -ca "$WORK/ca.crt" \
	-node "etl=127.0.0.1:$NODE_PORT" -issuer "$REALM" -client-id hugr-console -audience hugr-nodes -dist "$WEB/dist" \
	>"$WORK/console.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do curl -sf --cacert "$WORK/ca.crt" "$CONSOLE/api/v1/console/config" >/dev/null && break; sleep 0.2; done
curl -sf --cacert "$WORK/ca.crt" "$CONSOLE/api/v1/console/config" >/dev/null || fail "the console does not serve"

echo "e2e: the analyst's own session on the node"
(umask 077 && printf '%s' "$ANALYST_PW" >"$WORK/analyst.pw") # by a file: never in curl's argv
curl -sf --cacert "$WORK/ca.crt" "$REALM/protocol/openid-connect/token" -d grant_type=password -d client_id=e2e-probe \
	-d username=analyst --data-urlencode "password@$WORK/analyst.pw" |
	python3 -c 'import json, sys; print(json.load(sys.stdin)["access_token"])' >"$WORK/analyst.token" || fail "the analyst's token"
(cd "$ROOT" && GOWORK=off go build -o "$WORK/probe" ./test/console/local) || fail "the probe does not build"
"$WORK/probe" -keep -node "etl=127.0.0.1:$NODE_PORT" -ca "$WORK/ca.crt" -token-file "$WORK/analyst.token" \
	"SELECT * FROM c.t" "SELECT * FROM c.t" >"$WORK/probe.json" || fail "the analyst's session"

# leaked: a bearer token (a JWT) or a password of this run in the console's or the node's log
leaked() {
	local log
	for log in "$WORK/console.log" "$WORK/node.log"; do
		if grep -qF -e "$OPS_PW" -e "$ANALYST_PW" -e "$ADMIN_PW" -e "$KC_ADMIN_PW" "$log" ||
			grep -qE 'eyJ[A-Za-z0-9_-]{8,}\.eyJ' "$log"; then
			echo "$(basename "$log")"
		fi
	done
}

if [ -n "${E2E_SERVE:-}" ]; then
	# a directory of the stand's own: one that is there already keeps its mode, the sign-in file is 0600 anyway
	[ -d "$E2E_SERVE" ] || mkdir -m 700 "$E2E_SERVE"
	trap 'rm -f "$E2E_SERVE/sign-in"; l="$(leaked)"; [ -z "$l" ] || echo "FAIL: a token or a password reached $l" >&2; cleanup' EXIT
	cp "$WORK/ca.crt" "$E2E_SERVE/ca.crt"
	(umask 077 && printf 'console  %s\nadmin    %s\nops      %s\nanalyst  %s\n' "$CONSOLE" "$ADMIN_PW" "$OPS_PW" \
		"$ANALYST_PW" >"$E2E_SERVE/sign-in")
	echo "e2e: the stand serves: $CONSOLE (Keycloak $KC); the sign-in in $E2E_SERVE/sign-in, the CA in $E2E_SERVE/ca.crt"
	# until stopped, or until the node or the console ends (said, not waited on forever)
	while true; do
		for p in "${PIDS[@]}"; do
			kill -0 "$p" 2>/dev/null || fail "the stand's $( [ "$p" = "${PIDS[0]}" ] && echo node || echo console) ended"
		done
		sleep 2
	done
fi

echo "e2e: Playwright"
status=0
(cd "$WEB" && E2E_CONSOLE_URL="$CONSOLE" E2E_OPS_PW="$OPS_PW" E2E_ANALYST_PW="$ANALYST_PW" E2E_SHOTS="${E2E_SHOTS:-}" \
	npx playwright test) || status=$?
# nothing secret in the logs
l="$(leaked)"
[ -z "$l" ] || fail "a token or a password reached $l"
[ "$status" -eq 0 ] || fail "Playwright"
echo "PASS: the console end to end"
