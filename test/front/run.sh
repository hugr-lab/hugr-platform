#!/usr/bin/env bash
# The front's mechanics against real nodes (spec 002), without Kubernetes: two nodes of duckdb-acl, labelled node-1
# and node-2, each serving Flight and quack in the clear on localhost, the front's proxies over a static fleet
# (test/front/local), and the real clients - the ADBC Flight SQL driver and duckdb's own quack client. Each node's
# table `t` holds its label, so every read says which node answered.
#
#   DUCKDB_ACL=<a duckdb-acl checkout built with ACL_QUACK=1> test/front/run.sh
set -euo pipefail
unset AI_AGENT CLAUDECODE # duckdb's CLI answers an agent in another format
HERE="$(cd "$(dirname "$0")" && pwd)"
ACL="${DUCKDB_ACL:?DUCKDB_ACL is a duckdb-acl checkout built with ACL_QUACK=1}"
BUILD="$ACL/build/release"
DUCKDB="$BUILD/duckdb"
ACL_EXT="$BUILD/extension/acl/acl.duckdb_extension"
SOURCE_ID="$("$DUCKDB" -noheader -csv -c "SELECT source_id FROM pragma_version();")"
QUACK_EXT="$(ls "$BUILD"/repository/"$SOURCE_ID"/*/quack.duckdb_extension | head -1)"
BASE="${FRONT_E2E_PORT:-41010}"
WORK="$(mktemp -d)"
PIDS=()
cleanup() {
	exec 3>&- 4>&-
	for p in "${PIDS[@]}"; do pkill -P "$p" 2>/dev/null || true; kill "$p" 2>/dev/null || true; done
	rm -rf "$WORK"
}
trap cleanup EXIT
fail() {
	echo "FAIL: $*" >&2
	for f in "$WORK"/*.log; do echo "--- $f" >&2; tail -20 "$f" >&2; done
	exit 1
}

for port in $BASE $((BASE + 1)) $((BASE + 2)) $((BASE + 10)) $((BASE + 11)) $((BASE + 12)); do
	# a process left on a port would answer for the front or a node under test
	! (echo >"/dev/tcp/127.0.0.1/$port") 2>/dev/null || fail "port $port is taken (FRONT_E2E_PORT moves them)"
done

cd "$ACL" # the fixture issuer test/idp/s is read relative to its root; the token is minted, never printed
TOKEN="$(python3 test/scripts/idp_fixtures.py mint \
	'{"iss": "test/idp/s", "aud": "api://acl-test", "exp": 4102444800, "sub": "front-e2e", "roles": ["analyst"]}')"

serve() { # <label> <flight port> <quack port> <fd>
	cat >"$WORK/$1.sql" <<SQL
LOAD '$ACL_EXT'; LOAD '$QUACK_EXT';
ATTACH ':memory:' AS phys;
CREATE TABLE phys.main.t AS SELECT '$1' AS v;
CREATE TABLE phys.main.big AS SELECT range AS id FROM range(2000000);
ATTACH ':memory:' AS store;
SELECT acl_use_db('store', 'acl', true);
SET GLOBAL acl_allow_anonymous_admin = true;
SET GLOBAL acl_jwks_locations = 'test/idp/';
SELECT acl_define_issuer('test/idp/s', '{"url": "test/idp/s", "client": {"audiences": ["api://acl-test"], "roles_from": ["roles"]}}');
ACL ADMIN CREATE VIRTUAL CATALOG c;
ACL ADMIN CREATE VIRTUAL TABLE c.t AS phys.main.t;
ACL ADMIN CREATE VIRTUAL TABLE c.big AS phys.main.big;
ACL ADMIN CREATE ROLE analyst;
ACL ADMIN GRANT CATALOG c TO ROLE analyst WITH (select) MAIN;
ACL ADMIN MAP CLAIM 'analyst' FROM CLIENT 'test/idp/s' TO ROLE analyst;
ACL ADMIN GRANT FUNCTION current_setting TO ROLE analyst;
ACL ADMIN GRANT FUNCTION sum TO ROLE analyst;
ACL ADMIN GRANT FUNCTION count_star TO ROLE analyst;
SET GLOBAL acl_allow_anonymous_admin = false;
SET GLOBAL acl_node_label = '$1';
SELECT acl_flight_serve('grpc://localhost:$2');
SELECT acl_quack_serve('quack:127.0.0.1:$3', 'front-e2e-server-token');
SQL
	mkfifo "$WORK/$1.in"
	"$DUCKDB" -unsigned <"$WORK/$1.in" >"$WORK/$1.log" 2>&1 &
	PIDS+=($!)
	eval "exec $4>\"\$WORK/\$1.in\"" # held open (fd $4) until the run ends: the node serves until then
	cat "$WORK/$1.sql" >&"$4"
}
serve node-1 $((BASE + 1)) $((BASE + 11)) 3
serve node-2 $((BASE + 2)) $((BASE + 12)) 4
(cd "$HERE/../.." && GOWORK=off go build -o "$WORK/front" ./test/front/local) || fail "the local front does not build"
"$WORK/front" -flight "127.0.0.1:$BASE" -quack "127.0.0.1:$((BASE + 10))" \
	-node "node-1=127.0.0.1:$((BASE + 1)):$((BASE + 11))" -node "node-2=127.0.0.1:$((BASE + 2)):$((BASE + 12))" \
	>"$WORK/front.log" 2>&1 &
PIDS+=($!)

for port in $((BASE + 11)) $((BASE + 12)); do
	ready=""
	for _ in $(seq 1 120); do
		if echo "LOAD '$QUACK_EXT'; SELECT * FROM quack_query('quack:localhost:$port', 'SELECT 1', token := '$TOKEN');" |
			"$DUCKDB" -unsigned >/dev/null 2>&1; then
			ready=1 && break
		fi
		sleep 0.5
	done
	[ -n "$ready" ] || fail "the node on $port never served"
done
for _ in $(seq 1 60); do grep -q '"serving"' "$WORK/front.log" 2>/dev/null && break; sleep 0.5; done

echo "# Flight SQL through the front"
python3 "$HERE/flight_client.py" "grpc://127.0.0.1:$BASE" "$TOKEN" || fail "the Flight client"

echo "# quack through the front"
quack_client() {
	{
		echo "LOAD '$QUACK_EXT';"
		echo "ATTACH 'quack:localhost:$((BASE + 10))' AS r (TYPE quack, TOKEN '$TOKEN');"
		for _ in 1 2 3 4 5; do echo "SELECT v FROM r.main.t;"; done
		echo "CREATE TABLE l AS SELECT 1 AS k;"
		echo "SELECT count(*) FROM r.main.big b, l WHERE b.id >= l.k;" # FETCHes through the front
		echo "SELECT b.id FROM r.main.big b, l WHERE b.id >= l.k LIMIT 1;"
		echo "DETACH r;"
	} | "$DUCKDB" -unsigned -noheader -csv 2>&1
}
seen=""
for c in 1 2 3 4 5 6; do
	out="$(quack_client)" || fail "quack client $c: $out"
	labels="$(echo "$out" | grep -E '^node-[12]$' | sort -u)"
	[ "$(echo "$labels" | wc -l | tr -d ' ')" = 1 ] || fail "quack client $c: answers from several nodes ($labels): $out"
	echo "$out" | grep -qx 1999999 || fail "quack client $c: the joined read: $out"
	echo "ok - quack client $c: every message on $(echo "$labels")"
	seen="$seen $labels"
done
for n in node-1 node-2; do
	echo "$seen" | grep -q "$n" || echo "note: no quack client was seated on $n (two random choices)"
done

echo "# the front's log names no token, cookie or connection id"
if grep -Eq 'eyJ|acl_flight_session_id|acl_node=|node-[12]\.[0-9A-F]{8}' "$WORK/front.log"; then
	fail "the front logged a token, a cookie or a connection id"
fi
calls="$(grep -c '"msg":"call"' "$WORK/front.log" || true)"
[ "${calls:-0}" -gt 0 ] || fail "the front logged no call: the check above saw nothing ($(wc -l <"$WORK/front.log") lines)"
echo "ok - the log is clean ($calls calls)"
echo "PASS: the front against real nodes"
