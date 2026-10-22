# shellcheck shell=bash
# tresor-server and kista, each by its own chart, as an organisation installs them (spec 005 step 3: the umbrella takes
# them by their address). Their state on the demo's PostgreSQL, their keys in the demo's OpenBao (stack.sh), their
# certificates from the run's CA; the umbrella's checks then reach them like any given service. The charts' versions
# are the umbrella's pins (charts/hugr/Chart.yaml), so a run tests the release's products.

chart_pin() { # <dependency>: its version in the umbrella's Chart.yaml
	python3 - "$ROOT/charts/hugr/Chart.yaml" "$1" <<'PY'
import re, sys
text = open(sys.argv[1]).read()
for block in text.split("\n  - name: ")[1:]:
    if block.split("\n")[0].strip() == sys.argv[2]:
        print(re.search(r"version: (\S+)", block).group(1)); break
PY
}

# service_database <name> <login>: a service's database and login on the demo's PostgreSQL, its password in a Secret
# <name>-db of its namespace (made once for the run)
service_database() {
	local ns="$1" name="$2"
	# the password on stdin, never on a command line
	k -n db exec -i deploy/pg -- psql -qU postgres -v ON_ERROR_STOP=1 >/dev/null <<SQL || fail "$name's database"
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$name') THEN CREATE ROLE $name LOGIN; END IF;
END \$\$;
ALTER ROLE $name PASSWORD '$(secret_value "pg-$name")';
SELECT 'CREATE DATABASE $name OWNER $name' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '$name') \\gexec
SQL
	k -n "$ns" delete secret "$name-db" --ignore-not-found >/dev/null
	k -n "$ns" create secret generic "$name-db" --from-file=password="$(secret_file "pg-$name" openssl rand -hex 24)" \
		>/dev/null || fail "$name's database Secret"
}

# the private CAs a service trusts: the database's, the vault's, the IdP's - all the run's CA here
service_cas() {
	k -n "$1" delete secret hugr-cas --ignore-not-found >/dev/null
	local idp=()
	[ "$IDP_NAME" != Keycloak ] || idp=(--from-file=idp.crt="$WORK/ca.crt")
	k -n "$1" create secret generic hugr-cas --from-file=database.crt="$WORK/ca.crt" \
		--from-file=vault.crt="$WORK/ca.crt" "${idp[@]}" >/dev/null || fail "$1's CAs"
}

tresor_values() {
	local private=false
	case "$CLUSTER_ISSUER" in https://kubernetes.default.svc*) private=true ;; esac
	cat <<YAML
namespaceOverride: $TRESOR_NS
fullnameOverride: tresor
replicas: 2
tlsSecret: tresor-tls
service: {port: 443}
clusterIssuer: {enabled: $private}
vaultToken: {enabled: true, audience: vault}
resources: {requests: {cpu: 10m, memory: 32Mi}}
extraVolumes:
  - {name: db, secret: {secretName: tresor-db}}
  - {name: cas, secret: {secretName: hugr-cas}}
extraVolumeMounts:
  - {name: db, mountPath: /var/run/tresor/db, readOnly: true}
  - {name: cas, mountPath: /var/run/tresor/cas, readOnly: true}
config:
  public_url: "https://tresor.$TRESOR_NS.svc"
  state:
    kind: postgres
    dsn: host=pg.db.svc port=5432 dbname=tresor user=tresor sslmode=verify-full sslrootcert=/var/run/tresor/cas/database.crt
    auth: password
    password_file: /var/run/tresor/db/password
  keys: {kind: vault, mount: transit, key: hugr-tresor}
  vault:
    address: "$VAULT_ADDRESS"
    ca_file: /var/run/tresor/cas/vault.crt
    auth: {method: kubernetes, mount: kubernetes, role: tresor}
  issuers:
    - issuer: "$IDP_ISSUER"
      audience: "$TRESOR_AUDIENCE"
      roles_claim: "$TRESOR_ROLES_CLAIM"
      groups_claim: "$TRESOR_GROUPS_CLAIM"
      ca_file: "$([ "$IDP_NAME" != Keycloak ] || echo /var/run/tresor/cas/idp.crt)"
      service: {claim: "$TRESOR_SERVICE_CLAIM"}
      service_flows: [client_credentials, private_key_jwt, federated]
    - issuer: "$CLUSTER_ISSUER"
      audience: duckdb-secrets
      ca_file: "$([ "$private" = false ] || echo /var/run/tresor/cluster/ca.crt)"
      discovery_token_file: "$([ "$private" = false ] || echo /var/run/tresor/cluster/token)"
      service: {claim: kubernetes.io, client_claim: sub}
  policy:
    # the platform's hugr-init (it writes the catalog's secret), the seeder (the demo's data) and the console stand's
    # person (spec 003: tresor's console mounted)
    admins:
      - "subject:$CLUSTER_ISSUER|system:serviceaccount:hugr:hugr-init"
      - role:demo_seeder
      - role:secrets_admin
    # the nodes act for their sessions (ACT_FOR_SESSIONS): one rule by the role every group's identity carries, the
    # console's group c's too (tresor-server 0.7.5, spec 031); only the nodes' service accounts hold it
    actors:
      - {principal: "role:hugr_node", issuer: "$IDP_ISSUER", verbs: [use]}
YAML
}

kista_values() {
	# a DuckDB release is a vX.Y.Z tag; a node built on a commit runs a dev version
	local kind=dev
	case "$DUCKDB_VERSION" in v[0-9]*.[0-9]*.[0-9]*) kind=release ;; esac
	cat <<YAML
namespaceOverride: $KISTA_NS
mode: tls
tlsSecret: kista-tls
replicas: 1
service: {type: ClusterIP}
resources: {requests: {cpu: 20m, memory: 64Mi}, limits: {memory: 512Mi}}
blobVolume: {enabled: true, accessMode: ReadWriteOnce, size: 2Gi}
secretVolumes:
  - {name: db, secretName: kista-db, mountPath: /etc/kista/secrets/db}
  - {name: cas, secretName: hugr-cas, mountPath: /etc/kista/secrets/cas}
projectedTokens:
  - {name: vault, mountPath: /var/run/secrets/kista/vault, audience: vault, expirationSeconds: 3600}
config:
  serve: {public_url: "https://hugr-kista.$KISTA_NS.svc"}
  store:
    kind: postgres
    dsn: host=pg.db.svc port=5432 dbname=kista user=kista sslmode=verify-full sslrootcert=/etc/kista/secrets/cas/database.crt
    login: {kind: password, password_file: /etc/kista/secrets/db/password}
  blob:
    max_body: 256MiB
    domains:
      - {name: default, kind: fs, fs: {root: /var/lib/kista/blobs}}
  signers:
    sources:
      - name: keys
        kind: vault
        allow: [hugr-channel]
        vault:
          address: "$VAULT_ADDRESS"
          mount: transit
          ca_file: /etc/kista/secrets/cas/vault.crt
          software_keys: true
          auth: {kind: kubernetes, mount: kubernetes, role: kista, token_file: /var/run/secrets/kista/vault/token}
  bootstrap:
    versions:
      - {version: "$DUCKDB_VERSION", kind: $kind}
    tenant: {name: hugr}
    channels:
      - {name: platform, kind: signed, versions: ["$DUCKDB_VERSION"], key: "keys:hugr-channel:v1"}
    upstreams: []
YAML
}

# install_services: tresor-server and kista by their own charts, in their namespaces, ready before the umbrella
install_services() {
	step "tresor-server and kista by their own charts (an organisation's), on the demo's PostgreSQL and OpenBao"
	local ns tresor_version kista_version
	tresor_version="$(chart_pin tresor-server)"
	kista_version="$(chart_pin kista)"
	for ns in "$TRESOR_NS" "$KISTA_NS"; do
		namespace "$ns"
		service_cas "$ns"
	done
	ca_cert tresor "tresor.$TRESOR_NS.svc" "tresor.$TRESOR_NS.svc.cluster.local"
	ca_cert kista "hugr-kista.$KISTA_NS.svc" "hugr-kista.$KISTA_NS.svc.cluster.local"
	tls_secret "$TRESOR_NS" tresor-tls tresor >/dev/null
	tls_secret "$KISTA_NS" kista-tls kista >/dev/null
	service_database "$TRESOR_NS" tresor
	service_database "$KISTA_NS" kista
	CLUSTER_ISSUER="$(k get --raw /.well-known/openid-configuration |
		python3 -c 'import json, sys; print(json.load(sys.stdin)["issuer"])')" || fail "the cluster's ServiceAccount issuer"
	tresor_values >"$WORK/tresor-values.yaml"
	kista_values >"$WORK/kista-values.yaml"
	helm_ upgrade --install tresor oci://ghcr.io/hugr-lab/charts/tresor-server --version "$tresor_version" \
		-n "$TRESOR_NS" -f "$WORK/tresor-values.yaml" --wait --timeout 600s >"$WORK/tresor-helm.log" 2>&1 || {
		cat "$WORK/tresor-helm.log" >&2
		k -n "$TRESOR_NS" logs -l app.kubernetes.io/instance=tresor --tail 40 --prefix 2>&1 | redact >&2 || true
		fail "tresor-server did not install"
	}
	helm_ upgrade --install hugr-kista oci://ghcr.io/hugr-lab/charts/kista --version "$kista_version" \
		-n "$KISTA_NS" -f "$WORK/kista-values.yaml" --wait --timeout 600s >"$WORK/kista-helm.log" 2>&1 || {
		cat "$WORK/kista-helm.log" >&2
		k -n "$KISTA_NS" logs -l app.kubernetes.io/instance=hugr-kista --tail 40 --prefix 2>&1 | redact >&2 || true
		fail "kista did not install"
	}
	ok "tresor-server $tresor_version at https://$TRESOR_HOST and kista $kista_version at $KISTA_PREFIX, each by its own chart, their keys in the demo's OpenBao"
}
