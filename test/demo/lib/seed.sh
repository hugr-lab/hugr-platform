# shellcheck shell=bash
# The first deployment (spec 001, spec 004): the installation's Job runs the first SQL in its own process, unprefixed,
# on the node's image - tresor's secrets for the groups' identities, then the policy catalog written directly as the
# database's operator: the IdP, the roles, a virtual catalog over the source, the resource groups, the first profile.

# the platform's CA bundle: HUGR_CA_FILE replaces the node's system roots, so it holds them plus the run's CA. The
# roots are read from the node's image in the cluster, wherever it was built.
install_ca_bundle() {
	namespace hugr
	k -n hugr delete pod ca-roots --ignore-not-found --wait >/dev/null
	k -n hugr run ca-roots --image="$(img hugr-node)" --image-pull-policy="$PULL" --restart=Never \
		--command -- cat /etc/ssl/certs/ca-certificates.crt >/dev/null || fail "the node image's system roots"
	k -n hugr wait --for=jsonpath='{.status.phase}'=Succeeded pod/ca-roots --timeout=300s >/dev/null ||
		fail "the node image's system roots: $(k -n hugr get pod ca-roots -o jsonpath='{.status.containerStatuses[0].state}')"
	k -n hugr logs ca-roots >"$WORK/bundle.crt"
	k -n hugr delete pod ca-roots --wait=false >/dev/null
	grep -q 'BEGIN CERTIFICATE' "$WORK/bundle.crt" || fail "the node image holds no system roots"
	cat "$WORK/ca.crt" >>"$WORK/bundle.crt"
	k -n hugr delete configmap hugr-ca --ignore-not-found >/dev/null
	k -n hugr create configmap hugr-ca --from-file=ca.crt="$WORK/bundle.crt" >/dev/null
}

seed() {
	step "the first deployment: tresor's secrets, the policy catalog - one Job, in its own process"
	install_ca_bundle
	local pg seeder g grants="" scope=""
	[ -z "$TRESOR_SCOPE" ] || scope=", OAUTH_SCOPE '$TRESOR_SCOPE'"
	pg="$(secret_value pg)"
	seeder="$(secret_value principal-seeder)"
	# every node's identity is the one tresor principal: the secrets go to it
	for s in hugr_policy_catalog sales; do
		grants="${grants}CALL owner.grant_secret('$s', '$NODES_PRINCIPAL', ['use']);
"
	done
	# the SQL carries the run's secrets: it goes into a Secret of the demo's cluster, never a ConfigMap or a log
	cat >"$WORK/seed.sql" <<SQL
SET GLOBAL ca_cert_file = '/etc/hugr/ca/ca.crt';
SET GLOBAL tresor_http_client = 'duckdb';
SET GLOBAL autoinstall_known_extensions = false;
SET GLOBAL autoload_known_extensions = false;
-- postgres_scanner first, from the repository: a postgres secret's type is its
SET GLOBAL allow_extension_repositories = 'allowed';
CREATE OR REPLACE EXTENSION REPOSITORY hugr WITH PREFIX '$KISTA_PREFIX';
INSTALL postgres_scanner FROM hugr;
LOAD postgres_scanner FROM hugr;
CREATE SECRET seeder (TYPE tresor, SCOPE 'tresor:$TRESOR_HOST', FLOW 'client_credentials',
    CLIENT_ID '$(principal_id seeder)', CLIENT_SECRET '$seeder', ISSUER '$IDP_ISSUER'$scope);
ATTACH 'tresor:$TRESOR_HOST' AS owner (SECRET seeder);
CREATE OR REPLACE PERSISTENT SECRET hugr_policy_catalog IN owner (TYPE postgres, HOST 'pg.db.svc', PORT '5432',
    DATABASE 'hugr', USER 'postgres', PASSWORD '$pg');
CREATE OR REPLACE PERSISTENT SECRET sales IN owner (TYPE postgres, HOST 'pg.db.svc', PORT '5432', DATABASE 'sales',
    USER 'postgres', PASSWORD '$pg');
${grants}CALL owner.grant_secret('sales', 'role:demo_seeder', ['use']);
-- what the seeder may use is listed at ATTACH: again, after its own grant
DETACH owner;
ATTACH 'tresor:$TRESOR_HOST' AS owner (SECRET seeder);
ATTACH 'host=pg.db.svc port=5432 dbname=hugr user=postgres password=$pg' AS pg (TYPE postgres);
SELECT acl_use_db('pg', 'acl', true);
SET GLOBAL acl_allow_anonymous_admin = true;
ACL ADMIN CREATE ISSUER IF NOT EXISTS idp URL '$IDP_ISSUER';
ACL ADMIN CREATE CLIENT IF NOT EXISTS demo ISSUER idp AUDIENCES ('$HUGR_AUDIENCE') ROLES FROM ('$HUGR_ROLES_CLAIM') UNMAPPED AS ROLE;
ACL ADMIN CREATE ROLE IF NOT EXISTS analyst_a;
ACL ADMIN CREATE ROLE IF NOT EXISTS analyst_b;
ACL ADMIN CREATE ROLE IF NOT EXISTS platform_admin;
ACL ADMIN GRANT ADMIN cluster TO ROLE platform_admin;
ACL ADMIN GRANT ADMIN policy TO ROLE platform_admin;
-- the demo's operator reads a node's own settings (ACL NATIVE, the break-glass): the checks' only use of it
ACL ADMIN GRANT ADMIN passthrough TO ROLE platform_admin;
-- a role with an administration scope is reached only through the client's own mapping (duckdb-acl spec 095)
ACL ADMIN MAP CLAIM 'platform_admin' FROM CLIENT demo TO ROLE platform_admin;
ACL ADMIN CLUSTER ATTACH '' AS sales (TYPE postgres, SECRET sales);
ACL ADMIN CREATE VIRTUAL CATALOG IF NOT EXISTS demo;
ACL ADMIN ADD TABLE sales.public.orders AS demo.orders;
ACL ADMIN GRANT CATALOG demo TO ROLE analyst_a WITH (select) MAIN;
ACL ADMIN GRANT CATALOG demo TO ROLE analyst_b WITH (select) MAIN;
ACL ADMIN GRANT CATALOG demo TO ROLE platform_admin WITH (select) MAIN;
SQL
	for g in "${GROUPS_[@]}"; do
		cat >>"$WORK/seed.sql" <<SQL
ACL ADMIN CREATE RESOURCE GROUP $g;
ACL ADMIN GRANT RESOURCE GROUP $g TO ROLE analyst_$g;
ACL ADMIN GRANT RESOURCE GROUP $g TO ROLE platform_admin;
SQL
	done
	echo "SELECT 'version:' || acl_cluster_version();" >>"$WORK/seed.sql"
	k -n hugr delete secret demo-seed --ignore-not-found >/dev/null
	k -n hugr create secret generic demo-seed --from-file=seed.sql="$WORK/seed.sql" >/dev/null
	rm -f "$WORK/seed.sql"
	k -n hugr delete job demo-seed --ignore-not-found --wait >/dev/null
	k apply -f - >/dev/null <<YAML
apiVersion: batch/v1
kind: Job
metadata: {name: demo-seed, namespace: hugr}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      securityContext: {runAsNonRoot: true, runAsUser: 10001, fsGroup: 10001}
      containers:
        - name: seed
          image: $(img hugr-node)
          imagePullPolicy: $PULL
          command: [/usr/local/bin/duckdb, -bail, -csv, -noheader, -c, .read /seed/seed.sql]
          volumeMounts:
            - {name: seed, mountPath: /seed, readOnly: true}
            - {name: ca, mountPath: /etc/hugr/ca, readOnly: true}
            - {name: home, mountPath: /var/lib/hugr-node}
      volumes:
        - {name: seed, secret: {secretName: demo-seed}}
        - {name: ca, configMap: {name: hugr-ca}}
        - {name: home, emptyDir: {}}
YAML
	local done=1
	k -n hugr wait --for=condition=complete job/demo-seed --timeout=300s >/dev/null 2>&1 || done=""
	# the SQL is needed no longer than its Job: the Secret holding the run's passwords goes, whatever the outcome
	k -n hugr delete secret demo-seed --ignore-not-found >/dev/null
	if [ -z "$done" ]; then
		k -n hugr logs job/demo-seed --tail 60 2>&1 | redact >&2 || true
		fail "the first deployment's Job did not complete"
	fi
	VERSION="$(k -n hugr logs job/demo-seed | sed -n 's/^"\{0,1\}version:\([0-9]*\).*/\1/p' | tail -1)"
	[ -n "$VERSION" ] || {
		k -n hugr logs job/demo-seed --tail 40 2>&1 | redact >&2
		fail "the first deployment wrote no profile version"
	}
	ok "the catalog: issuer, client, roles, the source sales, the catalog demo, the groups a and b; profile v$VERSION"
}
