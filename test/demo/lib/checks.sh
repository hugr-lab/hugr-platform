# shellcheck shell=bash
# The demo's checks (spec 004), each an "ok: …" line or the run fails naming it. SQL runs as a principal through the
# checker (test/demo/check: Flight SQL, the principal's token by client credentials, its secret read from a file).

CHECKER=""

checker_up() {
	CHECKER="$WORK/check"
	(cd "$HERE/check" && go build -o "$CHECKER" .) || fail "the checker did not build"
	idp_checker_up
	rm -rf "$WORK/ports" && mkdir -p "$WORK/ports"
	follow_logs
}

# pods <group>: the group's ready pods that are not going away
pods() {
	k -n hugr get pods -l "platform.hugr.io/group=$1" -o json | python3 -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    ready = any(c["type"] == "Ready" and c["status"] == "True" for c in p["status"].get("conditions", []))
    if ready and not p["metadata"].get("deletionTimestamp"):
        print(p["metadata"]["name"])'
}

# as <principal> <pod> <sql>: the statement's rows, or its refusal on stderr (exit 1). The principals: analyst-a,
# analyst-b; admin (the cluster and policy bundles: the profile's and the resource groups' writes); breakglass
# (passthrough: a node's own state read through ACL NATIVE). A fourth argument -hold leaves the session open, as a client's own
as() {
	local principal="$1" pod="$2" sql="$3" group
	group="$(k -n hugr get pod "$pod" -o jsonpath='{.metadata.labels.platform\.hugr\.io/group}')"
	if [ ! -s "$WORK/ports/$pod" ]; then
		forward hugr "pod/$pod" 32010 >"$WORK/ports/$pod.new" && mv "$WORK/ports/$pod.new" "$WORK/ports/$pod" ||
			return 1
	fi
	# shellcheck disable=SC2046 # the IdP's flags are words
	bounded 120 "$CHECKER" $(token_flags) -client "$(principal_id "$principal")" -secret-file "$WORK/secrets/principal-$principal" \
		-node "127.0.0.1:$(cat "$WORK/ports/$pod")" -server-name "hugr-node-$group.hugr.svc" -ca "$WORK/ca.crt" -sql "$sql" \
		${4:+"$4"}
}

# via_front <principal> <sql>: as `as`, through the front (spec 002) - one front pod, so its metrics are the calls' own
via_front() {
	local principal="$1" sql="$2"
	if [ ! -s "$WORK/ports/front" ]; then
		forward hugr "pod/$(front_pod)" 32010 >"$WORK/ports/front.new" && mv "$WORK/ports/front.new" "$WORK/ports/front" ||
			return 1
	fi
	# shellcheck disable=SC2046 # the IdP's flags are words
	bounded 120 "$CHECKER" $(token_flags) -client "$(principal_id "$principal")" -secret-file "$WORK/secrets/principal-$principal" \
		-node "127.0.0.1:$(cat "$WORK/ports/front")" -server-name hugr-front.hugr.svc -ca "$WORK/ca.crt" -sql "$sql"
}
# front_pod: one ready front pod that is not going away - the same one for a whole check (its metrics are its own)
front_pod() {
	[ -s "$WORK/ports/front-pod" ] && k -n hugr get pod "$(cat "$WORK/ports/front-pod")" >/dev/null 2>&1 &&
		{ cat "$WORK/ports/front-pod"; return; }
	k -n hugr get pods -l app.kubernetes.io/name=hugr-front -o json | python3 -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    ready = any(c["type"] == "Ready" and c["status"] == "True" for c in p["status"].get("conditions", []))
    if ready and not p["metadata"].get("deletionTimestamp"):
        print(p["metadata"]["name"])
        break' | tee "$WORK/ports/front-pod"
}
# front_metric <series>: the value of one of the front pod's metrics (its internal port, never the Service's)
front_metric() {
	if [ ! -s "$WORK/ports/front-internal" ]; then
		forward hugr "pod/$(front_pod)" 8081 >"$WORK/ports/front-internal"
	fi
	curl -fsS "http://127.0.0.1:$(cat "$WORK/ports/front-internal")/metrics" |
		awk -v s="$1" '$1 == s {v = $2} END {print v + 0}' # a series not yet counted is 0
}
no_pods() { [ -z "$(k -n hugr get pods -l "platform.hugr.io/group=$1" -o name)" ]; }

first_pod() { pods "$1" | head -1; }
status() { k -n hugr get hng "$1" -o jsonpath="$2"; }
lease() { k -n hugr get lease "$1" -o jsonpath="{.metadata.annotations.platform\.hugr\.io/$2}"; }
deploy() { k -n hugr get deploy "hugr-node-$1" -o jsonpath="$2"; }

# conditions for eventually, each reading the state when it runs
status_is() { [ "$(status "$1" "$2")" = "$3" ]; }
lease_is() { [ "$(lease "$1" "$2")" = "$3" ]; }
deploy_is() { [ "$(deploy "$1" "$2")" = "$3" ]; }
group_ready() { status_is "$1" '{.status.readyReplicas}/{.status.resourceGroupKnown}' "$(status "$1" '{.spec.replicas}')/true"; }
restarts_needed() { [ "$(status "$1" '{.status.profile.needsRestart[*]}' | wc -w | tr -d ' ')" = "$2" ]; }
draining_is() { status_is "$1" '{.status.conditions[?(@.type=="Draining")].status}' "$2"; }
# went_drained <pod>: the pod is gone, and its log (kept from its start) says it drained and stopped
went_drained() {
	! k -n hugr get pod "$1" >/dev/null 2>&1 && grep -q 'hugr_node: stopping: draining' "$WORK/logs/$1.log" &&
		grep -q 'hugr_node: stopped' "$WORK/logs/$1.log"
}
# plain_duckdb <pod> <sql>: duckdb in the node's container, with nothing of the node's environment
plain_duckdb() {
	bounded 120 kubectl --context "$DEMO_CONTEXT" -n hugr exec "$1" -- env -i HOME=/tmp /usr/local/bin/duckdb -noheader \
		-csv -c "$2" | tail -1
}

check_entry() {
	step "1. the entry: through the operator, tresor by the node's own token, the catalog by its secret's type"
	local g pod
	# entered <pod>: its log (kept from its start; a follower just started fills it in a moment) shows the entry
	entered() {
		grep -q 'step 2 bootstrap document: ok' "$WORK/logs/$1.log" && grep -q 'step 3 tresor: ok' "$WORK/logs/$1.log" &&
			grep -q 'step 5 policy catalog: ok' "$WORK/logs/$1.log" && grep -q 'hugr_node: ready' "$WORK/logs/$1.log"
	}
	for g in "${GROUPS_[@]}"; do
		eventually 120 "group $g ready with its resource group known" group_ready "$g"
		for pod in $(pods "$g"); do
			eventually 30 "node $pod's log shows its entry" entered "$pod"
			[ "$(k -n hugr get lease "$pod" -o jsonpath='{.spec.holderIdentity}')" = "$pod" ] || fail "no Lease of $pod"
		done
	done
	ok "every node fetched its document from the operator, logged in to tresor, attached the catalog; each holds its Lease"
}

check_federation() {
	step "2. the federation: a node holds no credential of its own"
	local g problems secrets want
	for g in "${GROUPS_[@]}"; do
		k -n hugr get pods -l "platform.hugr.io/group=$g" -o json >"$WORK/pods.json"
		# the live pods, as admitted: a Secret is mounted only as the doors' certificate, none reaches the environment,
		# a projected volume carries tokens, ConfigMaps and the downward API only
		problems="$(python3 - "$WORK/pods.json" "hugr-node-$g-tls" <<'PY'
import json, sys
pods, tls = json.load(open(sys.argv[1]))["items"], sys.argv[2]
found = []
for p in pods:
    spec, name = p["spec"], p["metadata"]["name"]
    if spec.get("imagePullSecrets"):
        found.append(name + ": imagePullSecrets")
    for v in spec.get("volumes", []):
        if "secret" in v and v["secret"].get("secretName") != tls:
            found.append(name + ": the Secret volume " + v["name"])
        for source in v.get("projected", {}).get("sources", []):
            if not set(source) <= {"serviceAccountToken", "configMap", "downwardAPI"}:
                found.append(name + ": the projected volume " + v["name"] + " carries " + ",".join(source))
    for c in spec.get("containers", []) + spec.get("initContainers", []):
        found += [name + ": envFrom a Secret" for e in c.get("envFrom", []) if "secretRef" in e]
        found += [name + ": env " + e["name"] + " from a Secret" for e in c.get("env", [])
                  if "secretKeyRef" in e.get("valueFrom", {})]
print("; ".join(found))
sys.exit(1 if found else 0)
PY
)" || fail "group $g's pods: $problems"
		for pod in $(pods "$g"); do
			grep -q 'DEVELOPMENT identity' "$WORK/logs/$pod.log" && fail "node $pod runs a development identity (a key)"
		done
	done
	# the namespace's Secrets: the operator's certificate and the console's, each group's doors' and the front's public
	# one, nothing else
	secrets="$(k -n hugr get secrets -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort | tr '\n' ' ')"
	want="$( (printf '%s\n' hugr-platform-tls "${GROUPS_[@]/#/hugr-node-}" | sed -e '2,$s/$/-tls/'; echo hugr-front
		echo hugr-console) |
		sort | tr '\n' ' ')"
	[ "$secrets" = "$want" ] || fail "the namespace's Secrets are $secrets, want $want"
	ok "a node mounts only its doors' certificate and reads no Secret; its tresor login is its projected ServiceAccount token ($IDP_NAME's federation); the namespace holds only TLS Secrets"
}

check_engine() {
	step "3. the engine against the pod (2 GiB, a CPU request of $NODE_CPU, a 2 GiB spill volume)"
	local pod got want_memory want_temp
	pod="$(first_pod a)"
	got="$(as breakglass "$pod" "ACL NATIVE SELECT current_setting('memory_limit'), current_setting('threads'), current_setting('max_temp_directory_size')")" ||
		fail "the node's settings on $pod"
	# what duckdb shows for the values the node sets: 80% and 90% of 2 GiB in whole MiB
	want_memory="$(plain_duckdb "$pod" "SET memory_limit='1638MiB'; SELECT current_setting('memory_limit')")"
	want_temp="$(plain_duckdb "$pod" "SET max_temp_directory_size='1843MiB'; SELECT current_setting('max_temp_directory_size')")"
	[ "$got" = "$(printf '%s\t1\t%s' "$want_memory" "$want_temp")" ] || fail "the node's engine: $got (want $want_memory, 1, $want_temp)"
	ok "the node runs memory_limit $want_memory, threads 1, max_temp_directory_size $want_temp: 80% / the CPU / 90%"
	got="$(plain_duckdb "$pod" "SELECT current_setting('memory_limit') || ' / ' || current_setting('threads') || ' / ' || current_setting('max_temp_directory_size')")"
	CGROUP_FINDING="plain duckdb in the 2 GiB-limit container with a CPU request of $NODE_CPU chooses memory_limit / threads / max_temp_directory_size = $got"
	ok "$CGROUP_FINDING: the threads and the spill are the node's doing, the memory DuckDB's own choice too"
}

check_placement() {
	step "4. a principal on its group's node"
	local a b got
	a="$(first_pod a)"
	b="$(first_pod b)"
	got="$(as analyst-a "$a" "SELECT region, sum(amount) FROM orders GROUP BY region ORDER BY region")" ||
		fail "analyst_a's read on $a"
	[ "$got" = "$(printf 'north\t40\nsouth\t20')" ] || fail "analyst_a on a: $got"
	if got="$(as analyst-a "$b" "SELECT count(*) FROM orders" 2>&1)"; then fail "analyst_a is served on b"; fi
	grep -q 'code = Unavailable' <<<"$got" && grep -q 'served by "a"' <<<"$got" || fail "analyst_a on b: $got"
	ok "analyst_a reads demo.orders on a node of a; a node of b refuses it (UNAVAILABLE), naming a"
}

check_front() {
	step "4b. the front: one address, every principal on its group's node (spec 002)"
	local admin a b got want reseats routed
	rm -f "$WORK/ports/front" "$WORK/ports/front-internal" "$WORK/ports/front-pod"
	admin="$(first_pod a)"
	a="$(first_pod a)"
	b="$(first_pod b)"
	# group a is the policy's default: a new connection is seated there first, and a node of a sends analyst_b on
	as admin "$admin" "ALTER RESOURCE GROUP a SET DEFAULT" >/dev/null || fail "ALTER RESOURCE GROUP a SET DEFAULT"
	eventually 30 "a's node reports the default group" lease_is "$a" group-default true
	want="$(as analyst-b "$b" "SELECT count(*) FROM orders")" || fail "analyst_b's read on b"
	# the metrics are the front pod's since its start: what the check itself did is the difference
	reseats="$(front_metric 'hugr_front_reseats{reason="wrong_resource_group"}')"
	routed="$(front_metric 'hugr_front_calls{outcome="routed",protocol="flight"}')"
	for _ in 1 2 3; do
		got="$(via_front analyst-b "SELECT count(*) FROM orders")" || fail "analyst_b through the front: $got"
		[ "$got" = "$want" ] || fail "analyst_b through the front read $got, on b $want"
		got="$(via_front analyst-a "SELECT region, sum(amount) FROM orders GROUP BY region ORDER BY region")" ||
			fail "analyst_a through the front: $got"
		[ "$got" = "$(printf 'north\t40\nsouth\t20')" ] || fail "analyst_a through the front: $got"
	done
	reseats=$(($(front_metric 'hugr_front_reseats{reason="wrong_resource_group"}') - reseats))
	routed=$(($(front_metric 'hugr_front_calls{outcome="routed",protocol="flight"}') - routed))
	# analyst_b's statement is re-seated once from a; each DoGet goes to its ticket's node, never seated again
	[ "$reseats" = 3 ] || fail "the front re-seated $reseats times for three statements of analyst_b, want 3"
	[ "$routed" -ge 6 ] || fail "the front routed $routed calls by their ticket, want 6 (a DoGet per statement)"
	ok "both analysts read through the front; analyst_b, seated on the default group a, was re-seated on b by a's refusal; every DoGet went to the node its ticket names (the checker keeps no cookie)"
	# no node of b takes a new session: refused after the wait, never seated on another group's node
	k -n hugr patch hng b --type=merge -p '{"spec":{"replicas":0}}' >/dev/null || fail "group b to no node"
	eventually 120 "b has no node" no_pods b
	if got="$(via_front analyst-b "SELECT count(*) FROM orders" 2>&1)"; then fail "analyst_b was served with no node of b"; fi
	grep -q 'hugr: no node of group b can take a new session - retry' <<<"$got" || fail "analyst_b with no node of b: $got"
	k -n hugr patch hng b --type=merge -p '{"spec":{"replicas":1}}' >/dev/null || fail "group b back to one node"
	eventually 300 "group b serves again" group_ready b
	follow_logs
	ok "with no node of b, analyst_b is refused after the wait, naming b - never seated on a; b came back"
}

# console_port: the console's Service forwarded (spec 003) - one operator pod, so one pool of node sessions
console_port() {
	if [ ! -s "$WORK/ports/console" ]; then
		forward hugr svc/hugr-platform-console 443 >"$WORK/ports/console.new" &&
			mv "$WORK/ports/console.new" "$WORK/ports/console" || return 1
	fi
	cat "$WORK/ports/console"
}
# console_api <principal> <method> <path> [body] [scope]: the console's API as the principal, its answer's JSON - or
# the refusal on stderr (exit 1); a path from / is one of the console's mounts, a scope the token's for that service
console_api() {
	local principal="$1" method="$2" path="$3" body="${4:-}" scope="${5:-}" port
	case "$path" in /*) ;; *) path="/api/v1/console/$path" ;; esac
	port="$(console_port)" || return 1
	# shellcheck disable=SC2046 # the IdP's flags are words; a scope given overrides theirs (the last flag counts)
	bounded 120 "$CHECKER" $(token_flags) $([ -z "$scope" ] || echo "-scope $scope") -client "$(principal_id "$principal")" \
		-secret-file "$WORK/secrets/principal-$principal" -server-name hugr-platform-console.hugr.svc -ca "$WORK/ca.crt" \
		-method "$method" -body "$body" -console "https://127.0.0.1:$port$path"
}
# console_get <path>: what the console serves to anyone - its config, a mounted console's module
console_get() {
	curl -fsS --max-time 60 --cacert "$WORK/ca.crt" \
		--connect-to "hugr-platform-console.hugr.svc:443:127.0.0.1:$(console_port)" "https://hugr-platform-console.hugr.svc$1"
}
# json_true <json> <python expression over r> [values...]: the expression holds of the document. The values are a[0],
# a[1], ... in it - what a node or the cluster answered never becomes the expression's text
json_true() {
	python3 -c 'import json, sys; r, a = json.loads(sys.argv[1]), sys.argv[3:]; sys.exit(0 if eval(sys.argv[2]) else 1)' "$@"
}
json_get() { python3 -c 'import json, sys; r, a = json.loads(sys.argv[1]), sys.argv[3:]; print(eval(sys.argv[2]))' "$@"; }
# session_of <principal's role> <pods...>: "<pod> <ops id>" of a Flight session of the role, as the console lists it
session_of() {
	local role="$1" pod out
	shift
	for pod in "$@"; do
		out="$(console_api admin GET "nodes/$pod/sessions")" || return 1
		out="$(json_get "$out" "next((s['id'] for s in r['sessions'] if s['door'] == 'flight' and a[0] in (s['roles'] or [])), '')" "$role")"
		[ -z "$out" ] || { echo "$pod $out"; return 0; }
	done
	return 1
}
# no_admin_session <pod>: the node holds no session of platform_admin's - the console's calls are the admin's
no_admin_session() {
	[ "$(as breakglass "$1" "SELECT count(*) FROM platform.sessions WHERE list_contains(roles, 'platform_admin')")" = 0 ]
}
# tracing_is <pod> <id> <python condition over the session s>
tracing_is() {
	local out
	out="$(console_api admin GET "nodes/$1/sessions")" || return 1
	json_true "$out" "any(s['id'] == a[0] and ($3) for s in r['sessions'])" "$2"
}

check_console() {
	step "4c. the console (spec 003): the fleet, a node's sessions and actions, a group's scale - each judged by the ACL"
	local out me pod id held a b
	rm -f "$WORK/ports/console"
	a="$(first_pod a)"
	b="$(first_pod b)"
	# what the browser reads before a sign-in: who signs it in and where the mounts are - nothing secret
	out="$(console_get /api/v1/console/config)" || fail "the console's config"
	json_true "$out" "r['issuer'] == a[0] and r['clientID'] == 'hugr-console' and r['mounts']['tresor']['base'] == '/tresor'" "$IDP_ISSUER" ||
		fail "the console's config: $out"

	# analyst_a administers nothing: the console shows it nothing and does nothing for it (a node of b, asked first,
	# refuses it and names a)
	me="$(console_api analyst-a GET me)" || fail "analyst_a's rights in the console"
	json_true "$me" "not any(v for k, v in r['rights'].items() if k != 'bundles')" || fail "analyst_a's rights: $me"
	for out in "GET groups" "POST nodes/$a/drain" "GET nodes/$a/sessions" "PATCH groups/a"; do
		# shellcheck disable=SC2086 # method and path
		if console_api analyst-a $out '{"replicas": 1}' >/dev/null 2>"$WORK/console.err"; then fail "analyst_a: $out answered"; fi
		grep -q 'answered 403' "$WORK/console.err" || fail "analyst_a: $out: $(cat "$WORK/console.err")"
	done
	ok "analyst_a, who administers nothing, has no right in the console: the fleet, a node's sessions, a drain, a scale refused (403)"

	me="$(console_api admin GET me)" || fail "the admin's rights in the console"
	json_true "$me" "all(r['rights'][k] for k in ('fleet', 'sessions', 'endSession', 'drain', 'resume', 'tracing', 'scale', 'infrastructure', 'events'))" ||
		fail "the admin's rights: $me"
	out="$(console_api admin GET groups)" || fail "the fleet"
	json_true "$out" "sorted(g['name'] for g in r['groups']) == ['a', 'b'] and r['plan']['maxNodes'] == 4" || fail "the fleet: $out"
	for pod in $(pods a) $(pods b); do
		json_true "$out" "any(n['label'] == a[0] and n['current'] and n['admitting'] for g in r['groups'] for n in g['nodes'])" "$pod" ||
			fail "the fleet does not show $pod admitting: $out"
	done
	out="$(console_api admin GET "nodes/$a")" || fail "node $a's page"
	json_true "$out" "r['group'] == 'a' and r['load']['group'] == 'a' and not r['load']['draining'] and not r.get('loadError')" ||
		fail "node $a's load report: $out"
	out="$(console_api admin GET "nodes/$a/kubernetes")" || fail "node $a's pod"
	json_true "$out" "r['phase'] == 'Running' and r['limits']['memoryBytes'] == 2 << 30" || fail "node $a's pod: $out"
	out="$(console_api admin GET "nodes/$a/memory")" || fail "node $a's memory"
	json_true "$out" "not r.get('error') and r['tags']['rows'] and r['limits']['rows']" || fail "node $a's memory: $out"
	out="$(console_api admin GET "nodes/$a/inventory")" || fail "node $a's inventory"
	json_true "$out" "any(e['extension_name'] == 'hugr_node' and e['loaded'] for e in r['extensions']['rows'])" ||
		fail "node $a's extensions: $out"
	out="$(console_api admin GET "nodes/$a/logs")" || fail "node $a's log"
	json_true "$out" "'hugr_node: ' in r['text']" || fail "node $a's log reads no line of the node"
	console_api admin GET "nodes/$a/events" >/dev/null || fail "node $a's audit ring"
	out="$(console_api admin GET cluster)" || fail "the cluster's page"
	json_true "$out" "len(r['lineage']) == $(($(pods a | wc -l) + $(pods b | wc -l))) and not r['profile'].get('error')" ||
		fail "the cluster's page: $out"
	ok "the admin's console: the fleet (a and b, every node admitting), $a's load report, pod, memory, extensions, log and audit ring, the cluster's profile and every node's lineage"

	# a person's session on a node: its tracing, then its end - analyst_a's own, held as a driver holds one
	as analyst-a "$a" "SELECT count(*) FROM orders" -hold >/dev/null || fail "analyst_a's own session on $a"
	held="$(session_of analyst_a "$a")" || fail "the admin sees no session of analyst_a on $a"
	pod="${held% *}"
	id="${held#* }"
	console_api admin POST "nodes/$pod/sessions/$id/tracing" '{"audit": "all", "profile": "sampled"}' >/dev/null ||
		fail "the tracing of analyst_a's session"
	tracing_is "$pod" "$id" "s['audit_level'] == 'all' and s['audit_level_source'] == 'override' and s['profile_level'] == 'sampled' and s['profile_source'] == 'override'" ||
		fail "analyst_a's session does not trace"
	console_api admin POST "nodes/$pod/sessions/$id/tracing" '{"audit": "", "profile": ""}' >/dev/null ||
		fail "the tracing of analyst_a's session, cleared"
	tracing_is "$pod" "$id" "s['audit_level_source'] != 'override' and s['profile_source'] != 'override'" ||
		fail "analyst_a's session keeps the operator's levels"
	console_api admin POST "nodes/$pod/sessions/$id/end" >/dev/null || fail "ending analyst_a's session"
	tracing_is "$pod" "$id" "True" && fail "analyst_a's session lives on"
	if console_api admin POST "nodes/$pod/sessions/$id/end" >/dev/null 2>"$WORK/console.err"; then fail "a session ended twice"; fi
	grep -q 'answered 404' "$WORK/console.err" || fail "ending a session gone: $(cat "$WORK/console.err")"
	ok "analyst_a's session on $pod traced (audit all, profile sampled - the operator's), cleared, ended; ending it again is a 404"

	# a drain and its resume, each call a session of its own with a new token of the admin's: a draining node seats nobody
	# new but the call of who may resume it (duckdb-acl spec 066's addendum)
	console_api admin POST "nodes/$b/drain" >/dev/null || fail "DRAIN NODE through the console"
	eventually 30 "$b reports draining" lease_is "$b" draining true
	out="$(console_api admin GET "nodes/$b")" || fail "node $b's page while it drains"
	json_true "$out" "r['load']['draining'] and not r['load']['admit_new_session']" || fail "node $b drains: $out"
	console_api admin POST "nodes/$b/resume" >/dev/null || fail "RESUME NODE through the console, with the admin's next token"
	eventually 30 "$b admits sessions again" lease_is "$b" draining false
	ok "$b drained and resumed through the console, the admin's every call its own token"

	# a scale is Kubernetes' write, judged by the webhook; the person who made it is on the group
	if console_api admin PATCH groups/a '{"replicas": 4}' >/dev/null 2>"$WORK/console.err"; then
		fail "scaling a to 4 (with b's 1, past 4) through the console was accepted"
	fi
	grep -q 'answered 422' "$WORK/console.err" && grep -q 'at most 4' "$WORK/console.err" ||
		fail "the console's scale past the plan: $(cat "$WORK/console.err")"
	console_api admin PATCH groups/b '{"replicas": 1}' >/dev/null || fail "group b's scale through the console"
	out="$(k -n hugr get hng b -o json --show-managed-fields)"
	json_true "$out" "r['metadata']['annotations'].get('platform.hugr.io/changed-by', 'unknown') != 'unknown' and any(m['manager'] == 'hugr-console' for m in r['metadata']['managedFields'])" ||
		fail "group b does not name the console's write and its person"
	ok "the console's scale past the plan refused by the webhook (422); within it written by the console's field manager, the person named on the group"

	# the services' own consoles, mounted: their modules for anyone, their API with the person's token for them. The
	# demo's kista signs nobody in, so the platform names no kista audience and the shell offers no Extensions page: its
	# mount only serves the module here
	console_get /tresor/ui/mfe/tresor.js | grep -q contract || fail "tresor's module through the console"
	console_get /kista/ui/mfe/kista.js | grep -q contract || fail "kista's module through the console"
	out="$(console_api seeder GET /tresor/v1/whoami "" "$TRESOR_SCOPE")" || fail "tresor's whoami through the console's mount"
	json_true "$out" "'role:demo_seeder' in r['roles']" || fail "tresor's whoami of the seeder names no demo_seeder"
	ok "tresor's console mounted (its module, its API answered the seeder through the mount); kista's module served through its mount"

	# the console holds no session on any node: each of its calls was one of its own, ended with it (spec 003) - on the
	# call's way out, which may trail its last batch by a moment
	for pod in $(pods a) $(pods b); do
		eventually 10 "the console left no session of the admin's on $pod" no_admin_session "$pod"
	done
	ok "the console left no session on any node: each of its calls was a session of its own"
}

check_hot() {
	step "5. a hot item: ACL CLUSTER ATTACH … IN GROUP a, written through a node of a"
	local writer pod version
	writer="$(first_pod a)"
	as admin "$writer" "ACL CLUSTER ATTACH ':memory:' AS hot_a (TYPE duckdb) IN GROUP a" >/dev/null ||
		fail "the hot ATTACH was refused"
	version="$(as breakglass "$writer" "ACL NATIVE SELECT acl_cluster_version()")" || fail "the profile's version on $writer"
	for pod in $(pods a); do
		eventually 60 "node $pod applies version $version" lease_is "$pod" profile-applied "$version"
		[ "$(as breakglass "$pod" "ACL NATIVE SELECT count(*) FROM duckdb_databases() WHERE database_name = 'hot_a'")" = 1 ] ||
			fail "node $pod has no hot_a"
	done
	eventually 30 "group a's status: version $version applied by both" \
		status_is a '{.status.profile.target}/{.status.profile.applied}' "$version/2"
	# b's node reaches the same version, and takes nothing of it
	pod="$(first_pod b)"
	eventually 60 "b's node $pod applies version $version" lease_is "$pod" profile-applied "$version"
	[ "$(as breakglass "$pod" "ACL NATIVE SELECT count(*) FROM duckdb_databases() WHERE database_name = 'hot_a'")" = 0 ] ||
		fail "group b's node took group a's item"
	ok "every node of a attached hot_a on its tick (version $version, group a: applied 2/2); b's node is at $version without it"
}

check_restart() {
	step "6. a restart item, rolled: memory_limit 1GiB IN GROUP a, then the group's restartedAt"
	local writer pod got want old
	writer="$(first_pod a)"
	as admin "$writer" "ACL CLUSTER SET memory_limit = '1GiB' IN GROUP a" >/dev/null || fail "the SET was refused"
	eventually 60 "group a lists both nodes as needing a restart" restarts_needed a 2
	ok "group a's status lists its 2 nodes in needsRestart"
	# one at a time: a new pod up before an old one goes
	[ "$(deploy a '{.spec.strategy.rollingUpdate.maxSurge}/{.spec.strategy.rollingUpdate.maxUnavailable}')" = 1/0 ] ||
		fail "group a's Deployment does not roll one at a time"
	old="$(pods a)"
	restart_group a
	for pod in $old; do eventually 120 "the old node $pod drained and went" went_drained "$pod"; done
	eventually 120 "after the roll nobody needs a restart" restarts_needed a 0
	want="$(plain_duckdb "$(first_pod a)" "SET memory_limit='1GiB'; SELECT current_setting('memory_limit')")"
	for pod in $(pods a); do
		got="$(as breakglass "$pod" "ACL NATIVE SELECT current_setting('memory_limit')")" || fail "the node's settings on $pod"
		[ "$got" = "$want" ] || fail "after the roll $pod runs memory_limit $got, want $want"
	done
	ok "the pods rolled one at a time, each old one drained; the new nodes run memory_limit $want, nobody waits"
}

check_dropped_group() {
	step "7. a dropped resource group: its nodes drain themselves, and resume"
	local admin pod statement
	admin="$(first_pod a)"
	as admin "$admin" "DROP RESOURCE GROUP b" >/dev/null || fail "DROP RESOURCE GROUP b was refused"
	pod="$(first_pod b)"
	eventually 60 "group b says Draining" draining_is b True
	eventually 30 "b's node reports draining" lease_is "$pod" draining true
	ok "group b's node drained itself; the group says Draining"
	for statement in "CREATE RESOURCE GROUP b" "GRANT RESOURCE GROUP b TO ROLE analyst_b" \
		"GRANT RESOURCE GROUP b TO ROLE platform_admin" "GRANT RESOURCE GROUP b TO ROLE node_breakglass"; do
		as admin "$admin" "$statement" >/dev/null || fail "$statement was refused"
	done
	eventually 60 "group b resumes" draining_is b False
	eventually 30 "b's node admits sessions again" lease_is "$pod" draining false
	ok "recreated, the group resumed by itself"
}

check_plan() {
	step "8. the plan: past maxNodes refused by the webhook, within it a node enters by itself"
	local out before pod gone=""
	if out="$(k -n hugr patch hng a --type merge -p '{"spec":{"replicas":4}}' 2>&1)"; then
		fail "scaling a to 4 (with b's 1, past 4) was accepted"
	fi
	grep -q 'at most 4' <<<"$out" || fail "the webhook's refusal: $out"
	ok "kubectl's scale past the plan refused: $(sed -n 's/.*\(the plan runs at most[^"]*\).*/\1/p' <<<"$out" | head -1)"
	k -n hugr patch hng a --type merge -p '{"spec":{"replicas":3}}' >/dev/null
	eventually 180 "group a runs 3 ready nodes" status_is a '{.status.readyReplicas}' 3
	follow_logs
	ok "a third node of a entered by itself"
	before="$(pods a)"
	k -n hugr patch hng a --type merge -p '{"spec":{"replicas":2}}' >/dev/null
	one_went() {
		for pod in $before; do
			went_drained "$pod" && gone="$pod" && return 0
		done
		return 1
	}
	eventually 180 "one of a's nodes drained and went" one_went
	eventually 60 "group a is back to 2 ready nodes" status_is a '{.status.readyReplicas}' 2
	ok "scaled down to 2: $gone drained and went"
}

# reach <name> <labels> <host> <port>: a probe pod with the labels connects to host:port; prints Succeeded or Failed
PROBE_IMAGE="${PROBE_IMAGE:-busybox:1.37}"
reach() {
	k -n hugr delete pod "$1" --ignore-not-found --wait >/dev/null
	k -n hugr run "$1" --image="$PROBE_IMAGE" --restart=Never --labels="$2" --command -- nc -z -w 5 "$3" "$4" \
		>/dev/null || fail "the probe pod $1"
	probe_ended() { case "$(k -n hugr get pod "$1" -o jsonpath='{.status.phase}')" in Succeeded | Failed) return 0 ;; esac; return 1; }
	eventually 120 "the probe $1 ends" probe_ended "$1"
	k -n hugr get pod "$1" -o jsonpath='{.status.phase}'
	k -n hugr delete pod "$1" --wait=false >/dev/null
}

check_network() {
	step "9. the network: the node API answers a group's pods only, a node's door the front and the operator only"
	local door
	door="$(k -n hugr get pod "$(first_pod a)" -o jsonpath='{.status.podIP}')"
	[ "$(reach probe-outside app=probe hugr-platform.hugr.svc 8443)" = Failed ] ||
		fail "a pod of no group reaches the node API: the NetworkPolicy is not enforced"
	[ "$(reach probe-group platform.hugr.io/group=probe hugr-platform.hugr.svc 8443)" = Succeeded ] ||
		fail "a group's pod does not reach the node API"
	[ "$(reach probe-door app=probe "$door" 32010)" = Failed ] || fail "a pod of no role reaches a node's door"
	[ "$(reach probe-front app.kubernetes.io/part-of=hugr-platform,app.kubernetes.io/component=front "$door" 32010)" = Succeeded ] ||
		fail "the front does not reach a node's door"
	ok "a pod of no group is refused at the node API, a group's pod admitted; a node's door admits the front, refuses others"
}

check_policy() {
	step "10. the optional admission policy: a group's pods come only from its own chain"
	k apply -k "$ROOT/config/policy" >/dev/null || fail "the policy did not apply"
	eventually 30 "a hand-made pod as hugr-node-a refused" policy_refuses
	ok "a pod made by hand to run as hugr-node-a is refused"
	restart_group b
	ok "and the groups keep rolling under it"
	k delete -k "$ROOT/config/policy" >/dev/null 2>&1 || true
}
policy_refuses() {
	local out
	out="$(k -n hugr run by-hand --image="$(img hugr-node)" --image-pull-policy="$PULL" --restart=Never \
		--overrides='{"spec":{"serviceAccountName":"hugr-node-a"}}' 2>&1)" && {
		k -n hugr delete pod by-hand --wait=false >/dev/null 2>&1
		return 1
	}
	grep -q 'created by its Deployment only' <<<"$out"
}

check_no_secrets() {
	step "the run's secrets and tokens are nowhere they should not be"
	local file ns pod hits=""
	follow_logs
	{
		# every node's log from its start, the gone ones included, and a restarted container's previous one
		cat "$WORK"/logs/*.log
		for pod in $(k -n hugr get pods -l platform.hugr.io/group -o jsonpath='{.items[*].metadata.name}'); do
			k -n hugr logs "$pod" --previous 2>/dev/null || true
		done
		k -n hugr logs deploy/hugr-platform --tail -1
		k -n hugr logs -l app.kubernetes.io/name=hugr-front --tail -1 --prefix
		k -n hugr logs job/demo-seed --tail -1
		# the stack's services; OpenBao's dev mode prints its own root token, on purpose (spec 004)
		for ns in idp tresor kista db; do
			for pod in $(k -n "$ns" get pods -o jsonpath='{.items[*].metadata.name}'); do
				case "$pod" in openbao-*) continue ;; esac
				k -n "$ns" logs "$pod" --all-containers 2>/dev/null || true
			done
		done
		k get events -A
		k -n hugr get deploy,sa,svc,pdb,networkpolicy,lease,hng,hp,configmap,job -o yaml
	} >"$WORK/scan.txt" 2>&1
	k -n hugr get secret demo-seed >/dev/null 2>&1 && fail "the first deployment's SQL outlived its Job"
	for file in "$WORK"/secrets/*; do
		[ -s "$file" ] || continue
		grep -qFf "$file" "$WORK/scan.txt" && hits="$hits $(basename "$file")"
	done
	# a JWT by its shape - a header and a claims part, both base64 JSON - not three letters a certificate may hold
	grep -Eq 'eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.' "$WORK/scan.txt" && hits="$hits a-token"
	grep -q 'PRIVATE KEY' "$WORK/scan.txt" && hits="$hits a-private-key"
	rm -f "$WORK/scan.txt"
	[ -z "$hits" ] || fail "found in the logs, the events or the platform namespace's resources:$hits"
	ok "no secret of the run, no token, no private key in any log of the run (the nodes' from their start), the events or the namespace's resources; the seed's SQL is gone"
}

run_checks() {
	checker_up
	check_entry
	check_federation
	check_engine
	check_placement
	check_front
	check_console
	check_hot
	check_restart
	check_dropped_group
	check_plan
	check_network
	check_policy
	check_no_secrets
	echo
	echo "PASS: the platform in one cluster ($PROVIDER)"
	echo "finding: $CGROUP_FINDING"
}
