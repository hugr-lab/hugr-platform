#!/usr/bin/env bash
# The console's node calls against a real node (spec 003), without Kubernetes: a duckdb-acl node serving Flight over
# TLS under its group's name (hugr-node-etl.hugr.svc, a CA made here), and the console's own Pool
# (test/console/local) running the statements the console runs, with the tokens of an operator and of an analyst. It
# checks what the API answers is what the node answers: the rights, the load report, the sessions, drain and resume,
# a session ended, and the refusals.
#
#   DUCKDB_ACL=<a duckdb-acl checkout, built> test/console/run.sh
set -euo pipefail
unset AI_AGENT CLAUDECODE # duckdb's CLI answers an agent in another format
HERE="$(cd "$(dirname "$0")" && pwd)"
ACL="${DUCKDB_ACL:?DUCKDB_ACL is a duckdb-acl checkout, built}"
DUCKDB="$ACL/build/release/duckdb"
ACL_EXT="$ACL/build/release/extension/acl/acl.duckdb_extension"
PORT="${CONSOLE_E2E_PORT:-41101}"
WORK="$(mktemp -d)"
PIDS=()
cleanup() {
	exec 3>&- 2>/dev/null || true
	for p in ${PIDS[@]+"${PIDS[@]}"}; do pkill -P "$p" 2>/dev/null || true; kill "$p" 2>/dev/null || true; done
	rm -rf "$WORK"
}
trap cleanup EXIT
fail() {
	echo "FAIL: $*" >&2
	tail -20 "$WORK/node.log" >&2 || true
	exit 1
}
! (echo >"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null || fail "port $PORT is taken (CONSOLE_E2E_PORT moves it)"

# the group's certificate, signed by a CA of this run
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=test CA" \
	-keyout "$WORK/ca.key" -out "$WORK/ca.crt" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=hugr-node-etl.hugr.svc" \
	-keyout "$WORK/node.key" -out "$WORK/node.csr" 2>/dev/null
printf 'subjectAltName=DNS:hugr-node-etl.hugr.svc\nextendedKeyUsage=serverAuth\n' >"$WORK/ext"
openssl x509 -req -in "$WORK/node.csr" -CA "$WORK/ca.crt" -CAkey "$WORK/ca.key" -CAcreateserial -days 1 \
	-extfile "$WORK/ext" -out "$WORK/node.crt" 2>/dev/null

cd "$ACL" # the fixture issuer test/idp/s is read relative to its root; tokens are minted, never printed
mint() { python3 test/scripts/idp_fixtures.py mint \
	"{\"iss\": \"test/idp/s\", \"aud\": \"api://acl-test\", \"exp\": 4102444800, \"sub\": \"$1\", \"roles\": [\"$2\"]}"; }
mint ops@example.org ops >"$WORK/ops.token"
mint analyst@example.org analyst >"$WORK/analyst.token"

cat >"$WORK/node.sql" <<SQL
LOAD '$ACL_EXT';
ATTACH ':memory:' AS phys;
CREATE TABLE phys.main.t AS SELECT 1 AS v;
ATTACH ':memory:' AS store;
SELECT acl_use_db('store', 'acl', true);
SET GLOBAL acl_allow_anonymous_admin = true;
SET GLOBAL acl_jwks_locations = 'test/idp/';
SELECT acl_define_issuer('test/idp/s', '{"url": "test/idp/s", "client": {"audiences": ["api://acl-test"], "roles_from": ["roles"]}}');
ACL ADMIN CREATE VIRTUAL CATALOG c;
ACL ADMIN CREATE VIRTUAL TABLE c.t AS phys.main.t;
ACL ADMIN CREATE ROLE analyst;
ACL ADMIN CREATE ROLE ops;
ACL ADMIN GRANT CATALOG c TO ROLE analyst WITH (select) MAIN;
ACL ADMIN MAP CLAIM 'analyst' FROM CLIENT 'test/idp/s' TO ROLE analyst;
ACL ADMIN MAP CLAIM 'ops' FROM CLIENT 'test/idp/s' TO ROLE ops;
ACL ADMIN GRANT ADMIN observe TO ROLE ops;
ACL ADMIN GRANT ADMIN operate TO ROLE ops;
ACL ADMIN GRANT FUNCTION CATEGORY meta TO ROLE ops;
ACL ADMIN GRANT FUNCTION CATEGORY environment TO ROLE ops;
SET GLOBAL acl_allow_anonymous_admin = false;
SET GLOBAL acl_node_label = 'hugr-node-etl-a';
SELECT acl_flight_serve('grpc+tls://127.0.0.1:$PORT', '$WORK/node.crt', '$WORK/node.key');
SQL
mkfifo "$WORK/node.in"
"$DUCKDB" -unsigned <"$WORK/node.in" >"$WORK/node.log" 2>&1 &
PIDS+=($!)
exec 3>"$WORK/node.in" # held open until the run ends: the node serves until then
cat "$WORK/node.sql" >&3
for _ in $(seq 1 100); do (echo >"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null && break; sleep 0.1; done
(echo >"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null || fail "the node does not serve"

(cd "$HERE/../.." && GOWORK=off go build -o "$WORK/probe" ./test/console/local) || fail "the probe does not build"
probe() { # <token file> <statement>...
	local token="$1"
	shift
	"$WORK/probe" -node "etl=127.0.0.1:$PORT" -ca "$WORK/ca.crt" -token-file "$token" "$@"
}

# the operator: its rights, the load report, the sessions (its own among them), drain and resume, an unknown session
probe "$WORK/ops.token" \
	"SELECT * FROM platform.console_info()" \
	"SELECT * FROM platform.node_load" \
	"SELECT id, subject, roles, door, idle_seconds, expires_at, audit_level, audit_level_source, profile_level, profile_source, groups, charged_group FROM platform.sessions" \
	"DRAIN NODE" "RESUME NODE" "KILL SESSION '0A1B2C3D4E5F'" >"$WORK/ops.json" || fail "the operator's probe"
# round 2: the engine's memory, the node's inventory, its ring, lineage; the profile's views (a standalone node
# refuses them); a session's tracing
probe "$WORK/ops.token" \
	"SELECT tag, memory_usage_bytes, temporary_storage_bytes FROM duckdb_memory()" \
	"SELECT size FROM duckdb_temporary_files()" \
	"SELECT current_setting('memory_limit') AS memory_limit, current_setting('max_temp_directory_size') AS max_temp_directory_size" \
	"SELECT extension_name, loaded, installed, extension_version, install_mode, installed_from FROM duckdb_extensions() WHERE loaded OR installed" \
	"SELECT kind, name, state, node_value, profile_value, scope FROM platform.drift" \
	"SELECT scope, kind, name, spec, class, version, depends_on, comment FROM platform.cluster_items" \
	"SELECT status, sending, level, namespace FROM platform.lineage_status" \
	"SELECT ts, seq, kind, level, door, session, subject, roles, statement, verdict, reason_code, rows, duration_us, correlation_id, traceparent, detail FROM platform.audit_events ORDER BY seq DESC LIMIT 200" \
	"SET SESSION '0A1B2C3D4E5F' AUDIT LEVEL ALL" "PROFILE SESSION '0A1B2C3D4E5F' SAMPLED" \
	>"$WORK/round2.json" || fail "the round 2 probe"
# the analyst: no administration - the rights say so and the node refuses its reads
probe "$WORK/analyst.token" \
	"SELECT * FROM platform.console_info()" "SELECT * FROM platform.node_load" "DRAIN NODE" \
	>"$WORK/analyst.json" || fail "the analyst's probe"

python3 - "$WORK/ops.json" "$WORK/analyst.json" <<'PY' || fail "the answers"
import json, sys
ops, analyst = (json.load(open(f)) for f in sys.argv[1:3])
info = ops["SELECT * FROM platform.console_info()"][0]
assert set(info["bundles"]) >= {"observe", "operate"}, info["bundles"]
for v in ("node_load", "sessions"):
    assert v in info["my_views"], (v, info["my_views"])
for f in ("kill_session", "drain", "resume"):
    assert f in info["my_functions"], (f, info["my_functions"])
load = ops["SELECT * FROM platform.node_load"]
assert len(load) == 1 and load[0]["draining"] is False and load[0]["sessions_live"] >= 1, load
sessions = next(v for k, v in ops.items() if k.startswith("SELECT id, subject"))
assert any(s["subject"] == "ops@example.org" for s in sessions), sessions
assert all(len(s["id"]) == 12 for s in sessions), sessions
for k in ("DRAIN NODE", "RESUME NODE"):
    assert "error" not in ops[k], (k, ops[k])
assert ops["KILL SESSION '0A1B2C3D4E5F'"] == [{"kill_session": False}], ops["KILL SESSION '0A1B2C3D4E5F'"]
assert ops["signed_out"] == 1, ops["signed_out"]
# no administration at all: the node refuses console_info itself (a binder error) - the console reads no rights
assert analyst["SELECT * FROM platform.console_info()"].get("code") == "InvalidArgument", analyst
assert "error" in analyst["SELECT * FROM platform.node_load"], analyst
assert analyst["DRAIN NODE"].get("code") == "InvalidArgument", analyst
print("the shapes the console reads:")
print("  node_load:", json.dumps(load[0], sort_keys=True))
print("  a session:", json.dumps(sessions[0], sort_keys=True))
print("  KILL SESSION of no session:", json.dumps(ops["KILL SESSION '0A1B2C3D4E5F'"]))
print("  the analyst's node_load:", analyst["SELECT * FROM platform.node_load"]["error"])
PY
python3 - "$WORK/round2.json" <<'PY' || fail "the round 2 answers"
import json, sys
r = json.load(open(sys.argv[1]))
for k, v in r.items():
    print(" ", k[:60], "->", json.dumps(v)[:300])
PY
echo "PASS: the console's node calls"
