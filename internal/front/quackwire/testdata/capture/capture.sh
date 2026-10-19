#!/usr/bin/env bash
# Rebuilds the quack corpus (spec 002's unit tests): a node of duckdb-acl serving quack in the clear on localhost,
# proxy.py in front of it keeping every body, and duckdb's own quack client. The corpus is committed; this script
# is how it was made, and how it is made again when quack's wire changes.
#
#   DUCKDB_ACL=<a duckdb-acl checkout built with ACL_QUACK=1> ./capture.sh
set -euo pipefail
unset AI_AGENT CLAUDECODE # duckdb's CLI answers an agent in another format

HERE="$(cd "$(dirname "$0")" && pwd)"
ACL="${DUCKDB_ACL:?DUCKDB_ACL is a duckdb-acl checkout built with ACL_QUACK=1}"
BUILD="$ACL/build/release"
DUCKDB="$BUILD/duckdb"
ACL_EXT="$BUILD/extension/acl/acl.duckdb_extension"
SOURCE_ID="$("$DUCKDB" -noheader -csv -c "SELECT source_id FROM pragma_version();")"
QUACK_EXT="$(ls "$BUILD"/repository/"$SOURCE_ID"/*/quack.duckdb_extension | head -1)"
PORT="${CAPTURE_PORT:-31790}"
PROXY_PORT=$((PORT + 1))
WORK="$(mktemp -d)"
OUT="$WORK/out"
mkdir -p "$OUT"
cleanup() { kill "${SERVER_PID:-}" "${PROXY_PID:-}" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT

# the fixture issuer test/idp/s of duckdb-acl, read relative to its root; the token is minted, never printed
cd "$ACL"
TOKEN="$(python3 test/scripts/idp_fixtures.py mint \
	'{"iss": "test/idp/s", "aud": "api://acl-test", "exp": 4102444800, "sub": "capture", "roles": ["analyst"]}')"

mkfifo "$WORK/server"
"$DUCKDB" -unsigned <"$WORK/server" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
exec 3>"$WORK/server"
cat >&3 <<SQL
LOAD '$ACL_EXT'; LOAD '$QUACK_EXT';
ATTACH ':memory:' AS phys;
CREATE TABLE phys.main.big AS SELECT range AS id FROM range(2000000);
ATTACH ':memory:' AS store;
SELECT acl_use_db('store', 'acl', true);
SET GLOBAL acl_allow_anonymous_admin = true;
SET GLOBAL acl_jwks_locations = 'test/idp/';
SELECT acl_define_issuer('test/idp/s', '{"url": "test/idp/s", "client": {"audiences": ["api://acl-test"], "roles_from": ["roles"]}}');
ACL ADMIN CREATE VIRTUAL CATALOG c;
ACL ADMIN CREATE VIRTUAL TABLE c.big AS phys.main.big;
ACL ADMIN CREATE ROLE analyst;
ACL ADMIN GRANT CATALOG c TO ROLE analyst WITH (select) MAIN;
ACL ADMIN MAP CLAIM 'analyst' FROM CLIENT 'test/idp/s' TO ROLE analyst;
SET GLOBAL acl_quack_target_batch_bytes = 65536;
SET GLOBAL acl_allow_anonymous_admin = false;
SET GLOBAL acl_node_label = 'node-a';
SELECT acl_quack_serve('quack:localhost:$PORT', 'capture-server-token');
SQL
ready=""
for _ in $(seq 1 60); do
	if echo "LOAD '$QUACK_EXT'; SELECT * FROM quack_query('quack:localhost:$PORT', 'SELECT 1', token := '$TOKEN');" |
		"$DUCKDB" -unsigned >/dev/null 2>"$WORK/probe.err"; then
		ready=1
		break
	fi
	sleep 0.5
done
[ -n "$ready" ] || { cat "$WORK/server.log" "$WORK/probe.err" >&2; echo "capture: the node never served" >&2; exit 1; }

python3 "$HERE/proxy.py" "$PROXY_PORT" "localhost:$PORT" "$OUT" &
PROXY_PID=$!
sleep 1

# the client: a connect, a read to its end, a read stopped early (CANCEL), heartbeats, a disconnect - and a connect
# with a token that does not verify (the node's refusal)
{
	echo "LOAD '$QUACK_EXT';"
	echo "SET quack_enable_reconnects = true;" # a scan that read to its end ACKs it
	echo "ATTACH 'quack:localhost:$PROXY_PORT' AS r (TYPE quack, TOKEN '$TOKEN', HEARTBEAT_TIMEOUT 3);"
	echo "SELECT count(*) FROM r.main.big;"
	# a join with a local table keeps the scan on the client's side: the whole read is FETCHed, and a LIMIT over
	# the join stops it early (CANCEL)
	echo "CREATE TABLE l AS SELECT 1 AS k;"
	echo "SELECT count(*) FROM r.main.big b, l WHERE b.id >= l.k;"
	echo "SELECT b.id FROM r.main.big b, l WHERE b.id >= l.k LIMIT 1;"
	echo "SET VARIABLE cid = quack_connection_id('r');"
	echo "SELECT * FROM quack_cancel('r', getvariable('cid'));"
	sleep 3
	echo "DETACH r;"
	echo "ATTACH 'quack:localhost:$PROXY_PORT' AS bad (TYPE quack, TOKEN 'not-a-token');"
} | "$DUCKDB" -unsigned >"$WORK/client.log" 2>&1 || true
sleep 1

# one file per message type and direction, the first seen, under a stable name
python3 - "$OUT" "$HERE/.." <<'PY'
import os, re, sys
src, dst = sys.argv[1], sys.argv[2]
names = {1: "connect", 2: "connect_answer", 3: "prepare", 4: "prepare_answer", 7: "fetch", 8: "fetch_answer",
         10: "success", 11: "disconnect", 12: "cancel", 15: "ack", 16: "heartbeat", 100: "error"}
seen = set()
for f in sorted(os.listdir(src)):
    parts = f[:-4].split("-")
    direction, kind = "_".join(parts[1:-1]), int(parts[-1])
    key = "%s_%s" % (direction, names.get(kind, "type%d" % kind))
    if key not in seen:
        seen.add(key)
        data = open(os.path.join(src, f), "rb").read()
        # a token is blanked to its own length (the length prefix stays right): the corpus carries none
        data = re.sub(rb"eyJ[A-Za-z0-9_.-]+", lambda m: b"x" * len(m.group(0)), data)
        open(os.path.join(dst, key + ".bin"), "wb").write(data)
PY
