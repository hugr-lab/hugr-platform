# shellcheck shell=bash
# The demo's data (spec 004, spec 005): the platform installs itself - hugr-init makes the catalog's database, login and
# secret, the first node writes the first policy - and the demo's Job then writes its own data in its own process,
# unprefixed, on the node's image: the source's secret, the roles, a virtual catalog over the source, the groups'
# bindings, the first profile.

# tresor_job <name> <sql file>: the SQL in a Job of its own process on the node's image, as the seeder (tresor's
# administrator) - the file carries the run's secrets: it goes into a Secret of the demo's cluster, never a
# ConfigMap or a log, and is removed here. It trusts what a node trusts: the platform's CA bundle (the operator's -
# the system roots, the internal CA, tresor's and kista's, the database's). Its log is left in $WORK/<name>.log
tresor_job() {
	local name="$1" sql="$2" scope=""
	[ -z "$TRESOR_SCOPE" ] || scope=", OAUTH_SCOPE '$TRESOR_SCOPE'"
	{
		cat <<SQL
SET GLOBAL ca_cert_file = '/etc/hugr/ca/ca.crt';
SET GLOBAL tresor_http_client = 'duckdb';
SET GLOBAL autoinstall_known_extensions = false;
SET GLOBAL autoload_known_extensions = false;
CREATE SECRET seeder (TYPE tresor, SCOPE 'tresor:$TRESOR_HOST', FLOW 'client_credentials',
    CLIENT_ID '$(principal_id seeder)', CLIENT_SECRET '$(secret_value principal-seeder)', ISSUER '$IDP_ISSUER'$scope);
ATTACH 'tresor:$TRESOR_HOST' AS owner (SECRET seeder);
SQL
		cat "$sql"
	} >"$WORK/$name.sql"
	rm -f "$sql"
	k -n hugr delete secret "$name" --ignore-not-found >/dev/null
	k -n hugr create secret generic "$name" --from-file=job.sql="$WORK/$name.sql" >/dev/null
	rm -f "$WORK/$name.sql"
	k -n hugr delete job "$name" --ignore-not-found --wait >/dev/null
	k apply -f - >/dev/null <<YAML
apiVersion: batch/v1
kind: Job
metadata: {name: $name, namespace: hugr}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      securityContext: {runAsNonRoot: true, runAsUser: 10001, fsGroup: 10001}
      containers:
        - name: job
          image: $(img hugr-node)
          imagePullPolicy: $PULL
          command: [/usr/local/bin/duckdb, -bail, -csv, -noheader, -c, .read /job/job.sql]
          volumeMounts:
            - {name: job, mountPath: /job, readOnly: true}
            - {name: ca, mountPath: /etc/hugr/ca, readOnly: true}
            - {name: home, mountPath: /var/lib/hugr-node}
      volumes:
        - {name: job, secret: {secretName: $name}}
        - {name: ca, configMap: {name: hugr-ca-bundle}}
        - {name: home, emptyDir: {}}
YAML
	local done=1
	k -n hugr wait --for=condition=complete "job/$name" --timeout=300s >/dev/null 2>&1 || done=""
	# the SQL is needed no longer than its Job: the Secret holding the run's passwords goes, whatever the outcome
	k -n hugr delete secret "$name" --ignore-not-found >/dev/null
	k -n hugr logs "job/$name" >"$WORK/$name.log" 2>&1 || true
	if [ -z "$done" ]; then
		tail -60 "$WORK/$name.log" | redact >&2 || true
		fail "the Job $name did not complete"
	fi
}

# seed: the demo's own data, as the database's operator (an anonymous administrator in the Job's own process, spec
# 001's first deployment) once the nodes wrote the first policy: the source sales and its secret, the roles and the
# catalog demo, the groups' bindings, the profile's first source. The catalog is reached by the secret hugr-init made
# (granted to the seeder).
seed() {
	step "the demo's data: the source sales, the analysts, the catalog demo, the first profile - one Job"
	local sales g grants=""
	sales="$(secret_value pg-sales)"
	# every node's identity is the one tresor principal: the source's secret goes to it
	cat >"$WORK/seed.sql" <<SQL
-- postgres_scanner first, from the repository: a postgres secret's type is its
SET GLOBAL allow_extension_repositories = 'allowed';
CREATE OR REPLACE EXTENSION REPOSITORY hugr WITH PREFIX '$KISTA_PREFIX';
INSTALL postgres_scanner FROM hugr;
LOAD postgres_scanner FROM hugr;
CREATE OR REPLACE PERSISTENT SECRET sales IN owner (TYPE postgres, HOST 'pg.db.svc', PORT '5432', DATABASE 'sales',
    USER 'sales_reader', PASSWORD '$sales');
CALL owner.grant_secret('sales', '$NODES_PRINCIPAL', ['use']);
CALL owner.grant_secret('sales', 'role:demo_seeder', ['use']);
-- what the seeder may use is listed at ATTACH: again, after its own grant
DETACH owner;
ATTACH 'tresor:$TRESOR_HOST' AS owner (SECRET seeder);
-- the catalog by the secret hugr-init made: its password is tresor's only
ATTACH '' AS pg (TYPE postgres, SECRET hugr_policy_catalog);
SELECT acl_use_db('pg', 'acl');
-- the Job's own process, unprefixed (spec 001's first deployment): no door, nothing of it reaches a node
SET GLOBAL acl_allow_anonymous_admin = true;
-- the first policy's issuer (the first node wrote it); the demo's client for the checks' principals, whose tokens
-- name no console (the console's client is the first policy's)
ACL ADMIN CREATE ISSUER IF NOT EXISTS idp URL '$IDP_ISSUER';
ACL ADMIN CREATE CLIENT IF NOT EXISTS demo ISSUER idp AUDIENCES ('$HUGR_AUDIENCE') ROLES FROM ('$HUGR_ROLES_CLAIM') UNMAPPED AS ROLE;
ACL ADMIN CREATE ROLE IF NOT EXISTS analyst_a;
ACL ADMIN CREATE ROLE IF NOT EXISTS analyst_b;
ACL ADMIN CREATE ROLE IF NOT EXISTS node_breakglass;
-- platform_admin is the first policy's (cluster, policy, passthrough); what the console reads of a node's engine - its
-- memory, its extensions, its settings - are duckdb's own functions, in the categories an operator grants
-- (hugr-platform spec 003 Q7)
ACL ADMIN GRANT FUNCTION CATEGORY meta TO ROLE platform_admin;
ACL ADMIN GRANT FUNCTION CATEGORY environment TO ROLE platform_admin;
-- the checks read a node's own state (its settings, the profile's version, its attached databases) through ACL
-- NATIVE, the break-glass: a principal of its own, writing nothing
ACL ADMIN GRANT ADMIN passthrough TO ROLE node_breakglass;
-- a role with an administration scope is reached only through the client's own mapping (duckdb-acl spec 095)
ACL ADMIN MAP CLAIM 'platform_admin' FROM CLIENT demo TO ROLE platform_admin;
ACL ADMIN MAP CLAIM 'node_breakglass' FROM CLIENT demo TO ROLE node_breakglass;
ACL ADMIN CLUSTER ATTACH '' AS sales (TYPE postgres, SECRET sales);
ACL ADMIN CREATE VIRTUAL CATALOG IF NOT EXISTS demo;
ACL ADMIN ADD TABLE sales.public.orders AS demo.orders;
ACL ADMIN GRANT CATALOG demo TO ROLE analyst_a WITH (select) MAIN;
ACL ADMIN GRANT CATALOG demo TO ROLE analyst_b WITH (select) MAIN;
ACL ADMIN GRANT CATALOG demo TO ROLE platform_admin WITH (select) MAIN;
SQL
	for g in "${GROUPS_[@]}"; do
		# the groups are the first policy's (bound to platform_admin): the analysts and the break-glass are the demo's
		cat >>"$WORK/seed.sql" <<SQL
ACL ADMIN GRANT RESOURCE GROUP $g TO ROLE analyst_$g;
ACL ADMIN GRANT RESOURCE GROUP $g TO ROLE node_breakglass;
SQL
	done
	echo "SELECT 'version:' || acl_cluster_version();" >>"$WORK/seed.sql"
	echo "SET GLOBAL acl_allow_anonymous_admin = false;" >>"$WORK/seed.sql"
	tresor_job demo-seed "$WORK/seed.sql"
	VERSION="$(sed -n 's/^"\{0,1\}version:\([0-9]*\).*/\1/p' "$WORK/demo-seed.log" | tail -1)"
	[ -n "$VERSION" ] || {
		tail -40 "$WORK/demo-seed.log" | redact >&2
		fail "the demo's data wrote no profile version"
	}
	# the profile's source is a hot item: every node attaches it on its tick, and reports the version applied
	local pod
	for g in "${GROUPS_[@]}"; do
		for pod in $(pods "$g"); do
			eventually 120 "node $pod applies the profile's version $VERSION" lease_is "$pod" profile-applied "$VERSION"
		done
	done
	ok "the demo's data: the source sales, the analysts, the catalog demo; every node at profile v$VERSION"
}
