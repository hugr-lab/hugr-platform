# shellcheck shell=bash
# The demo's services outside the platform (spec 004, spec 005): the PostgreSQL server the umbrella chart takes as the
# organisation's (global.database.host), the source sales, and a vault standing in for the organisation's, which
# tresor-server and kista - installed by their own charts (services.sh) - keep their keys in. Each made again by a run;
# its secrets are the run's (secret_value), held in Kubernetes Secrets of the demo's cluster, never printed.

POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:18}"
# the platform's services, by their own charts (services.sh): tresor in hugr-tresor, kista (its release hugr-kista) in
# hugr-kista
TRESOR_NS=hugr-tresor
KISTA_NS=hugr-kista
TRESOR_HOST="tresor.$TRESOR_NS.svc:443"
KISTA_PREFIX="https://hugr-kista.$KISTA_NS.svc/hugr/platform"
GROUPS_=(a b)
# spec 006: a group the console makes in check 13 - its identity given beforehand, as an installation gives one (the IdP's
# client, tresor's actor), the group itself not the chart's
CONSOLE_GROUP=c
IDENTITIES_=("${GROUPS_[@]}" "$CONSOLE_GROUP")
NODE_CPU="${HUGR_DEMO_NODE_CPU:-1}"

install_db() {
	step "PostgreSQL 18 over TLS: the server of the catalog, tresor's state and kista's store; a source (sales)"
	namespace db
	ca_cert pg pg.db.svc
	local cert
	cert="$(tls_secret db pg-tls pg)"
	k -n db delete secret pg --ignore-not-found >/dev/null
	k -n db create secret generic pg --from-file=password="$(secret_file pg openssl rand -hex 24)" >/dev/null
	# the key is root's and the postgres group's (fsGroup 999, the image's postgres): PostgreSQL takes a key owned by
	# root at 0640
	k apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: pg, namespace: db}
spec:
  selector: {matchLabels: {app: pg}}
  template:
    metadata: {labels: {app: pg}}
    spec:
      securityContext: {fsGroup: 999}
      containers:
        - name: postgres
          image: $POSTGRES_IMAGE
          args: [-c, ssl=on, -c, ssl_cert_file=/tls/tls.crt, -c, ssl_key_file=/tls/tls.key]
          env:
            - name: POSTGRES_PASSWORD
              valueFrom: {secretKeyRef: {name: pg, key: password}}
          ports: [{containerPort: 5432}]
          # over TCP: the image's init runs a temporary server on the socket only, then restarts
          readinessProbe: {exec: {command: [pg_isready, -h, 127.0.0.1, -U, postgres]}, periodSeconds: 2}
          resources: {requests: {cpu: 50m, memory: 128Mi}}
          volumeMounts: [{name: tls, mountPath: /tls, readOnly: true}]
      volumes:
        - {name: tls, secret: {secretName: pg-tls, defaultMode: 0640}}
---
apiVersion: v1
kind: Service
metadata: {name: pg, namespace: db}
spec:
  selector: {app: pg}
  ports: [{port: 5432}]
YAML
	[ -z "$cert" ] || k -n db rollout restart deploy/pg >/dev/null
	rollout db pg
	pg_answers() { k -n db exec deploy/pg -- psql -h 127.0.0.1 -qU postgres -c "SELECT 1"; }
	eventually 60 "PostgreSQL answers" pg_answers
	k -n db exec deploy/pg -- sh -c 'psql -qU postgres -tc "SELECT 1 FROM pg_database WHERE datname = '"'"'sales'"'"'" |
		grep -q 1 || psql -qU postgres -c "CREATE DATABASE sales"' >/dev/null || fail "the source's database"
	k -n db exec deploy/pg -- psql -qU postgres -v ON_ERROR_STOP=1 -d sales -c "CREATE TABLE IF NOT EXISTS orders (id int
		PRIMARY KEY, region text, amount numeric); INSERT INTO orders VALUES (1, 'north', 10), (2, 'south', 20),
		(3, 'north', 30) ON CONFLICT DO NOTHING" >/dev/null || fail "the source's table"
	# the source's login, not the superuser: a reader of its one table. The password goes on stdin, never on a command
	# line.
	k -n db exec -i deploy/pg -- psql -qU postgres -v ON_ERROR_STOP=1 >/dev/null <<SQL || fail "PostgreSQL's logins"
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sales_reader') THEN CREATE ROLE sales_reader LOGIN; END IF;
END \$\$;
ALTER ROLE sales_reader PASSWORD '$(secret_value pg-sales)';
REVOKE ALL ON DATABASE sales FROM PUBLIC;
GRANT CONNECT ON DATABASE sales TO sales_reader;
\\c sales
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO sales_reader;
GRANT SELECT ON orders TO sales_reader;
SQL
	ok "PostgreSQL in db, TLS by the run's CA: the source sales.orders (sales_reader); tresor's and kista's databases are their installs', the catalog's hugr-init's"
}

# drop_databases: the catalog's, tresor's and kista's databases and logins (and an older run's OpenBao's), for an install
# afresh (hugr-init and services.sh make them again)
drop_databases() {
	local name
	for name in hugr:hugr_catalog tresor:tresor kista:kista openbao:openbao; do
		# PGOPTIONS: no NOTICE for a name already gone
		k -n db exec deploy/pg -- env PGOPTIONS=--client-min-messages=warning psql -qU postgres \
			-c "DROP DATABASE IF EXISTS ${name%%:*} WITH (FORCE)" -c "DROP ROLE IF EXISTS ${name##*:}" >/dev/null ||
			fail "dropping the database and the login of ${name%%:*}"
	done
}

# --- the organisation's vault: OpenBao, the services' Transit keys and their Kubernetes-auth roles -------------------
# What tresor-server and kista, installed by their own charts, take from the organisation's vault: kista signs its
# channel with hugr-channel, tresor wraps its data keys with hugr-tresor, each logging in by its own ServiceAccount's
# token.
OPENBAO_IMAGE="${OPENBAO_IMAGE:-openbao/openbao:2.7.1}"
VAULT_ADDRESS="https://openbao.openbao.svc:8200"

install_vault() {
	step "the vault (OpenBao): the Transit keys hugr-channel and hugr-tresor, a Kubernetes-auth role for each service"
	namespace openbao
	ca_cert openbao openbao.openbao.svc
	local cert
	cert="$(tls_secret openbao openbao-tls openbao)"
	k -n openbao delete secret openbao-root --ignore-not-found >/dev/null
	# dev mode prints its root token in its own log: the throwaway cluster's, and blanked wherever the run shows a log
	k -n openbao create secret generic openbao-root --from-file=token="$(secret_file openbao-root openssl rand -hex 24)" >/dev/null
	k -n openbao delete configmap openbao-config --ignore-not-found >/dev/null
	k -n openbao create configmap openbao-config --from-literal=bao.hcl='listener "tcp" {
  address       = "0.0.0.0:8200"
  tls_cert_file = "/tls/tls.crt"
  tls_key_file  = "/tls/tls.key"
}' >/dev/null
	k apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: openbao, namespace: openbao}
---
# OpenBao reviews the services' ServiceAccount tokens
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: hugr-demo-openbao-tokenreview}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: system:auth-delegator}
subjects: [{kind: ServiceAccount, name: openbao, namespace: openbao}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: openbao, namespace: openbao}
spec:
  selector: {matchLabels: {app: openbao}}
  template:
    metadata: {labels: {app: openbao}}
    spec:
      serviceAccountName: openbao
      containers:
        - name: openbao
          image: $OPENBAO_IMAGE
          args: [server, -dev, -dev-listen-address=127.0.0.1:8201, -config=/config/bao.hcl]
          env:
            - {name: SKIP_SETCAP, value: "true"}
            - name: BAO_DEV_ROOT_TOKEN_ID
              valueFrom: {secretKeyRef: {name: openbao-root, key: token}}
          ports: [{containerPort: 8200}]
          volumeMounts: [{name: tls, mountPath: /tls}, {name: config, mountPath: /config}]
      volumes:
        - {name: tls, secret: {secretName: openbao-tls}}
        - {name: config, configMap: {name: openbao-config}}
---
apiVersion: v1
kind: Service
metadata: {name: openbao, namespace: openbao}
spec: {selector: {app: openbao}, ports: [{port: 8200}]}
YAML
	[ -z "$cert" ] || k -n openbao rollout restart deploy/openbao >/dev/null
	rollout openbao openbao
	# the root token stays in the pod's environment: the command reads it there, never on a command line here
	bao() { k -n openbao exec -i deploy/openbao -- sh -c 'BAO_ADDR=http://127.0.0.1:8201 BAO_TOKEN="$BAO_DEV_ROOT_TOKEN_ID" bao "$@"' bao "$@"; }
	eventually 60 "OpenBao answers" bao status
	bao secrets list | grep -q '^transit/' || bao secrets enable transit >/dev/null
	# dev mode keeps everything in memory: a restarted OpenBao has new keys - each run installs the services afresh
	# (platform.sh), so no state of theirs was sealed by the old ones. A DuckDB extension's signature is RSA
	bao read transit/keys/hugr-channel >/dev/null 2>&1 ||
		bao write -f transit/keys/hugr-channel type=rsa-2048 >/dev/null || fail "OpenBao's key hugr-channel"
	bao read transit/keys/hugr-tresor >/dev/null 2>&1 ||
		bao write -f transit/keys/hugr-tresor >/dev/null || fail "OpenBao's key hugr-tresor"
	bao policy write kista - >/dev/null <<'HCL'
path "transit/keys/hugr-channel" {
  capabilities = ["read"]
}
path "transit/sign/hugr-channel/sha2-256" {
  capabilities = ["update"]
}
HCL
	# tresor-server's documented policy for its KEK: the root an HMAC only it computes
	bao policy write tresor - >/dev/null <<'HCL'
path "transit/keys/hugr-tresor" {
  capabilities = ["read"]
}
path "transit/encrypt/hugr-tresor" {
  capabilities = ["update"]
}
path "transit/decrypt/hugr-tresor" {
  capabilities = ["update"]
}
path "transit/hmac/hugr-tresor/sha2-256" {
  capabilities = ["update"]
}
HCL
	bao auth list | grep -q '^kubernetes/' || bao auth enable kubernetes >/dev/null
	bao write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc >/dev/null
	# the services' ServiceAccounts: kista's is its release's hugr-kista, tresor's its fullname tresor (services.sh)
	bao write auth/kubernetes/role/kista bound_service_account_names=hugr-kista \
		bound_service_account_namespaces="$KISTA_NS" audience=vault token_policies=kista token_ttl=1h >/dev/null
	bao write auth/kubernetes/role/tresor bound_service_account_names=tresor \
		bound_service_account_namespaces="$TRESOR_NS" audience=vault token_policies=tresor token_ttl=1h >/dev/null
	ok "OpenBao in openbao at $VAULT_ADDRESS: the keys hugr-channel (kista's channel) and hugr-tresor (tresor's KEK)"
}

# --- kista: the node's extensions in the channel its bootstrap made -------------------------------------------------
# A node built on a commit of DuckDB's runs a dev version, which no upstream mirrors: its release's extensions are
# published by hand, as the platform's maintainers do. Until they are, a node's catalog step fails and the node
# restarts (its bootstrap stops at the first failing step).
publish_extensions() {
	step "kista: the node's extensions into hugr/platform (kista's bootstrap made the channel, its version and key)"
	# kista's own failure stands (pipefail) and is shown
	ka() { k -n "$KISTA_NS" exec -i deploy/hugr-kista -c kista -- /kista admin -config /etc/kista/kista.yaml "$@" 2>&1; }
	channel_has_version() { ka channel versions hugr/platform | grep -qx -- "$DUCKDB_VERSION"; }
	eventually 120 "kista's bootstrap made hugr/platform for $DUCKDB_VERSION" channel_has_version
	local file name out published=""
	for file in "$DIST"/*.duckdb_extension; do
		name="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["name"])' \
			"$(python3 "$NODE_SRC/scripts/ext_footer.py" "$file")")"
		[ "$name" = hugr_node ] && continue # linked into the node
		# every asset, whatever kista has: a release it already holds is answered as such, a new one added
		out="$(ka release add hugr/platform - -name "$name" <"$file")" ||
			grep -q 'already exists' <<<"$out" || fail "kista: the release $name: $(redact <<<"$out")"
		published="$published $name"
	done
	ok "kista at $KISTA_PREFIX: the release's extensions ($published ) for $DUCKDB_VERSION, signed by hugr-channel"
}
