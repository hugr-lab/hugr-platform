# shellcheck shell=bash
# The demo's services (spec 004): PostgreSQL, the IdP, tresor-server, kista. Each made again by a run; its secrets
# are the run's (secret_value), held in Kubernetes Secrets of the demo's cluster, never printed.

POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:17}"
TRESOR_HOST="tresor.tresor.svc:443"
KISTA_PREFIX="https://kista.kista.svc/hugr/demo"
GROUPS_=(a b)
NODE_CPU="${HUGR_DEMO_NODE_CPU:-1}"

install_db() {
	step "PostgreSQL: the policy catalog (hugr) and a source (sales)"
	namespace db
	k -n db delete secret pg --ignore-not-found >/dev/null
	k -n db create secret generic pg --from-file=password="$(secret_file pg openssl rand -hex 24)" >/dev/null
	k apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: pg, namespace: db}
spec:
  selector: {matchLabels: {app: pg}}
  template:
    metadata: {labels: {app: pg}}
    spec:
      containers:
        - name: postgres
          image: $POSTGRES_IMAGE
          env:
            - name: POSTGRES_PASSWORD
              valueFrom: {secretKeyRef: {name: pg, key: password}}
          ports: [{containerPort: 5432}]
          # over TCP: the image's init runs a temporary server on the socket only, then restarts
          readinessProbe: {exec: {command: [pg_isready, -h, 127.0.0.1, -U, postgres]}, periodSeconds: 2}
          resources: {requests: {cpu: 50m, memory: 128Mi}}
---
apiVersion: v1
kind: Service
metadata: {name: pg, namespace: db}
spec:
  selector: {app: pg}
  ports: [{port: 5432}]
YAML
	rollout db pg
	pg_answers() { k -n db exec deploy/pg -- psql -h 127.0.0.1 -qU postgres -c "SELECT 1"; }
	eventually 60 "PostgreSQL answers" pg_answers
	# the policy catalog is the demo's own: made again by every run's first deployment
	k -n db exec deploy/pg -- psql -qU postgres -c "DROP DATABASE IF EXISTS hugr WITH (FORCE)" >/dev/null ||
		fail "dropping the catalog's database"
	k -n db exec deploy/pg -- psql -qU postgres -c "CREATE DATABASE hugr" >/dev/null || fail "the catalog's database"
	k -n db exec deploy/pg -- sh -c 'psql -qU postgres -tc "SELECT 1 FROM pg_database WHERE datname = '"'"'sales'"'"'" |
		grep -q 1 || psql -qU postgres -c "CREATE DATABASE sales"' >/dev/null || fail "the source's database"
	k -n db exec deploy/pg -- psql -qU postgres -v ON_ERROR_STOP=1 -d sales -c "CREATE TABLE IF NOT EXISTS orders (id int
		PRIMARY KEY, region text, amount numeric); INSERT INTO orders VALUES (1, 'north', 10), (2, 'south', 20),
		(3, 'north', 30) ON CONFLICT DO NOTHING" >/dev/null || fail "the source's table"
	# two logins, neither the superuser: the catalog's owner, and a reader of the source's one table. The passwords go
	# on stdin, never on a command line.
	k -n db exec -i deploy/pg -- psql -qU postgres -v ON_ERROR_STOP=1 >/dev/null <<SQL || fail "PostgreSQL's logins"
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'hugr_catalog') THEN CREATE ROLE hugr_catalog LOGIN; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sales_reader') THEN CREATE ROLE sales_reader LOGIN; END IF;
END \$\$;
ALTER ROLE hugr_catalog PASSWORD '$(secret_value pg-catalog)';
ALTER ROLE sales_reader PASSWORD '$(secret_value pg-sales)';
ALTER DATABASE hugr OWNER TO hugr_catalog;
REVOKE ALL ON DATABASE sales FROM PUBLIC;
GRANT CONNECT ON DATABASE sales TO sales_reader;
\\c sales
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO sales_reader;
GRANT SELECT ON orders TO sales_reader;
SQL
	ok "PostgreSQL in db: the catalog's database (its owner hugr_catalog) and the source sales.orders (sales_reader)"
}

# --- tresor-server: the nodes' secrets, the seeder its admin ---------------------------------------------------------
install_tresor() {
	step "tresor-server: the catalog's login and the source's, for the groups' identities"
	namespace tresor
	ca_cert tresor tresor.tresor.svc
	local cert
	cert="$(tls_secret tresor tresor-tls tresor)"
	k -n tresor delete secret kek --ignore-not-found >/dev/null
	# the KEK seals tresor's state in the cluster: the run's own, kept between runs, or the state no longer opens
	k -n tresor create secret generic kek --from-file=key="$(secret_file tresor-kek openssl rand -base64 32)" >/dev/null
	k -n tresor delete configmap ca --ignore-not-found >/dev/null
	k -n tresor create configmap ca --from-file=ca.crt="$WORK/ca.crt" >/dev/null
	local actors="" g groups_claim=""
	for g in "${GROUPS_[@]}"; do
		actors="$actors      - {principal: 'client:$(group_client_id "$g")', verbs: [use]}
"
	done
	[ -z "$TRESOR_GROUPS_CLAIM" ] || groups_claim="groups_claim: $TRESOR_GROUPS_CLAIM"
	cat >"$WORK/tresor.yaml" <<YAML
fullnameOverride: tresor
image: {repository: "$(img_repo tresor-server)", tag: demo, pullPolicy: $PULL}
localKEK: {secretName: kek}
tlsSecret: tresor-tls
# the run's CA beside the system's roots (an IdP outside the cluster): SSL_CERT_DIR adds, SSL_CERT_FILE would replace
env: [{name: SSL_CERT_DIR, value: /etc/tresor-ca}]
extraVolumes: [{name: ca, configMap: {name: ca}}]
extraVolumeMounts: [{name: ca, mountPath: /etc/tresor-ca, readOnly: true}]
resources: {requests: {cpu: 10m, memory: 32Mi}}
service: {port: 443}
config:
  public_url: https://$TRESOR_HOST
  issuers:
    - issuer: $IDP_ISSUER
      audience: $TRESOR_AUDIENCE
      roles_claim: $TRESOR_ROLES_CLAIM
      $groups_claim
      service: {claim: $TRESOR_SERVICE_CLAIM}
      service_flows: [client_credentials, private_key_jwt, federated]
  policy:
    admins: [role:demo_seeder]
    # the nodes act for their sessions (ACT_FOR_SESSIONS); an actor is a service, client:<id>
    actors:
$actors
YAML
	if ! helm_ upgrade --install tresor "$TRESOR_SRC/deploy/helm/tresor-server" -n tresor -f "$WORK/tresor.yaml" \
		--wait --timeout 300s >"$WORK/tresor-helm.log" 2>&1; then
		cat "$WORK/tresor-helm.log" >&2
		k -n tresor get pods -o wide >&2 || true
		k -n tresor logs -l app.kubernetes.io/instance=tresor --tail 60 2>&1 | redact >&2 || true
		fail "tresor-server did not install"
	fi
	if [ -n "$cert" ]; then # a new certificate: served once the pod starts again
		k -n tresor rollout restart deploy/tresor >/dev/null && rollout tresor tresor
	fi
	ok "tresor-server in tresor at https://$TRESOR_HOST"
}

# --- kista: a channel for the node's duckdb, the release's extensions in it ------------------------------------------
# Its channel key is in OpenBao Transit, reached by kista's own ServiceAccount token (as kista's kind test): kista
# refuses a key file from a Secret, on purpose.
OPENBAO_IMAGE="${OPENBAO_IMAGE:-openbao/openbao:2.7.1}"

install_openbao() {
	ca_cert openbao openbao.kista.svc
	local cert
	cert="$(tls_secret kista openbao-tls openbao)"
	k -n kista delete secret openbao-root --ignore-not-found >/dev/null
	# dev mode prints its root token in its own log: the throwaway cluster's, and blanked wherever the run shows a log
	k -n kista create secret generic openbao-root --from-file=token="$(secret_file openbao-root openssl rand -hex 24)" >/dev/null
	k -n kista delete configmap openbao-config --ignore-not-found >/dev/null
	k -n kista create configmap openbao-config --from-literal=bao.hcl='listener "tcp" {
  address       = "0.0.0.0:8200"
  tls_cert_file = "/tls/tls.crt"
  tls_key_file  = "/tls/tls.key"
}' >/dev/null
	k apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: openbao, namespace: kista}
---
# OpenBao reviews kista's ServiceAccount token
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: hugr-demo-openbao-tokenreview}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: system:auth-delegator}
subjects: [{kind: ServiceAccount, name: openbao, namespace: kista}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: openbao, namespace: kista}
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
metadata: {name: openbao, namespace: kista}
spec: {selector: {app: openbao}, ports: [{port: 8200}]}
YAML
	[ -z "$cert" ] || k -n kista rollout restart deploy/openbao >/dev/null
	rollout kista openbao
	# the root token stays in the pod's environment: the command reads it there, never on a command line here
	bao() { k -n kista exec -i deploy/openbao -- sh -c 'BAO_ADDR=http://127.0.0.1:8201 BAO_TOKEN="$BAO_DEV_ROOT_TOKEN_ID" bao "$@"' bao "$@"; }
	eventually 60 "OpenBao answers" bao status
	bao secrets list | grep -q '^transit/' || bao secrets enable transit >/dev/null
	# dev mode keeps everything in memory: a restarted OpenBao has a new key, and kista - whose state is on its volume -
	# refuses to sign with a key that changed; its state starts anew then
	KISTA_FRESH=""
	if ! bao read transit/keys/ext-demo >/dev/null 2>&1; then
		bao write -f transit/keys/ext-demo type=rsa-2048 >/dev/null || fail "OpenBao's transit key"
		KISTA_FRESH=1
	fi
	bao policy write kista-ext-demo - >/dev/null <<'HCL'
path "transit/keys/ext-demo" {
  capabilities = ["read"]
}
path "transit/sign/ext-demo/sha2-256" {
  capabilities = ["update"]
}
HCL
	bao auth list | grep -q '^kubernetes/' || bao auth enable kubernetes >/dev/null
	bao write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc >/dev/null
	bao write auth/kubernetes/role/kista bound_service_account_names=kista bound_service_account_namespaces=kista \
		audience=openbao token_policies=kista-ext-demo token_ttl=1h >/dev/null
}

install_kista() {
	step "kista: the channel hugr/demo, the node's extensions in it, its key in OpenBao Transit"
	namespace kista
	install_openbao
	if [ -n "$KISTA_FRESH" ] && helm_ status kista -n kista >/dev/null 2>&1; then
		helm_ uninstall kista -n kista --wait >/dev/null || fail "kista's uninstall"
		k -n kista delete pvc -l app.kubernetes.io/instance=kista --wait >/dev/null || fail "kista's volume"
	fi
	ca_cert kista kista.kista.svc
	local cert
	cert="$(tls_secret kista kista-tls kista)"
	k -n kista delete secret kista-ca --ignore-not-found >/dev/null
	k -n kista create secret generic kista-ca --from-file=ca.crt="$WORK/ca.crt" >/dev/null
	cat >"$WORK/kista.yaml" <<YAML
image: {repository: "$(img_repo kista)", tag: demo, pullPolicy: $PULL}
mode: tls
tlsSecret: kista-tls
devProfile: true
replicas: 1
service: {type: ClusterIP}
persistence: {size: 2Gi}
secretVolumes: [{name: ca, secretName: kista-ca, mountPath: /etc/kista/secrets/ca}]
projectedTokens: [{name: bao, mountPath: /var/run/secrets/kista/bao, audience: openbao, expirationSeconds: 3600}]
resources: {requests: {cpu: 20m, memory: 64Mi}, limits: {memory: 512Mi}}
config:
  profile: dev
  store: {kind: sqlite}
  signers:
    sources:
      - name: bao
        kind: vault
        allow: ["ext-"]
        vault:
          address: https://openbao.kista.svc:8200
          mount: transit
          ca_file: /etc/kista/secrets/ca/ca.crt
          software_keys: true
          auth: {kind: kubernetes, role: kista, token_file: /var/run/secrets/kista/bao/token}
  serve: {public_url: https://kista.kista.svc}
  blob: {max_body: 256MiB}
YAML
	if ! helm_ upgrade --install kista "$KISTA_SRC/deploy/helm/kista" -n kista -f "$WORK/kista.yaml" \
		--wait --timeout 300s >"$WORK/kista-helm.log" 2>&1; then
		cat "$WORK/kista-helm.log" >&2
		k -n kista get pods -o wide >&2 || true
		k -n kista logs -l app.kubernetes.io/name=kista --tail 60 2>&1 | redact >&2 || true
		fail "kista did not install"
	fi
	if [ -n "$cert" ]; then
		k -n kista rollout restart deploy/kista >/dev/null && rollout kista kista
	fi
	# kista's own failure stands (pipefail) and is shown; its profile-dev warning is not
	ka() {
		k -n kista exec -i deploy/kista -c kista -- /kista admin -config /etc/kista/kista.yaml "$@" 2>&1 |
			{ grep -v 'WARNING: profile dev' || true; }
	}
	# kd <what> <args...>: a kista admin change, its refusal shown
	kd() {
		local what="$1" out
		shift
		out="$(ka "$@")" || fail "kista: $what: $(redact <<<"$out")"
	}
	ka tenant list | grep -qw hugr || kd "the tenant" tenant create hugr
	ka channel list hugr | grep -qw demo || kd "the channel" channel create hugr/demo -kind signed
	ka key list hugr/demo | grep -q 'ext-demo' || kd "the channel's key" key add hugr/demo -signer bao:ext-demo:v1 -active
	local file meta name version out
	for file in "$DIST"/*.duckdb_extension; do
		meta="$(python3 "$NODE_SRC/scripts/ext_footer.py" "$file")"
		name="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["name"])' "$meta")"
		version="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["duckdb_version"])' "$meta")"
		[ "$name" = hugr_node ] && continue # linked into the node
		ka version list | grep -qw -- "$version" || kd "the version $version" version add "$version" -kind dev
		ka channel versions hugr/demo | grep -qx -- "$version" ||
			kd "the channel's version $version" channel versions hugr/demo -add "$version"
		# every asset, whatever kista has: a release it already holds is answered as such, a new one added
		out="$(ka release add hugr/demo - -name "$name" <"$file")" ||
			grep -q 'already exists' <<<"$out" || fail "kista: the release $name: $(redact <<<"$out")"
	done
	ok "kista in kista: $KISTA_PREFIX, its key in OpenBao Transit, the release's extensions in it"
}
