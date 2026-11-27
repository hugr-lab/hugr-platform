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
	# VIA_WAIT: a statement held while a node starts for it (spec 011 stage 2) waits longer
	bounded "$((${VIA_WAIT:-60} + 60))" "$CHECKER" $(token_flags) -client "$(principal_id "$principal")" \
		-secret-file "$WORK/secrets/principal-$principal" -node "127.0.0.1:$(cat "$WORK/ports/front")" \
		-server-name hugr-front.hugr.svc -ca "$WORK/ca.crt" -sql "$sql" -timeout "${VIA_WAIT:-60}s"
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
no_group() { ! k -n hugr get hng "$1" >/dev/null 2>&1; }

first_pod() { pods "$1" | head -1; }
status() { k -n hugr get hng "$1" -o jsonpath="$2"; }
lease() { k -n hugr get lease "$1" -o jsonpath="{.metadata.annotations.platform\.hugr\.io/$2}"; }
deploy() { k -n hugr get deploy "hugr-node-$1" -o jsonpath="$2"; }

# conditions for eventually, each reading the state when it runs
status_is() { [ "$(status "$1" "$2")" = "$3" ]; }
# demand_expired <lease>: the front's demand Lease is gone or no longer renewed (spec 011 stage 2)
demand_expired() {
	local json
	json="$(k -n hugr get lease "$1" -o json 2>/dev/null)" || return 0
	python3 -c '
import datetime, json, sys
s = json.loads(sys.argv[1])["spec"]
renewed = datetime.datetime.fromisoformat(s["renewTime"].replace("Z", "+00:00"))
sys.exit(0 if datetime.datetime.now(datetime.timezone.utc) > renewed + datetime.timedelta(seconds=s["leaseDurationSeconds"]) else 1)' "$json"
}
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
	# spec 005: the platform installed itself - hugr-init made the catalog and its secret, a first node wrote the first
	# policy into it (nodes that start together may both write it; one that comes after writes nothing)
	local writers=0
	for g in "${GROUPS_[@]}"; do
		for pod in $(pods "$g"); do
			grep -q 'step 5 first policy: written' "$WORK/logs/$pod.log" && writers=$((writers + 1))
			grep -q 'step 5 first policy: \(written\|the catalog holds its policy\)' "$WORK/logs/$pod.log" ||
				fail "node $pod's log shows no first-policy decision"
		done
	done
	[ "$writers" -ge 1 ] || fail "no node wrote the first policy"
	ok "every node fetched its document from the operator, logged in to tresor, attached the catalog hugr-init made ($writers wrote the first policy); each holds its Lease"
}

check_federation() {
	step "2. the federation: a node holds no credential of its own"
	local g problems secrets want
	for g in "${GROUPS_[@]}"; do
		k -n hugr get pods -l "platform.hugr.io/group=$g" -o json >"$WORK/pods.json"
		# the live pods, as admitted: no Secret is mounted - the doors' certificate is the csi-driver's, issued into the
		# pod with its key made there (spec 005) - none reaches the environment, a projected volume carries tokens,
		# ConfigMaps and the downward API only
		problems="$(python3 - "$WORK/pods.json" <<'PY'
import json, sys
pods = json.load(open(sys.argv[1]))["items"]
found = []
for p in pods:
    spec, name = p["spec"], p["metadata"]["name"]
    if spec.get("imagePullSecrets"):
        found.append(name + ": imagePullSecrets")
    for v in spec.get("volumes", []):
        if "secret" in v:
            found.append(name + ": the Secret volume " + v["name"])
        if v["name"] == "tls" and v.get("csi", {}).get("driver") != "csi.cert-manager.io":
            found.append(name + ": the doors' certificate is not the csi-driver's")
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
	# the namespace's Secrets: the platform's internal CA (no node mounts it), cert-manager's - the operator's
	# certificate, the console's and the front's public ones - and Helm's record of the release (its manifests and
	# values, which hold no credential: spec 005); no door's
	secrets="$(k -n hugr get secrets -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' |
		grep -v '^sh\.helm\.release\.v1\.hugr\.v[0-9]*$' | sort | tr '\n' ' ')"
	want="hugr-console hugr-front hugr-internal-ca hugr-platform-tls "
	[ "$secrets" = "$want" ] || fail "the namespace's Secrets are $secrets, want $want"
	ok "a node mounts no Secret: its doors' key is made in its pod (the csi-driver) and its tresor login is its projected ServiceAccount token ($IDP_NAME's federation); the namespace holds the internal CA, cert-manager's TLS Secrets and Helm's record"
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
	ok "with no node of b, analyst_b is refused after the wait, naming b - never seated on a"
	# spec 011 stage 2: b scales itself from 0 - analyst_b's statement is held by the front while the operator starts
	# b's node for it (the front's demand Lease, answered "starting"), then answered there. The refused statement above
	# left a demand for b: it expires first (30 s), so what wakes b is the statement below
	local demand started
	demand="hugr-demand-$(printf b | python3 -c 'import hashlib, sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest()[:16])')"
	eventually 90 "b's earlier demand expired" demand_expired "$demand"
	k -n hugr patch hng b --type=merge -p '{"spec":{"scaling":{"mode":"auto","min":0,"max":1}}}' >/dev/null ||
		fail "group b to auto, min 0"
	sleep 10 # two of the operator's looks: no demand, so b stays at 0
	status_is b '{.spec.replicas}' 0 || fail "b woke with nobody waiting: $(status b '{.spec.replicas}')"
	started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	got="$(VIA_WAIT=300 via_front analyst-b "SELECT count(*) FROM orders" 2>&1)" ||
		fail "analyst_b was not served once b's node started: $got"
	[ "$got" = "$want" ] || fail "analyst_b read $got once b's node started, on b $want"
	status_is b '{.spec.replicas}' 1 || fail "b was not scaled to 1: $(status b '{.spec.replicas}')"
	status b '{.status.scaling.last.reason}' | grep -q 'client waits' ||
		fail "b's last scaling: $(status b '{.status.scaling.last}')"
	[[ "$(status b '{.status.scaling.last.at}')" > "$started" || "$(status b '{.status.scaling.last.at}')" = "$started" ]] ||
		fail "b woke at $(status b '{.status.scaling.last.at}'), before analyst_b's statement at $started"
	# back to manual, one node, for the checks after
	k -n hugr patch hng b --type=merge -p '{"spec":{"scaling":null,"replicas":1}}' >/dev/null || fail "group b back to manual"
	eventually 300 "group b serves again" group_ready b
	follow_logs
	ok "b at 0 nodes, auto with min 0: analyst_b's statement was held while the operator started b's node, then answered on it; b back to manual"
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
# console_door <principal>: the doors' secret read as the principal through the console's /tresor, then the console's me
# with the token tresor minted for it (spec 009) - the me's JSON; no token is printed
console_door() {
	local port
	port="$(console_port)" || return 1
	# shellcheck disable=SC2046 # the IdP's flags are words
	bounded 120 "$CHECKER" $(token_flags) $([ -z "$TRESOR_SCOPE" ] || echo "-scope $TRESOR_SCOPE") \
		-client "$(principal_id "$1")" -secret-file "$WORK/secrets/principal-$1" \
		-server-name hugr-platform-console.hugr.svc -ca "$WORK/ca.crt" -console "https://127.0.0.1:$port" -door hugr_door
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
	local out me pod id held a b version
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
	json_true "$out" "not r.get('error') and r['tags']['rows']" || fail "node $a's memory: $out"
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
	# spec 007: the policy, read through the console - a catalog's grants, a role's effective rights there
	out="$(console_api admin GET "policy/views/grants?catalog=demo")" || fail "the catalog demo's grants"
	json_true "$out" "sorted(g['role'] for g in r['rows']) == ['analyst_a', 'analyst_b', 'platform_admin']" ||
		fail "the catalog demo's grants: $out"
	out="$(console_api admin GET "policy/functions/effective_rights?role=analyst_a&catalog=demo")" ||
		fail "analyst_a's effective rights in demo"
	json_true "$out" "any('orders' in json.dumps(row) for row in r['rows'])" || fail "analyst_a's effective rights: $out"
	if console_api analyst-a GET "policy/views/grants" >/dev/null 2>"$WORK/console.err"; then fail "analyst_a read the grants"; fi
	grep -q 'answered 403' "$WORK/console.err" || fail "analyst_a's grants: $(cat "$WORK/console.err")"
	ok "the policy through the console: the catalog demo's grants, analyst_a's effective rights there; analyst_a reads none of it (403)"
	# spec 007: the console's /quack is the platform's door for the SQL page's DuckDB - forwarded to the front for a
	# person signed in to the console (the front answers what is no quack message), never for anyone else
	if out="$(console_api admin POST /quack "not quack" 2>&1)"; then fail "the front took no quack message"; fi
	grep -q 'answered 400' <<<"$out" && grep -q 'not a quack message' <<<"$out" || fail "the console's /quack: $out"
	[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 --cacert "$WORK/ca.crt" -X POST --data 'x' \
		--connect-to "hugr-platform-console.hugr.svc:443:127.0.0.1:$(console_port)" https://hugr-platform-console.hugr.svc/quack)" = 401 ] ||
		fail "the console's /quack forwards a request of nobody signed in"
	ok "the console's /quack reaches the front's quack door for a person signed in to the console, nobody else (401)"

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

	# what a node starts with (spec 010): the console's document for group a is the one its nodes were served - the
	# operator serves it (they run), naming the platform's tresor, the group's own identity and the first policy
	out="$(console_api admin GET groups/a/bootstrap)" || fail "group a's bootstrap document"
	json_true "$out" "r['served'] and r['document']['node_group'] == 'a' and r['document']['node_label'] == '<pod>' and r['document']['tresor']['host'] == a[0] and r['document']['tresor']['identity']['client_id'] == a[1] and sorted(g['name'] for g in r['document']['first_policy']['resource_groups']) == ['a', 'b']" \
		"$TRESOR_HOST" "$(group_client_id a)" || fail "group a's bootstrap document: $out"
	ok "group a's bootstrap in the console: the document its nodes are served - the platform's tresor, the group's identity, the first policy's groups a and b"

	# the profile from the console's form (spec 010 stage 2): a source for group b's nodes attached, then detached - each
	# one CLUSTER statement the console built and a node judged, hot; the profile lists it between the two
	out="$(console_api admin POST cluster/profile '{"action": "attach", "path": ":memory:", "alias": "console_scratch", "type": "duckdb", "group": "b"}')" ||
		fail "the profile's attach from the console"
	json_true "$out" "r['class'] == 'hot' and r['version'] > 0" || fail "the profile's attach from the console: $out"
	version="$(json_get "$out" "r['version']")"
	out="$(console_api admin GET cluster)" || fail "the cluster's profile"
	json_true "$out" "any(i.get('name') == 'console_scratch' for i in r['profile']['rows'])" ||
		fail "the profile lists no console_scratch of group b: $out"
	out="$(console_api admin POST cluster/profile '{"action": "detach", "alias": "console_scratch", "group": "b"}')" ||
		fail "the profile's detach from the console"
	json_true "$out" "r['class'] == 'hot' and r['version'] > int(a[0])" "$version" || fail "the profile's detach: $out"
	ok "the profile changed from the console's form: console_scratch attached for group b (version $version), listed, detached"

	# the doors through tresor (spec 009): tresor's discovery answered under the console's own /tresor, and the doors'
	# secret hugr-init wrote gives the admin a token tresor mints for the nodes - which the node behind the console takes
	# as the admin's, through the policy's client for tresor
	out="$(console_get /tresor/.well-known/duckdb-secrets)" || fail "tresor's discovery through the console"
	json_true "$out" "r['api'] == 'https://hugr-platform-console.hugr.svc/tresor' and r['protocol'] == 'duckdb-secrets/1'" ||
		fail "tresor's discovery through the console: $out"
	if [ -n "$TRESOR_EXCHANGE_CLIENT" ]; then
		me="$(console_door admin)" || fail "the doors' secret through tresor"
		json_true "$me" "all(b in r['rights']['bundles'] for b in ('cluster', 'policy', 'passthrough'))" ||
			fail "the doors' minted token's rights: $me"
		ok "the doors through tresor: its discovery under the console's /tresor; hugr_door read by the admin there is a token tresor minted for the nodes, and the console's node takes it as the admin's (cluster, policy, passthrough)"
	else
		ok "tresor's discovery under the console's /tresor; the doors' token (On-Behalf-Of) is not set up on $IDP_NAME"
	fi

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
		# hugr-init's Jobs still there (its catalog phase's Job is gone once it succeeded)
		k -n hugr logs -l app.kubernetes.io/component=init --tail -1 --prefix
		# the stack's services; OpenBao's dev mode prints its own root token, on purpose (spec 004): its namespace is
		# not read
		for ns in idp "$TRESOR_NS" "$KISTA_NS" db; do
			for pod in $(k -n "$ns" get pods -o jsonpath='{.items[*].metadata.name}'); do
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

# check_upgrade: the release upgraded to itself (spec 008's first step): the CRDs' pre-upgrade hook applies the
# release's CRDs, hugr-init's catalog phase runs again and makes nothing twice; the platform serves throughout
check_upgrade() {
	step "11. an upgrade of the release to itself: the CRDs applied before it, nothing made twice, the platform serves"
	local revision out
	revision="$(helm_ status hugr -n hugr -o json | python3 -c 'import json, sys; print(json.load(sys.stdin)["version"] + 1)')"
	chart_release
	[ "$(helm_ status hugr -n hugr -o json | python3 -c 'import json, sys; print(json.load(sys.stdin)["version"])')" = "$revision" ] ||
		fail "the release is not at revision $revision"
	# the hook's resources are gone after it succeeded (hook-succeeded): its CRDs' field manager says it ran
	k get crd hugrplatforms.platform.hugr.io --show-managed-fields -o json | python3 -c '
import json, sys
managers = {f["manager"] for f in json.load(sys.stdin)["metadata"]["managedFields"]}
sys.exit(0 if "hugr-init" in managers else 1)' || fail "the CRDs were not applied by the pre-upgrade hook"
	! k get clusterrole hugr-hugr-crds >/dev/null 2>&1 || fail "the hook's right outlived the upgrade"
	out="$(as analyst-a "$(first_pod a)" "SELECT count(*) FROM orders")" || fail "analyst_a's read after the upgrade"
	[ "$out" = 3 ] || fail "analyst_a after the upgrade: $out"
	ok "revision $revision: the CRDs applied by the pre-upgrade hook (its right gone after), analyst_a still reads"
}

# check_console_group: spec 006 - the console makes a group (its identity given beforehand: the IdP's client, holding
# the nodes' role tresor's actor rule names, is the installation's), whose node drains itself until the policy has its
# resource group; the console adds it by hand on a node under the admin's token, a principal of a role bound there is
# served, then the group leaves the policy (its node drains itself again) and is deleted (its node drains and goes)
check_console_group() {
	step "13. a group from the console (spec 006): made, added to the policy by hand, serving, removed, deleted"
	local g="$CONSOLE_GROUP" out pod got body
	body="{\"name\": \"$g\", \"size\": \"small\", \"replicas\": 1, \"clientID\": \"$(group_client_id "$g")\", \"drainSeconds\": 10}"
	out="$(console_api admin GET groups)" || fail "the fleet"
	json_true "$out" "r['newGroup']['available'] and r['newGroup']['subjectPrefix'] == 'system:serviceaccount:hugr:hugr-node-'" ||
		fail "the fleet's new group: $out"
	if console_api analyst-a POST groups "$body" >/dev/null 2>"$WORK/console.err"; then fail "analyst_a made a group"; fi
	grep -q 'answered 403' "$WORK/console.err" || fail "analyst_a's new group: $(cat "$WORK/console.err")"
	console_api admin POST groups "$body" >/dev/null || fail "the console made no group $g"
	[ "$(k -n hugr get hng "$g" -o jsonpath='{.spec.tls.csi.issuerRef.name}')" = hugr-internal ] ||
		fail "group $g's certificate is not the chart's groups' own"
	[ -n "$(k -n hugr get hng "$g" -o jsonpath='{.metadata.annotations.platform\.hugr\.io/changed-by}')" ] ||
		fail "group $g does not name who made it"
	eventually 240 "group $g's node drains itself: the policy has no resource group $g" draining_is "$g" True
	follow_logs
	out="$(console_api admin GET "groups/$g/policy")" || fail "group $g's place in the policy"
	json_true "$out" "not r['inPolicy'] and r['resourceGroup'] == a[0] and 'analyst_b' in r['roles']" "$g" ||
		fail "group $g's place in the policy: $out"
	ok "the console made group $g (the chart's certificate, who made it named); its node entered and drains itself: the policy has no resource group $g"

	out="$(console_api admin POST "groups/$g/policy" '{"roles": ["analyst_b", "node_breakglass"]}')" ||
		fail "group $g added to the policy"
	json_true "$out" "r['inPolicy'] and sorted(r['bound']) == ['analyst_b', 'node_breakglass']" || fail "group $g in the policy: $out"
	eventually 120 "group $g resumes by itself" draining_is "$g" False
	eventually 120 "group $g serves" group_ready "$g"
	pod="$(first_pod "$g")"
	got="$(as analyst-b "$pod" "SELECT count(*) FROM orders")" || fail "analyst_b's read on $pod"
	[ "$got" -gt 0 ] 2>/dev/null || fail "analyst_b on $g: $got"
	if got="$(as analyst-a "$pod" "SELECT count(*) FROM orders" 2>&1)"; then fail "analyst_a is served on $g"; fi
	grep -q 'code = Unavailable' <<<"$got" || fail "analyst_a on $g: $got"
	ok "the console added $g to the policy (analyst_b, node_breakglass bound): the group resumed by itself, analyst_b reads on $pod, analyst_a is refused there"

	console_api admin DELETE "groups/$g/policy" "{\"resourceGroup\": \"$g\"}" >/dev/null || fail "group $g removed from the policy"
	eventually 120 "group $g's node drains itself again" draining_is "$g" True
	console_api admin DELETE "groups/$g" >/dev/null || fail "group $g deleted"
	eventually 120 "the node $pod drained and went" went_drained "$pod"
	eventually 60 "group $g is gone" no_group "$g"
	out="$(console_api admin GET groups)" || fail "the fleet"
	json_true "$out" "sorted(g['name'] for g in r['groups']) == ['a', 'b']" || fail "the fleet after the delete: $out"
	ok "removed from the policy, $g's node drained itself; deleted, it drained and went, and the fleet is a and b again"
}

# json_body <python expression over a>: a request's JSON, the values a[0], a[1], ... - never part of the expression
json_body() { python3 -c 'import json, sys; a = sys.argv[2:]; print(json.dumps(eval(sys.argv[1])))' "$@"; }
# profile_applied <version>: every node of a and b reports the profile's version applied
profile_applied() {
	local pod
	for pod in $(pods a) $(pods b); do lease_is "$pod" profile-applied "$1" || return 1; done
}
# refused_with <code> <principal> <method> <path> [body]: the console refuses it with that HTTP status
refused_with() {
	local code="$1"
	shift
	if console_api "$@" >/dev/null 2>"$WORK/console.err"; then return 1; fi
	grep -q "answered $code" "$WORK/console.err"
}

# check_console_sources: spec 007 stage 2b - a group's source set and its check through the console: a second source
# the analysts reach, group b's set narrowed to sales, the check names sales2 left out with the node's repair, the
# repair run through the console (only the text the check answers now), analyst_b reading through it on b's node;
# then the set reset and the source gone
check_console_sources() {
	step "14. a group's sources from the console (spec 007 stage 2b): the set, the check, a repair run by hand"
	local out version repair pod body
	if ! refused_with 403 analyst-a GET groups/b/sources; then fail "analyst_a read group b's sources: $(cat "$WORK/console.err")"; fi
	out="$(console_api admin GET groups/b/sources)" || fail "group b's sources"
	json_true "$out" "r['set'] is None and 'sales' in r['profile'] and not r.get('checkError') and any(s['source'] == 'sales' and 'analyst_b' in s['roles'] for s in r['rows'])" ||
		fail "group b's sources: $out"
	ok "group b's sources: no set (every source), sales reached by analyst_b; analyst_a reads none of it (403)"

	# a second source of the cluster, and an object of demo over it: the analysts reach it by their catalog grants
	# no path: the secret names the source, as the seed's sales
	out="$(console_api admin POST cluster/profile '{"action": "attach", "alias": "sales2", "type": "postgres", "secret": "sales"}')" ||
		fail "the profile's ATTACH sales2"
	json_true "$out" "r['ok']" || fail "the ATTACH sales2: $out"
	version="$(json_get "$out" "r['version']")"
	eventually 120 "every node applies version $version (sales2)" profile_applied "$version"
	as admin "$(first_pod a)" "ADD TABLE sales2.public.orders AS demo.orders2" >/dev/null || fail "demo.orders2 over sales2"

	out="$(console_api admin POST cluster/profile '{"action": "sources", "group": "b", "sources": ["sales"]}')" ||
		fail "group b's set (sales)"
	json_true "$out" "r['ok']" || fail "group b's set: $out"
	out="$(console_api admin GET groups/b/sources)" || fail "group b's sources after its set"
	json_true "$out" "r['set'] is not None and r['set']['names'] == ['sales'] and not r['set']['all'] and not r.get('checkError')" ||
		fail "group b's set: $out"
	json_true "$out" "any(s['source'] == 'sales2' and s['listed'] is False and not s.get('elsewhere') and 'analyst_b' in s['roles'] and s['repair'] for s in r['rows'])" ||
		fail "group b's check does not name sales2 left out with a repair: $out"
	repair="$(json_get "$out" "next(s['repair'] for s in r['rows'] if s['source'] == 'sales2')")"
	ok "group b's set is (sales): its check names sales2, which analyst_b reaches, left out - the repair is the node's: $repair"

	# the repair runs only as the check answers it now: another text is refused, and so is a source with no repair
	body=$(json_body '{"source": "sales2", "repair": a[0] + " "}' "$repair")
	if ! refused_with 409 admin POST groups/b/sources/repair "$body"; then
		fail "a repair the check did not answer ran: $(cat "$WORK/console.err")"
	fi
	body=$(json_body '{"source": "sales", "repair": a[0]}' "$repair")
	if ! refused_with 409 admin POST groups/b/sources/repair "$body"; then
		fail "a listed source was repaired: $(cat "$WORK/console.err")"
	fi
	body=$(json_body '{"source": "sales2", "repair": a[0]}' "$repair")
	out="$(console_api admin POST groups/b/sources/repair "$body")" ||
		fail "the repair of sales2"
	json_true "$out" "r['ok']" || fail "the repair of sales2: $out"
	version="$(json_get "$out" "r['version']")"
	out="$(console_api admin GET groups/b/sources)" || fail "group b's sources after the repair"
	json_true "$out" "sorted(r['set']['names']) == ['sales', 'sales2'] and all(s['listed'] for s in r['rows'] if s['source'] in ('sales', 'sales2'))" ||
		fail "group b after the repair: $out"
	if ! refused_with 409 admin POST groups/b/sources/repair "$body"; then
		fail "a repair ran twice: $(cat "$WORK/console.err")"
	fi
	pod="$(first_pod b)"
	eventually 120 "b's node $pod applies version $version" lease_is "$pod" profile-applied "$version"
	[ "$(as analyst-b "$pod" "SELECT count(*) FROM orders2")" = 3 ] || fail "analyst_b's read of orders2 on $pod"
	ok "the repair ran as the check answered it (another text, a listed source and a second run refused, 409): b's set is (sales, sales2), analyst_b reads orders2 on $pod"

	out="$(console_api admin POST cluster/profile '{"action": "reset-sources", "group": "b"}')" ||
		fail "group b's set reset"
	json_true "$out" "r['ok']" || fail "group b's set reset: $out"
	as admin "$(first_pod a)" "DROP VIRTUAL TABLE demo.orders2" >/dev/null || fail "demo.orders2 dropped"
	out="$(console_api admin POST cluster/profile '{"action": "detach", "alias": "sales2"}')" ||
		fail "the profile's DETACH sales2"
	json_true "$out" "r['ok']" || fail "the DETACH sales2: $out"
	out="$(console_api admin GET groups/b/sources)" || fail "group b's sources at the end"
	json_true "$out" "r['set'] is None and 'sales2' not in r['profile']" || fail "group b's sources at the end: $out"
	ok "group b's set reset, demo.orders2 dropped and sales2 detached: b attaches every source again"
}

# reads <principal> <pod> <sql> <answer>: the statement answers that
reads() { [ "$(as "$1" "$2" "$3")" = "$4" ]; }

# grant_edit <principal> <path> <form json> [version] [statements json]: the editor's request body sent
grant_edit() {
	local body
	body=$(json_body 'dict(form=json.loads(a[0]), version=a[1], **({"statements": json.loads(a[2])} if a[2] else {}))' "$3" "${4:-}" "${5:-}")
	console_api "$1" POST "$2" "$body"
}

# check_console_grants: spec 007 stage 2c - an object grant written through the console's editor: read (none), the
# statements previewed, checked with the node, written as shown; analyst_a then reads what it says (a row filter with a
# quote in it, a column left out, a mask); a stale or another edit refused; revoked, analyst_a reads the catalog
# grant's rows again
check_console_grants() {
	step "15. an object grant from the console's editor (spec 007 stage 2c): previewed, checked, written, read, revoked"
	local out form version statements pod body
	pod="$(first_pod a)"
	[ "$(as analyst-a "$pod" "SELECT count(*) FROM orders")" = 3 ] || fail "analyst_a before the object grant"
	out="$(console_api admin GET "policy/grant?level=object&role=analyst_a&catalog=demo&target=orders")" ||
		fail "analyst_a's grant on demo.orders"
	json_true "$out" "not r['exists'] and r['version'] == ''" || fail "analyst_a's grant on demo.orders: $out"
	form=$(json_body '{"level": "object", "role": "analyst_a", "catalog": "demo", "target": "orders", "caps": ["select"], "rls": a[0], "columns": {"mode": "except", "names": ["id"]}, "masks": [{"column": "amount", "mask": "0"}]}' "region = 'north'")

	out="$(console_api admin POST policy/grant/check "$form")" || fail "the grant checked with the node"
	json_true "$out" "r['rows'] and all(row['status'] == 'ok' for row in r['rows']) and any(row['item'] == 'column' and row['name'] == 'amount' and row['message'] for row in r['rows'])" ||
		fail "the check of analyst_a's grant: $out"
	body=$(json_body 'dict(json.loads(a[0]), rls="nope > 1")' "$form")
	out="$(console_api admin POST policy/grant/check "$body")" ||
		fail "a broken filter checked"
	json_true "$out" "any(row['item'] == 'rls' and row['status'] == 'refused' for row in r['rows'])" ||
		fail "the check passed a filter that does not bind: $out"
	ok "Check with the node: the grant is accepted (amount masked), a filter that does not bind is refused - nothing written"

	out="$(grant_edit admin policy/grant/preview "$form")" || fail "the preview of analyst_a's grant"
	json_true "$out" "len(r['statements']) == 1 and r['statements'][0].startswith('GRANT OBJECT ') and \"RLS 'region = ''north'''\" in r['statements'][0] and 'MASK ' in r['statements'][0]" ||
		fail "the preview: $out"
	statements="$(json_get "$out" "json.dumps(r['statements'])")"
	# what runs is what was shown
	body=$(json_body '{"form": json.loads(a[0]), "version": "", "statements": ["GRANT OBJECT demo.orders TO ROLE analyst_a"]}' "$form")
	if ! refused_with 409 admin POST policy/grant "$body"; then
		fail "statements not shown were written: $(cat "$WORK/console.err")"
	fi
	out="$(grant_edit admin policy/grant "$form" "" "$statements")" || fail "analyst_a's grant written"
	json_true "$out" "not r.get('refused') and len(r['answers']) == 1 and r['answers'][0] and r['answers'][0]['ok']" ||
		fail "analyst_a's grant written: $out"
	# a node reads a written policy at its next freshness check
	eventually 60 "analyst_a reads 2 rows under the filter" reads analyst-a "$pod" "SELECT count(*) FROM orders" 2
	[ "$(as analyst-a "$pod" "SELECT count(*) FROM orders WHERE amount = 0")" = 2 ] || fail "analyst_a's amount is not masked"
	if out="$(as analyst-a "$pod" "SELECT id FROM orders" 2>&1)"; then fail "analyst_a reads id: $out"; fi
	ok "the grant written as previewed (RLS 'region = ''north''', * EXCLUDE (id), amount masked): analyst_a reads 2 rows, amount 0, no id"

	# a new grant over it, a stale version, statements other than its own: each refused, nothing written
	body=$(json_body '{"form": json.loads(a[0]), "version": ""}' "$form")
	if ! refused_with 409 admin POST policy/grant/preview "$body"; then
		fail "a new grant over analyst_a's: $(cat "$WORK/console.err")"
	fi
	out="$(console_api admin GET "policy/grant?level=object&role=analyst_a&catalog=demo&target=orders")" ||
		fail "analyst_a's grant read again"
	json_true "$out" "r['exists'] and 'north' in r['grant']['rls'] and r['grant']['columns'] == {'mode': 'except', 'names': ['id']} and len(r['grant']['masks']) == 1 and r['grant']['masks'][0]['column'] == 'amount' and r['version']" ||
		fail "analyst_a's grant read again: $out"
	version="$(json_get "$out" "r['version']")"
	body=$(json_body '{"form": json.loads(a[0]), "version": "stale"}' "$form")
	if ! refused_with 409 admin POST policy/grant/preview "$body"; then
		fail "a stale edit: $(cat "$WORK/console.err")"
	fi
	ok "read back as written (its filter, test and mask); a new grant over it and a stale edit refused (409)"

	# revoked through the editor: the catalog grant's rows again
	form=$(json_body 'dict(json.loads(a[0]), op="revoke")' "$form")
	out="$(grant_edit admin policy/grant/preview "$form" "$version")" || fail "the revoke previewed"
	json_true "$out" "r['statements'] == [a[0]]" 'REVOKE OBJECT "demo"."orders" FROM ROLE "analyst_a"' || fail "the revoke: $out"
	statements="$(json_get "$out" "json.dumps(r['statements'])")"
	out="$(grant_edit admin policy/grant "$form" "$version" "$statements")" || fail "the revoke written"
	json_true "$out" "not r.get('refused') and r['answers'][0]['ok']" || fail "the revoke written: $out"
	eventually 60 "analyst_a reads the 3 rows again" reads analyst-a "$pod" "SELECT count(*) FROM orders" 3
	out="$(console_api admin GET "policy/grant?level=object&role=analyst_a&catalog=demo&target=orders")" ||
		fail "analyst_a's grant after the revoke"
	json_true "$out" "not r['exists']" || fail "analyst_a's grant after the revoke: $out"
	ok "revoked through the editor: analyst_a reads demo.orders whole again (3 rows)"
}

# rg_edit <path> <form json> <version> [statements json]: the resource group editor's request body sent as the admin
rg_edit() {
	local body
	body=$(json_body 'dict(form=json.loads(a[0]), version=a[1], **({"statements": json.loads(a[2])} if a[2] else {}))' "$2" "$3" "${4:-}")
	console_api admin POST "$1" "$body"
}
# rg_write <form json> <version>: previewed, then written as shown; the write's answer
rg_write() {
	local out statements
	out="$(rg_edit policy/resource-group/preview "$1" "$2")" || return 1
	statements="$(json_get "$out" "json.dumps(r['statements'])")"
	rg_edit policy/resource-group "$1" "$2" "$statements"
}
# rg_clear_limit <group> <limit>: the limit read and cleared from the console, once both calls landed where they may
rg_clear_limit() {
	local out version form
	out="$(rg_read "$1")" || return 1
	json_true "$out" "r['group']['limits'][a[0]] is not None" "$2" || return 0 # cleared by a try cut after its write
	version="$(json_get "$out" "r['version']")"
	form="$(json_get "$out" "json.dumps(dict(r['group'], limits=dict(r['group']['limits'], **{a[0]: None})))" "$2")"
	out="$(rg_write "$form" "$version")" || return 1
	json_true "$out" "not r.get('refused') and r['answers'][0]['ok']"
}
# rg_read <name>: the resource group as the console reads it
rg_read() { console_api admin GET "policy/resource-group?name=$1"; }

# check_console_resource_groups: spec 013 - a resource group edited from the console: a's limit written, and the next
# session of analyst_a's runs under it; cleared, it reads as before; a group no group of nodes serves made, a role bound,
# then dropped; a's own drop refused (its nodes would drain)
check_console_resource_groups() {
	step "16. resource groups from the console (spec 013): a limit, a group of no nodes, a drop refused"
	local out version form pod body
	pod="$(first_pod a)"
	# 3^5 = 243 rows: above the limit set below, which stays above what the console reads
	local wide="SELECT o1.id FROM orders o1, orders o2, orders o3, orders o4, orders o5"
	if ! refused_with 403 analyst-a GET "policy/resource-group?name=a"; then fail "analyst_a read a resource group: $(cat "$WORK/console.err")"; fi
	out="$(rg_read a)" || fail "resource group a"
	json_true "$out" "r['exists'] and r['nodeGroups'] == ['a'] and 'analyst_a' in r['group']['roles'] and r['version']" ||
		fail "resource group a: $out"
	version="$(json_get "$out" "r['version']")"
	form="$(json_get "$out" "json.dumps(dict(r['group'], limits=dict(r['group']['limits'], max_result_rows='100')))")"
	out="$(rg_write "$form" "$version")" || fail "a's limit written"
	json_true "$out" "not r.get('refused') and len(r['statements']) == 1 and r['statements'][0].startswith('CREATE OR REPLACE RESOURCE GROUP \"a\"') and 'max_result_rows 100' in r['statements'][0] and r['answers'][0]['ok']" ||
		fail "a's limit written: $out"
	# a session resolves its groups when it opens: the next one of analyst_a's hands out 100 rows, then refuses
	eventually 60 "analyst_a's next session under max_result_rows 100" refuses_rows analyst-a "$pod" "$wide"
	[ "$(as analyst-a "$pod" "SELECT count(*) FROM orders")" = 3 ] || fail "analyst_a's count under the limit"
	# the admin is in a too, and on a's nodes the console's own reads run under a's limits (duckdb-acl spec 096): 100
	# rows is above every policy view the editor reads here, so the console reads on any node
	eventually 60 "a's limit cleared from the console" rg_clear_limit a max_result_rows
	eventually 60 "analyst_a reads every row again" reads_rows analyst-a "$pod" "$wide" 243
	ok "a's max_result_rows 100 written from the console: analyst_a's next session got 100 of 243 rows and the refusal; cleared, all 243"

	out="$(rg_read adhoc)" || fail "resource group adhoc"
	json_true "$out" "not r['exists'] and r['nodeGroups'] == [] and 'analyst_b' in r['roles']" || fail "resource group adhoc: $out"
	out="$(rg_write '{"name": "adhoc", "limits": {"max_sessions": "3"}, "comment": "a night'"'"'s batch", "default": false, "roles": ["analyst_b"]}' "")" ||
		fail "adhoc made"
	json_true "$out" "not r.get('refused') and len(r['answers']) == 2 and all(x['ok'] for x in r['answers'])" || fail "adhoc made: $out"
	out="$(rg_read adhoc)" || fail "adhoc read back"
	json_true "$out" "r['exists'] and r['group']['limits']['max_sessions'] == '3' and r['group']['comment'] == a[0] and r['group']['roles'] == ['analyst_b']" "a night's batch" ||
		fail "adhoc read back: $out"
	version="$(json_get "$out" "r['version']")"
	# a over again with the version read: its groups of nodes serve it, so its drop is refused
	body="$(rg_read a)" || fail "resource group a"
	body=$(json_body '{"form": {"name": "a", "op": "drop", "limits": {}, "comment": "", "default": False, "roles": []}, "version": json.loads(a[0])["version"], "statements": ["DROP RESOURCE GROUP \"a\""]}' "$body")
	if ! refused_with 409 admin POST policy/resource-group "$body"; then fail "a's drop: $(cat "$WORK/console.err")"; fi
	grep -q '"served"' "$WORK/console.err" || fail "a's drop: $(cat "$WORK/console.err")"
	out="$(rg_write "$(json_get "$out" "json.dumps(dict(r['group'], op='drop'))")" "$version")" || fail "adhoc dropped"
	json_true "$out" "r['statements'] == [a[0]] and r['answers'][0]['ok']" 'DROP RESOURCE GROUP "adhoc"' || fail "adhoc dropped: $out"
	out="$(rg_read adhoc)" || fail "adhoc after its drop"
	json_true "$out" "not r['exists']" || fail "adhoc after its drop: $out"
	ok "adhoc, served by no nodes, made with a limit, a comment and analyst_b, read back, dropped; a's drop refused (its nodes would drain)"
}
# refuses_rows <principal> <pod> <sql>: the statement fails (a result past the principal's row limit)
refuses_rows() { ! as "$1" "$2" "$3" >/dev/null 2>&1; }
# reads_rows <principal> <pod> <sql> <n>: the statement answers n rows
reads_rows() { [ "$(as "$1" "$2" "$3" | wc -l | tr -d ' ')" = "$4" ]; }

# hold_sessions <principal> <pod> <n>: n sessions of the principal's own left open on the pod, as clients keep them
hold_sessions() {
	local i
	for i in $(seq "$3"); do
		as "$1" "$2" "SELECT 1" -hold >/dev/null || return 1
	done
}
# end_sessions <role> <group>: every Flight session of the role on the group's nodes ended through the console
end_sessions() {
	local pod out id
	for pod in $(pods "$2"); do
		out="$(console_api admin GET "nodes/$pod/sessions")" || return 1
		for id in $(json_get "$out" "' '.join(s['id'] for s in r['sessions'] if s['door'] == 'flight' and a[0] in (s['roles'] or []))" "$1"); do
			console_api admin POST "nodes/$pod/sessions/$id/end" >/dev/null || return 1
		done
	done
}
# profile_set <json change>: one profile change through the console, every node at its version after
profile_set() {
	local out version
	out="$(console_api admin POST cluster/profile "$1")" || return 1
	json_true "$out" "r['ok']" || return 1
	version="$(json_get "$out" "r['version']")"
	eventually 120 "every node applies version $version" profile_applied "$version"
}
replicas_is() { status_is "$1" '{.spec.replicas}' "$2"; }
# console_restart <group>: the group rolled from the console (spec 006), nobody left needing a restart after
console_restart() {
	console_api admin PATCH "groups/$1" '{"restart": true}' >/dev/null || fail "group $1 restarted from the console"
	eventually 300 "group $1 rolled: nobody needs a restart" restarts_needed "$1" 0
	eventually 300 "group $1 serves after its roll" group_ready "$1"
	follow_logs
}
last_reason_has() { status "$1" '{.status.scaling.last.reason}' | grep -q "$2"; }

# check_autoscale_load: spec 011 under load, group by group. The nodes of a and b admit 20 sessions each (a profile
# item per group); both groups go auto (1..2, growth past 50% of the sessions for 30 s, the scale-down below 30% for
# 1 min, a drained node gone after 1 min with no session). Idle, a shrinks to 1 through a drain; 12 sessions held on a's node grow
# a, and b stays; 12 on b's grow b, and a stays; a's ended, a shrinks back while b holds; b's ended, b too
check_autoscale_load() {
	step "17. autoscaling under load, per resource group (spec 011): grows on its own load, shrinks through a drain"
	local ra rb g pod scaling
	ra="$(status a '{.spec.replicas}')"
	rb="$(status b '{.spec.replicas}')"
	# a setting is a restart item: each group rolled from the console, its new nodes reporting 20 in their Leases
	for g in a b; do
		profile_set "$(json_body '{"action": "set", "setting": "acl_max_sessions", "value": "20", "group": a[0]}' "$g")" ||
			fail "acl_max_sessions 20 in group $g"
		console_restart "$g"
		for pod in $(pods "$g"); do lease_is "$pod" sessions-max 20 || fail "$pod admits $(lease "$pod" sessions-max) sessions, want 20"; done
	done
	# `idle` (a node with no session at all goes) stays the default 30 min: the sessions below are held on one node, as
	# a client keeps them, and the empty node beside it is what the load-based drain is to judge
	scaling='{"spec":{"scaling":{"mode":"auto","min":1,"max":2,"cooldown":"30s",
		"scaleUp":{"sessionsUsed":50,"for":"30s"},"scaleDown":{"load":30,"for":"1m","idle":"1m"}}}}'
	for g in a b; do
		k -n hugr patch hng "$g" --type=merge -p "$scaling" >/dev/null || fail "group $g to auto"
	done
	ok "a and b auto (1..2): growth past 50% of 20 sessions a node for 30 s, the scale-down below 30% for 1 min"

	# idle, a (2 nodes) is above what its load needs: one node drained, gone once idle - b, at its minimum, stays
	eventually 420 "idle a shrinks to 1" replicas_is a 1
	last_reason_has a 'drained for the scale-down' || fail "a's shrink: $(status a '{.status.scaling.last}')"
	eventually 300 "a's drained node went" group_ready a
	replicas_is b 1 || fail "b moved while idle: $(status b '{.status.scaling.last}')"
	ok "idle, a shrank from $ra to 1 through a drained node ($(status a '{.status.scaling.last.reason}')); b stayed at 1"

	pod="$(first_pod a)"
	hold_sessions analyst-a "$pod" 12 || fail "analyst_a's sessions on $pod"
	eventually 240 "a grows under analyst_a's sessions" replicas_is a 2
	last_reason_has a 'under load' || fail "a's growth: $(status a '{.status.scaling.last}')"
	eventually 300 "a's second node serves" group_ready a
	replicas_is b 1 || fail "b grew on a's load: $(status b '{.status.scaling.last}')"
	ok "12 sessions on $pod: a grew to 2 ($(status a '{.status.scaling.last.reason}')); b stayed at 1"

	pod="$(first_pod b)"
	hold_sessions analyst-b "$pod" 12 || fail "analyst_b's sessions on $pod"
	eventually 240 "b grows under analyst_b's sessions" replicas_is b 2
	eventually 300 "b's second node serves" group_ready b
	replicas_is a 2 || fail "a moved while its sessions held: $(status a '{.status.scaling.last}')"
	ok "12 sessions on $pod: b grew to 2; a held at 2 (its 12 sessions on two nodes stay above 30%)"

	end_sessions analyst_a a || fail "analyst_a's sessions ended"
	eventually 420 "a shrinks back once its sessions ended" replicas_is a 1
	last_reason_has a 'drained for the scale-down' || fail "a's shrink: $(status a '{.status.scaling.last}')"
	replicas_is b 2 || fail "b moved on a's end: $(status b '{.status.scaling.last}')"
	ok "a's sessions ended: a shrank back to 1 through a drain; b held at 2 under its own load"

	end_sessions analyst_b b || fail "analyst_b's sessions ended"
	eventually 420 "b shrinks back once its sessions ended" replicas_is b 1
	ok "b's sessions ended: b shrank back to 1"

	# as before: manual, the counts the check found, the nodes' own session limit
	k -n hugr patch hng a --type=merge -p "{\"spec\":{\"scaling\":null,\"replicas\":$ra}}" >/dev/null || fail "a back to manual"
	k -n hugr patch hng b --type=merge -p "{\"spec\":{\"scaling\":null,\"replicas\":$rb}}" >/dev/null || fail "b back to manual"
	for g in a b; do
		profile_set "$(json_body '{"action": "reset", "setting": "acl_max_sessions", "group": a[0]}' "$g")" ||
			fail "acl_max_sessions reset in group $g"
		eventually 300 "group $g serves at its count" group_ready "$g"
		console_restart "$g"
	done
	follow_logs
	ok "a and b back to manual ($ra and $rb nodes), the nodes' session limit their own"
}

# check_rollout: a new node image - the same build under another name - rolls group by group (spec 008): the default
# group a is the canary, b waits on its image until a is done, then moves; the status ends Complete and the fleet
# serves
check_rollout() {
	step "12. a new node image: the canary group first, then the next, one at a time; the rollout ends Complete"
	local from to seen="" phase group
	from="$(img hugr-node)"
	to="${from%:demo}:demo-next"
	provider_retag "$from" "$to"
	k -n hugr patch hugrplatform hugr --type merge -p "{\"spec\":{\"node\":{\"image\":\"$to\"}}}" >/dev/null ||
		fail "the platform's node image could not be set"
	local deadline=$((SECONDS + 900))
	while [ "$SECONDS" -lt "$deadline" ]; do
		phase="$(k -n hugr get hugrplatform hugr -o jsonpath='{.status.upgrade.phase}' 2>/dev/null || true)"
		group="$(k -n hugr get hugrplatform hugr -o jsonpath='{.status.upgrade.group}' 2>/dev/null || true)"
		case "$phase" in
		Canary | Groups)
			[[ " $seen " == *" $group "* ]] || seen="$seen $group"
			if [ "$group" = a ] && [ "$(k -n hugr get deploy hugr-node-b -o jsonpath='{.spec.template.spec.containers[0].image}')" != "$from" ]; then
				fail "b moved while the canary a rolled"
			fi
			;;
		Complete) break ;;
		Blocked) fail "the rollout blocked: $(k -n hugr get hugrplatform hugr -o jsonpath='{.status.upgrade.reason}')" ;;
		esac
		sleep 3
	done
	[ "$phase" = Complete ] || fail "the rollout did not complete in 15 minutes (phase $phase, group $group)"
	[ "$seen" = " a b" ] || fail "the groups rolled in the order:$seen - want the default group a, then b"
	[ "$(k -n hugr get hugrplatform hugr -o jsonpath='{.status.image}')" = "$to" ] || fail "status.image is not $to"
	local g
	for g in a b; do
		[ "$(k -n hugr get deploy "hugr-node-$g" -o jsonpath='{.spec.template.spec.containers[0].image}')" = "$to" ] ||
			fail "group $g does not run $to"
		k -n hugr rollout status "deploy/hugr-node-$g" --timeout=300s >/dev/null || fail "group $g's roll"
	done
	local out
	out="$(as analyst-a "$(first_pod a)" "SELECT count(*) FROM orders")" || fail "analyst_a's read after the rollout"
	[ "$out" = 3 ] || fail "analyst_a after the rollout: $out"
	ok "the canary a rolled first and b waited on its image, then b; the rollout is Complete on $to, and analyst_a still reads"
}

# check_boot_scripts: spec 015 (hugr-node spec 047) - a boot script from the console's form: group b gets an AFTER
# SOURCES script with ON ERROR RETRY that fails (it reads a table no source has). The answer says restart and the
# group lists its nodes as needing one; the boot in order lists the script; the console restarts b, its nodes serve,
# and the group's status lists the script as failing with ProfileHeld - the node's reason only, never the text. The
# console drops it: the item clears, ProfileHeld turns false.
check_boot_scripts() {
	step "20. a boot script from the console (spec 015): written, listed in the boot, failing in the status, dropped"
	local out pod name=20_demo_fail sql="SELECT * FROM demo_no_such_table"
	out="$(console_api admin POST cluster/profile "$(json_body '{"action": "add-script", "script": a[0], "place": "after_sources", "sql": a[1] + "\n  LIMIT 1", "onError": "retry", "group": "b"}' "$name" "$sql")")" ||
		fail "the script written from the console"
	json_true "$out" "r['ok'] and r['class'] == 'restart'" || fail "the script written from the console: $out"
	eventually 120 "group b lists its nodes as needing a restart" restarts_listed b
	pod="$(first_pod b)"
	out="$(console_api admin GET "nodes/$pod/inventory")" || fail "$pod's boot in order"
	# the script is listed with its SQL as written, after every source of the profile and before the node serves
	json_true "$out" "(lambda steps, at: r['order']['rows'][at]['sql'] == a[1] + '\n  LIMIT 1' and all(i < at for i, s in enumerate(steps) if s.startswith('6 profile: source ')) and any(i > at for i, s in enumerate(steps) if s.startswith('8 serve')))([x['step'] for x in r['order']['rows']], [i for i, x in enumerate(r['order']['rows']) if x['step'].startswith('6 profile: script ' + a[0] + ' (after_sources, on error retry')][0])" "$name" "$sql" ||
		fail "$pod's boot in order: $out"
	console_restart b
	eventually 180 "group b's status lists the script as failing, ProfileHeld" script_failing b "$name"
	out="$(k -n hugr get hng b -o json)" || fail "group b's status"
	! grep -q demo_no_such_table <<<"$out" || fail "the script's text reached group b's status"
	json_true "$out" "all(f['reason'] == 'script ' + a[0] + ' failed (Catalog Error)' for f in r['status']['profile']['failing'])" "$name" ||
		fail "the failing items' reason: $out"
	as analyst-b "$(first_pod b)" "SELECT 1" >/dev/null || fail "group b serves with its script failing"
	out="$(console_api admin GET groups)" || fail "the fleet"
	json_true "$out" "any(f['name'] == a[0] for g in r['groups'] if g['name'] == 'b' for f in g['profile']['failing'])" "$name" ||
		fail "the console's group b lists no failing script: $out"
	ok "$name written for group b (restart class), listed in the boot; after the restart b serves, its status lists the script failing (Catalog Error) with ProfileHeld, never its text"

	out="$(console_api admin POST cluster/profile "$(json_body '{"action": "drop-script", "script": a[0], "group": "b"}' "$name")")" ||
		fail "the script dropped from the console"
	json_true "$out" "r['ok']" || fail "the script dropped from the console: $out"
	# the node tries a held script again after its backoff (up to 5 minutes): there it finds it gone
	eventually 420 "the failing item clears, ProfileHeld false" status_is b '{.status.conditions[?(@.type=="ProfileHeld")].status}' False
	ok "$name dropped from the console: the failing item cleared at the node's next try, ProfileHeld false"
}
# restarts_listed <group>: the group lists at least one node as needing a restart
restarts_listed() { [ -n "$(status "$1" '{.status.profile.needsRestart[*]}')" ]; }
# script_failing <group> <script>: a node of the group reports the script failing, and ProfileHeld says so
script_failing() {
	status "$1" '{.status.profile.failing[*].name}' | tr ' ' '\n' | grep -qx "$2" &&
		status_is "$1" '{.status.conditions[?(@.type=="ProfileHeld")].status}' True
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
	check_upgrade
	check_rollout
	check_console_group
	check_console_sources
	check_console_grants
	check_console_resource_groups
	check_autoscale_load
	if [ "$IDP_NAME" = Keycloak ]; then
		check_federation_link
		check_federation_token
	fi
	check_boot_scripts
	check_no_secrets
	echo
	echo "PASS: the platform in one cluster ($PROVIDER)"
	echo "finding: $CGROUP_FINDING"
}
