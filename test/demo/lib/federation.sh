# shellcheck shell=bash
# Platform B, a source of platform A (spec 014): a second release of the umbrella in its own namespace - one group of
# one node, its quack door open - sharing A's tresor, kista, PostgreSQL server and Keycloak realm. B is a source like
# PostgreSQL: its consumer is ordinary SQL on B (a client, a mapping, a role, a grant), and A attaches it as any source,
# its token in a secret of A's tresor (minted by A's node identity, mint+exchange).
B_NS=hugr-b
B_GROUPS_=(x)
B_AUDIENCE=hugr-b
B_PRINCIPAL=group:hugr-b-nodes
# B's front, as A's nodes reach it: the Service's name, its quack port
B_DOOR="hugr-front.$B_NS.svc:9494"

b_client_id() { echo "system:serviceaccount:$B_NS:hugr-node-$1"; }

# b_chart_values: B's umbrella values - A's services, its own catalog database, secrets and groups, its nodes' audience
# B_AUDIENCE (so a token of A's own never passes at B)
b_chart_values() {
	local operator private=false
	operator="$(img hugr-platform)"
	case "$CLUSTER_ISSUER" in https://kubernetes.default.svc*) private=true ;; esac
	cat <<YAML
global:
  tresor: {host: "$TRESOR_HOST"}
  repository: {prefix: "$KISTA_PREFIX"}
  database: {host: pg.db.svc}
  idp:
    issuer: "$IDP_ISSUER"
    tresorScope: "$TRESOR_SCOPE"
  clusterIssuer: {url: "$CLUSTER_ISSUER", private: $private}
  duckdbVersion: "$DUCKDB_VERSION"
hugr-platform:
  operator:
    image: {repository: "${operator%:*}", tag: "${operator##*:}", pullPolicy: $PULL}
    replicas: 1
  internalCA: {existingSecret: hugr-internal-ca}
  console: {dnsNames: [localhost], ipAddresses: [127.0.0.1]}
  front: {dnsNames: [localhost], ipAddresses: [127.0.0.1]}
  platform:
    spec:
      node: {image: "$(img hugr-node)", imagePullPolicy: $PULL}$([ -z "$IDP_FEDERATION_AUDIENCE" ] || printf '\n      idp: {federationAudience: "%s"}' "$IDP_FEDERATION_AUDIENCE")
      heartbeat: {intervalSeconds: 5}
      front: {replicas: 1}
      catalog: {secret: hugr_b_policy_catalog}
      # the first policy needs the console's client (how an administrator gets in): B's console, by B's audience
      console:
        clientID: hugr-console
        audiences: {nodes: "$B_AUDIENCE", tresor: "$TRESOR_AUDIENCE"}
      firstPolicy:
        issuer: {name: idp, rolesClaim: "$HUGR_ROLES_CLAIM", audiences: ["$B_AUDIENCE"]}
        administrators: platform_admin
      sizes:
        small:
          resources: {requests: {cpu: 250m, memory: 1Gi}, limits: {memory: 2Gi}} # B's one node: light
          temp: {medium: local, size: 2Gi}
      plan: {maxNodes: 1, sizes: [small]}
  hugrInit:
    tresor:
      grants: ["$B_PRINCIPAL", "role:demo_seeder"]
    catalog: {database: hugr_b, login: hugr_b_catalog}
    door:
      secret: hugr_b_door
      audience: "$B_AUDIENCE"
      scopes: ["quack:$B_DOOR"]
      grants: [role:platform_admin]
  groupCertificateDuration: 24h
  groups:
YAML
	local g
	for g in "${B_GROUPS_[@]}"; do
		echo "    - {name: $g, spec: {replicas: 1, size: small, identity: {clientID: \"$(b_client_id "$g")\"}, doors: {quack: true}, drainSeconds: 10}}"
	done
}

# install_platform_b: B's release, after A's (A's services and CRDs are there); its group serving
install_platform_b() {
	step "platform B (spec 014): a second release in $B_NS - one group, its quack door open - on A's services"
	namespace "$B_NS"
	# the run's CA is B's internal CA too: B's front chains to what A's bundle trusts (an organisation's CA). A B of
	# another CA is given to A's caBundle
	k -n "$B_NS" delete secret hugr-internal-ca --ignore-not-found >/dev/null
	k -n "$B_NS" create secret tls hugr-internal-ca --cert "$WORK/ca.crt" --key "$WORK/ca.key" >/dev/null ||
		fail "B's internal CA's Secret"
	k -n "$B_NS" delete secret hugr-init-db-admin --ignore-not-found >/dev/null
	k -n "$B_NS" create secret generic hugr-init-db-admin --from-literal=username=postgres \
		--from-file=password="$WORK/secrets/pg" >/dev/null || fail "B's hugr-init administrator Secret"
	b_chart_values >"$WORK/chart-values-b.yaml"
	local cas=(--set-file "global.database.caCert=$WORK/ca.crt")
	[ "$IDP_NAME" != Keycloak ] || cas+=(--set-file "global.idp.caCert=$WORK/ca.crt")
	if ! helm_ upgrade --install hugr-b "$ROOT/charts/hugr" -n "$B_NS" -f "$WORK/chart-values-b.yaml" "${cas[@]}" \
		--wait --timeout 900s >"$WORK/chart-helm-b.log" 2>&1; then
		cat "$WORK/chart-helm-b.log" >&2
		k -n "$B_NS" get pods -o wide >&2 || true
		k -n "$B_NS" logs -l app.kubernetes.io/component=init --tail 40 --prefix 2>&1 | redact >&2 || true
		fail "platform B did not install"
	fi
	local g
	for g in "${B_GROUPS_[@]}"; do
		eventually 120 "B's operator makes group $g's Deployment" k -n "$B_NS" get "deploy/hugr-node-$g"
		if ! k -n "$B_NS" rollout status "deploy/hugr-node-$g" --timeout 300s >/dev/null 2>&1; then
			k -n "$B_NS" get pods -o wide >&2 || true
			k -n "$B_NS" logs -l "platform.hugr.io/group=$g" --tail 60 --prefix 2>&1 | redact >&2 || true
			fail "B's group $g is not ready"
		fi
	done
	eventually 120 "B's operator makes its front" k -n "$B_NS" get deploy/hugr-front
	k -n "$B_NS" rollout status deploy/hugr-front --timeout 180s >/dev/null || fail "B's front is not ready"
	ok "platform B in $B_NS: its operator, its front at $B_DOOR, group ${B_GROUPS_[*]} serving; its catalog hugr_b"
}

# seed_b: B's own data and its consumer, as B's database operator (the seed's way, in B's namespace): the source
# sales_b and its secret, the catalog shared over it, the consumer - a client for the token A's tresor mints for A's
# node (B's audience, tresor's exchange client as its azp), A's nodes' role mapped to platform_a, its grant with a
# row filter
seed_b() {
	step "B's data and its consumer: the source sales_b, the catalog shared, platform_a for A's nodes - one Job"
	cat >"$WORK/seed-b.sql" <<SQL
SET GLOBAL allow_extension_repositories = 'allowed';
CREATE OR REPLACE EXTENSION REPOSITORY hugr WITH PREFIX '$KISTA_PREFIX';
INSTALL postgres_scanner FROM hugr;
LOAD postgres_scanner FROM hugr;
CREATE OR REPLACE PERSISTENT SECRET sales_b IN owner (TYPE postgres, HOST 'pg.db.svc', PORT '5432', DATABASE 'sales_b',
    USER 'sales_reader', PASSWORD '$(secret_value pg-sales)');
CALL owner.grant_secret('sales_b', '$B_PRINCIPAL', ['use']);
CALL owner.grant_secret('sales_b', 'role:demo_seeder', ['use']);
DETACH owner;
ATTACH 'tresor:$TRESOR_HOST' AS owner (SECRET seeder);
ATTACH '' AS pg (TYPE postgres, SECRET hugr_b_policy_catalog);
SELECT acl_use_db('pg', 'acl');
SET GLOBAL acl_allow_anonymous_admin = true;
ACL ADMIN CREATE ISSUER IF NOT EXISTS idp URL '$IDP_ISSUER';
-- the checks' admin on B (B's audience): the demo's client, its platform_admin reached by the client's own mapping
ACL ADMIN CREATE CLIENT IF NOT EXISTS demo ISSUER idp AUDIENCES ('$B_AUDIENCE') ROLES FROM ('$HUGR_ROLES_CLAIM') UNMAPPED AS ROLE;
ACL ADMIN MAP CLAIM 'platform_admin' FROM CLIENT demo TO ROLE platform_admin;
-- the consumer: A's node's token, minted by A's tresor for B's audience - its azp tresor's exchange client
ACL ADMIN CREATE CLIENT IF NOT EXISTS consumer_a ISSUER idp AUDIENCES ('$B_AUDIENCE') AZP '$TRESOR_EXCHANGE_CLIENT'
    ROLES FROM ('$HUGR_ROLES_CLAIM');
ACL ADMIN CREATE ROLE IF NOT EXISTS platform_a;
ACL ADMIN MAP CLAIM 'hugr_node' FROM CLIENT consumer_a TO ROLE platform_a;
ACL ADMIN CLUSTER ATTACH '' AS sales_b (TYPE postgres, SECRET sales_b);
ACL ADMIN CREATE VIRTUAL CATALOG IF NOT EXISTS shared;
ACL ADMIN ADD TABLE sales_b.public.orders AS shared.orders;
-- what platform A may read of B: the north only
ACL ADMIN GRANT CATALOG shared TO ROLE platform_a WITH (select) MAIN RLS 'region = ''north''';
ACL ADMIN GRANT RESOURCE GROUP ${B_GROUPS_[0]} TO ROLE platform_a;
SELECT 'version:' || acl_cluster_version();
SET GLOBAL acl_allow_anonymous_admin = false;
SQL
	TJ_NS="$B_NS" tresor_job demo-seed-b "$WORK/seed-b.sql"
	local version pod
	version="$(sed -n 's/^"\{0,1\}version:\([0-9]*\).*/\1/p' "$WORK/demo-seed-b.log" | tail -1)"
	[ -n "$version" ] || {
		tail -40 "$WORK/demo-seed-b.log" | redact >&2
		fail "B's data wrote no profile version"
	}
	for pod in $(b_pods "${B_GROUPS_[0]}"); do
		eventually 120 "B's node $pod applies the profile's version $version" b_lease_is "$pod" profile-applied "$version"
	done
	ok "B's data: the source sales_b, the catalog shared; platform_a (A's nodes, by client consumer_a) reads its north"
}
b_lease_is() { [ "$(k -n "$B_NS" get lease "$1" -o jsonpath="{.metadata.annotations.platform\.hugr\.io/$2}")" = "$3" ]; }
# b_pods <group>: B's group's ready pods
b_pods() {
	k -n "$B_NS" get pods -l "platform.hugr.io/group=$1" -o json | python3 -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    ready = any(c["type"] == "Ready" and c["status"] == "True" for c in p["status"].get("conditions", []))
    if ready and not p["metadata"].get("deletionTimestamp"):
        print(p["metadata"]["name"])'
}

# as_b <principal> <pod> <sql>: as `as`, on a node of B (B's group's certificate)
as_b() {
	local principal="$1" pod="$2" sql="$3" port
	if [ ! -s "$WORK/ports/b-$pod" ]; then
		forward "$B_NS" "pod/$pod" 32010 >"$WORK/ports/b-$pod.new" && mv "$WORK/ports/b-$pod.new" "$WORK/ports/b-$pod" ||
			return 1
	fi
	port="$(cat "$WORK/ports/b-$pod")"
	# shellcheck disable=SC2046 # the IdP's flags are words
	bounded 120 "$CHECKER" $(token_flags) -client "$(principal_id "$principal")" -secret-file "$WORK/secrets/principal-$principal" \
		-node "127.0.0.1:$port" -server-name "hugr-node-${B_GROUPS_[0]}.$B_NS.svc" -ca "$WORK/ca.crt" -sql "$sql"
}
# a_sessions_on_b: platform_a's sessions on B's node (the link's connections)
a_sessions_on_b() { as_b admin "$(b_pods "${B_GROUPS_[0]}" | head -1)" "SELECT count(*) FROM platform.sessions WHERE list_contains(roles, 'platform_a')"; }
# kill_a_on_b: every session of platform_a's on B ended by B (B's operator)
kill_a_on_b() {
	local pod id
	pod="$(b_pods "${B_GROUPS_[0]}" | head -1)"
	for id in $(as_b admin "$pod" "SELECT id FROM platform.sessions WHERE list_contains(roles, 'platform_a')"); do
		as_b admin "$pod" "KILL SESSION '$id'" >/dev/null || return 1
	done
}

# check_federation_link: spec 014 - A attaches B as a source: the link's secret in A's tresor (TYPE hugr, the token
# mint+exchange'd for B's audience by the node reading it), the ATTACH a profile item from the console, an object of
# A's catalog over B's; analyst_a reads B's rows as B lets platform_a read them (north only); B sees platform_a
check_federation_link() {
	step "18. platform B as a source of A (spec 014): A's secret in tresor, the ATTACH, analyst_a reads B's north"
	local out pod
	cat >"$WORK/link-b.sql" <<SQL
CREATE OR REPLACE PERSISTENT SECRET b IN owner (TYPE hugr, SCOPE 'hugr:$B_DOOR', TOKEN 'mint+exchange://$B_AUDIENCE');
CALL owner.grant_secret('b', '$NODES_PRINCIPAL', ['use']);
SQL
	tresor_job demo-link-b "$WORK/link-b.sql"
	# a node lists its tresor's secrets for 30 s (tresor spec 004): the new secret is the node's within that
	eventually 90 "A's ATTACH of B (the node's list of its secrets refreshed)" attach_b
	out="$(cat "$WORK/attach-b.json")"
	eventually 120 "every node of A applies version $(json_get "$out" "r['version']")" profile_applied "$(json_get "$out" "r['version']")"
	pod="$(first_pod a)"
	as admin "$pod" "ADD TABLE b.main.orders AS demo.orders_b" >/dev/null || fail "demo.orders_b over B"
	eventually 60 "analyst_a reads B's north through A" reads analyst-a "$pod" "SELECT id FROM orders_b ORDER BY id" "$(printf '1\n3')"
	[ "$(a_sessions_on_b)" -ge 1 ] 2>/dev/null || fail "B lists no session of platform_a's: $(a_sessions_on_b 2>&1)"
	ok "A's link to B: a hugr secret in A's tresor and an ATTACH from the console; analyst_a reads B's rows 1, 3 - B's north for platform_a; B lists platform_a's session"
}

# check_federation_token: spec 014 - the token kept. A's own token is refused at B (B's audience is its own); past the
# token's exp, B ends A's sessions and the next read reconnects with a token minted afresh; B's consumer revoked, the
# link fails and A's other sources serve; recreated, the link serves again
check_federation_token() {
	step "19. the link's token (spec 014): B's audience its own, kept past its exp, revoked and back"
	local pod bpod got
	pod="$(first_pod a)"
	bpod="$(b_pods "${B_GROUPS_[0]}" | head -1)"
	if got="$(as_b analyst-a "$bpod" "SELECT 1" 2>&1)"; then fail "analyst_a's own token passed at B"; fi
	ok "analyst_a's token (A's audience) is refused at B"
	# past the minted token's life (2 min): B ends the link's sessions, the next read connects again - the secret read
	# again, a token minted afresh
	sleep 150
	kill_a_on_b || fail "B ended platform_a's sessions"
	eventually 60 "B holds no session of platform_a's" a_sessions_is 0
	eventually 60 "analyst_a reads B again, through a new connection" reads analyst-a "$pod" "SELECT count(*) FROM orders_b" 2
	[ "$(a_sessions_on_b)" -ge 1 ] 2>/dev/null || fail "no new session of platform_a's on B"
	ok "past the token's 2 minutes, B ended the link's sessions: the next read connected again on a fresh token"
	# B revokes its consumer: the next connect gets no session there; A's own sources keep serving
	as_b admin "$bpod" "DROP CLIENT consumer_a" >/dev/null || fail "B dropped consumer_a"
	kill_a_on_b || fail "B ended platform_a's sessions"
	eventually 60 "the link to B fails" refuses_rows analyst-a "$pod" "SELECT count(*) FROM orders_b"
	[ "$(as analyst-a "$pod" "SELECT count(*) FROM orders")" = 3 ] || fail "A's own source after B's revocation"
	as_b admin "$bpod" "CREATE CLIENT consumer_a ISSUER idp AUDIENCES ('$B_AUDIENCE') AZP '$TRESOR_EXCHANGE_CLIENT' ROLES FROM ('$HUGR_ROLES_CLAIM')" >/dev/null ||
		fail "B recreated consumer_a"
	as_b admin "$bpod" "MAP CLAIM 'hugr_node' FROM CLIENT consumer_a TO ROLE platform_a" >/dev/null || fail "B mapped hugr_node again"
	eventually 120 "the link serves again" reads analyst-a "$pod" "SELECT count(*) FROM orders_b" 2
	ok "B revoked consumer_a: the link failed and A's own source served; recreated, the link serves again"
}
attach_b() {
	console_api admin POST cluster/profile "$(json_body '{"action": "attach", "path": a[0], "alias": "b", "type": "hugr", "secret": "b"}' "$B_DOOR")" >"$WORK/attach-b.json" &&
		json_true "$(cat "$WORK/attach-b.json")" "r['ok']"
}
a_sessions_is() { [ "$(a_sessions_on_b)" = "$1" ]; }
