# shellcheck shell=bash
# The demo's checks (spec 004), each an "ok: …" line or the run fails naming it. SQL runs as a principal through the
# checker (test/demo/check: Flight SQL, the principal's token by client credentials, its secret read from a file).

CHECKER=""

checker_up() {
	CHECKER="$WORK/check"
	(cd "$HERE/check" && go build -o "$CHECKER" .) || fail "the checker did not build"
	idp_checker_up
	rm -rf "$WORK/ports" && mkdir -p "$WORK/ports"
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

# as <principal> <pod> <sql>: the statement's rows, or its refusal on stderr (exit 1); principal: analyst-a,
# analyst-b, admin
as() {
	local principal="$1" pod="$2" sql="$3" group
	group="$(k -n hugr get pod "$pod" -o jsonpath='{.metadata.labels.platform\.hugr\.io/group}')"
	[ -f "$WORK/ports/$pod" ] || forward hugr "pod/$pod" 32010 >"$WORK/ports/$pod"
	# shellcheck disable=SC2046 # the IdP's flags are words
	"$CHECKER" $(token_flags) -client "$(principal_id "$principal")" -secret-file "$WORK/secrets/principal-$principal" \
		-node "127.0.0.1:$(cat "$WORK/ports/$pod")" -server-name "hugr-node-$group.hugr.svc" -ca "$WORK/ca.crt" -sql "$sql"
}

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

check_entry() {
	step "1. the entry: through the operator, tresor by the node's own token, the catalog by its secret's type"
	local g pod
	for g in "${GROUPS_[@]}"; do
		eventually 120 "group $g ready with its resource group known" group_ready "$g"
		for pod in $(pods "$g"); do
			k -n hugr logs "$pod" >"$WORK/node.log" 2>&1
			grep -q 'step 2 bootstrap document: ok' "$WORK/node.log" && grep -q 'step 3 tresor: ok' "$WORK/node.log" &&
				grep -q 'step 5 policy catalog: ok' "$WORK/node.log" && grep -q 'hugr_node: ready' "$WORK/node.log" ||
				fail "node $pod did not enter: $(redact <"$WORK/node.log" | tail -5)"
			[ "$(k -n hugr get lease "$pod" -o jsonpath='{.spec.holderIdentity}')" = "$pod" ] || fail "no Lease of $pod"
		done
	done
	ok "every node fetched its document from the operator, logged in to tresor, attached the catalog; each holds its Lease"
}

check_federation() {
	step "2. the federation: no secret of a node anywhere"
	local g volumes
	for g in "${GROUPS_[@]}"; do
		volumes="$(k -n hugr get deploy "hugr-node-$g" -o jsonpath='{range .spec.template.spec.volumes[*]}{.name}={.secret.secretName}{" "}{end}')"
		[ "$volumes" = "tokens= tls=hugr-node-$g-tls home= temp= ca= " ] ||
			fail "group $g's pods mount more than their doors' certificate: $volumes"
		k -n hugr get deploy "hugr-node-$g" -o yaml | grep -qi 'secretKeyRef' && fail "group $g reads a secret into its env"
	done
	ok "a node's only Secret is its doors' certificate: its tresor login is its projected ServiceAccount token ($IDP_NAME's federation)"
}

check_engine() {
	step "3. the engine against the pod (2 GiB, a CPU request of $NODE_CPU, a 2 GiB spill volume)"
	local pod got want_memory want_temp
	pod="$(first_pod a)"
	got="$(as admin "$pod" "ACL NATIVE SELECT current_setting('memory_limit'), current_setting('threads'), current_setting('max_temp_directory_size')")" ||
		fail "the admin's query on $pod"
	# what duckdb shows for the values the node sets: 80% and 90% of 2 GiB in whole MiB
	want_memory="$(k -n hugr exec "$pod" -- /usr/local/bin/duckdb -noheader -csv -c "SET memory_limit='1638MiB'; SELECT current_setting('memory_limit')" | tail -1)"
	want_temp="$(k -n hugr exec "$pod" -- /usr/local/bin/duckdb -noheader -csv -c "SET max_temp_directory_size='1843MiB'; SELECT current_setting('max_temp_directory_size')" | tail -1)"
	[ "$got" = "$(printf '%s\t1\t%s' "$want_memory" "$want_temp")" ] || fail "the node's engine: $got (want $want_memory, 1, $want_temp)"
	ok "the node runs memory_limit $want_memory, threads 1, max_temp_directory_size $want_temp: 80% / the CPU / 90%"
	got="$(k -n hugr exec "$pod" -- /usr/local/bin/duckdb -noheader -csv -c "SELECT current_setting('memory_limit') || ' / ' || current_setting('threads')" | tail -1)"
	CGROUP_FINDING="plain duckdb in the 2 GiB-limit container with a CPU request of $NODE_CPU chooses memory_limit / threads = $got"
	ok "$CGROUP_FINDING (recorded in spec 001's 'Still to verify')"
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
	grep -q 'served by "a"' <<<"$got" || fail "analyst_a on b: $got"
	ok "analyst_a reads demo.orders on a node of a; a node of b refuses it, naming a"
}

check_hot() {
	step "5. a hot item: ACL CLUSTER ATTACH … IN GROUP a, written through a node of a"
	local writer pod version
	writer="$(first_pod a)"
	as admin "$writer" "ACL CLUSTER ATTACH ':memory:' AS hot_a (TYPE duckdb) IN GROUP a" >/dev/null ||
		fail "the hot ATTACH was refused"
	version="$(as admin "$writer" "ACL NATIVE SELECT acl_cluster_version()")" || fail "the profile's version on $writer"
	for pod in $(pods a); do
		eventually 60 "node $pod applies version $version" lease_is "$pod" profile-applied "$version"
		[ "$(as admin "$pod" "ACL NATIVE SELECT count(*) FROM duckdb_databases() WHERE database_name = 'hot_a'")" = 1 ] ||
			fail "node $pod has no hot_a"
	done
	eventually 30 "group a's status: version $version applied by both" \
		status_is a '{.status.profile.target}/{.status.profile.applied}' "$version/2"
	pod="$(first_pod b)"
	[ "$(as admin "$(first_pod a)" "ACL NATIVE SELECT 1")" = 1 ] # the admin's session is on a; b's node is read by its log
	k -n hugr logs "$pod" | grep -q 'hot_a' && fail "group b's node took group a's item"
	ok "every node of a attached hot_a on its tick (version $version, group a: applied 2/2); b did not"
}

check_restart() {
	step "6. a restart item, rolled: memory_limit 1GiB IN GROUP a, then the group's restartedAt"
	local writer pod got want
	writer="$(first_pod a)"
	as admin "$writer" "ACL CLUSTER SET memory_limit = '1GiB' IN GROUP a" >/dev/null || fail "the SET was refused"
	eventually 60 "group a lists both nodes as needing a restart" restarts_needed a 2
	ok "group a's status lists its 2 nodes in needsRestart"
	restart_group a
	eventually 120 "after the roll nobody needs a restart" restarts_needed a 0
	pod="$(first_pod a)"
	got="$(as admin "$pod" "ACL NATIVE SELECT current_setting('memory_limit')")" || fail "the admin's query on $pod"
	want="$(k -n hugr exec "$pod" -- /usr/local/bin/duckdb -noheader -csv -c "SET memory_limit='1GiB'; SELECT current_setting('memory_limit')" | tail -1)"
	[ "$got" = "$want" ] || fail "after the roll memory_limit is $got, want $want"
	ok "the pods rolled one at a time; the new nodes run memory_limit $got, nobody waits"
}

check_dropped_group() {
	step "7. a dropped resource group: its nodes drain themselves, and resume"
	local admin pod
	admin="$(first_pod a)"
	as admin "$admin" "DROP RESOURCE GROUP b" >/dev/null || fail "DROP RESOURCE GROUP b was refused"
	pod="$(first_pod b)"
	eventually 60 "group b says Draining" draining_is b True
	[ "$(lease "$pod" draining)" = true ] || fail "b's node does not report draining"
	ok "group b's node drained itself; the group says Draining"
	for statement in "CREATE RESOURCE GROUP b" "GRANT RESOURCE GROUP b TO ROLE analyst_b" "GRANT RESOURCE GROUP b TO ROLE platform_admin"; do
		as admin "$admin" "$statement" >/dev/null || fail "$statement was refused"
	done
	eventually 60 "group b resumes" draining_is b False
	eventually 30 "b's node admits sessions again" lease_is "$pod" draining false
	ok "recreated, the group resumed by itself"
}

check_plan() {
	step "8. the plan: past maxNodes refused by the webhook, within it a node enters by itself"
	local out
	if out="$(k -n hugr patch hng a --type merge -p '{"spec":{"replicas":4}}' 2>&1)"; then
		fail "scaling a to 4 (with b's 1, past 4) was accepted"
	fi
	grep -q 'at most 4' <<<"$out" || fail "the webhook's refusal: $out"
	ok "kubectl's scale past the plan refused: $(sed -n 's/.*\(the plan runs at most[^"]*\).*/\1/p' <<<"$out" | head -1)"
	k -n hugr patch hng a --type merge -p '{"spec":{"replicas":3}}' >/dev/null
	eventually 180 "group a runs 3 ready nodes" status_is a '{.status.readyReplicas}' 3
	ok "a third node of a entered by itself"
	k -n hugr patch hng a --type merge -p '{"spec":{"replicas":2}}' >/dev/null
	eventually 180 "group a is back to 2" deploy_is a '{.status.replicas}' 2
	ok "scaled down to 2: a node drained and went"
}

check_policy() {
	step "9. the optional admission policy: a group's pods come only from its own chain"
	k apply -k "$ROOT/config/policy" >/dev/null || fail "the policy did not apply"
	local out
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
	local file hits=""
	{
		k -n hugr logs deploy/hugr-platform --tail -1
		k -n hugr logs job/demo-seed --tail -1
		for g in "${GROUPS_[@]}"; do
			for pod in $(pods "$g"); do k -n hugr logs "$pod" --tail -1; done
		done
		k -n hugr get deploy,sa,svc,pdb,networkpolicy,lease,hng,hp,configmap,job -o yaml
	} >"$WORK/scan.txt" 2>&1
	k -n hugr get secret demo-seed >/dev/null 2>&1 && fail "the first deployment's SQL outlived its Job"
	for file in "$WORK"/secrets/*; do
		grep -qF "$(cat "$file")" "$WORK/scan.txt" && hits="$hits $(basename "$file")"
	done
	# a JWT by its shape - a header and a claims part, both base64 JSON - not three letters a certificate may hold
	grep -Eq 'eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.' "$WORK/scan.txt" && hits="$hits a-token"
	rm -f "$WORK/scan.txt"
	[ -z "$hits" ] || fail "found in the logs or the resources of the platform's namespace:$hits"
	ok "no secret of the run and no token in the operator's, the nodes' or the seed's logs, nor in the namespace's resources; the seed's SQL is gone"
}

run_checks() {
	checker_up
	check_entry
	check_federation
	check_engine
	check_placement
	check_hot
	check_restart
	check_dropped_group
	check_plan
	check_policy
	check_no_secrets
	echo
	echo "PASS: the platform in one cluster ($PROVIDER)"
	echo "finding: $CGROUP_FINDING"
}
