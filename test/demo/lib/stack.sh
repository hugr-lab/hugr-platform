# shellcheck shell=bash
# The demo's services (spec 004): PostgreSQL, the IdP, tresor-server, kista. Each made again by a run; its secrets
# are the run's (secret_value), held in Kubernetes Secrets of the demo's cluster, never printed.

KEYCLOAK_IMAGE="${KEYCLOAK_IMAGE:-quay.io/keycloak/keycloak:26.6.4}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:17}"
KC_ISSUER="https://keycloak.idp.svc:8443/realms/hugr"
TRESOR_HOST="tresor.tresor.svc:443"
KISTA_PREFIX="https://kista.kista.svc/hugr/demo"
GROUPS_=(a b)

install_db() {
	step "PostgreSQL: the policy catalog (hugr) and a source (sales)"
	namespace db
	k -n db delete secret pg --ignore-not-found >/dev/null
	k -n db create secret generic pg --from-literal=password="$(secret_value pg)" >/dev/null
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
          readinessProbe: {exec: {command: [pg_isready, -U, postgres]}, periodSeconds: 2}
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
	k -n db exec deploy/pg -- psql -qU postgres -c "SELECT 1" >/dev/null
	# the policy catalog is the demo's own: made again by every run's first deployment
	k -n db exec deploy/pg -- psql -qU postgres -c "DROP DATABASE IF EXISTS hugr WITH (FORCE)" >/dev/null 2>&1
	k -n db exec deploy/pg -- psql -qU postgres -c "CREATE DATABASE hugr" >/dev/null
	k -n db exec deploy/pg -- sh -c 'psql -qU postgres -tc "SELECT 1 FROM pg_database WHERE datname = '"'"'sales'"'"'" |
		grep -q 1 || psql -qU postgres -c "CREATE DATABASE sales"' >/dev/null
	k -n db exec deploy/pg -- psql -qU postgres -d sales -c "CREATE TABLE IF NOT EXISTS orders (id int PRIMARY KEY,
		region text, amount numeric); INSERT INTO orders VALUES (1, 'north', 10), (2, 'south', 20), (3, 'north', 30)
		ON CONFLICT DO NOTHING" >/dev/null 2>&1
	ok "PostgreSQL in db: the catalog's database and the source sales.orders"
}

# --- the IdP on kind and minikube: Keycloak, the nodes' clients authenticated by their ServiceAccount tokens ----------
install_idp() {
	step "Keycloak: the realm hugr - the nodes by their ServiceAccount tokens, no secret of theirs"
	namespace idp
	ca_cert keycloak keycloak.idp.svc
	tls_secret idp keycloak-tls keycloak
	local sa_issuer
	sa_issuer="$(k get --raw /.well-known/openid-configuration | python3 -c 'import json,sys; print(json.load(sys.stdin)["issuer"])')"
	DEMO_SEEDER="$(secret_value kc-seeder)" DEMO_ANALYST_A="$(secret_value kc-analyst-a)" \
		DEMO_ANALYST_B="$(secret_value kc-analyst-b)" DEMO_ADMIN="$(secret_value kc-admin)" \
		python3 - "$WORK/realm.json" "$sa_issuer" "${GROUPS_[@]}" <<'PY'
import json, os, sys
out, sa_issuer, groups = sys.argv[1], sys.argv[2], sys.argv[3:]
aud = lambda audience: {"name": "aud-" + audience, "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
                        "config": {"included.custom.audience": audience, "access.token.claim": "true"}}
# the client's id as a claim, whatever scopes an imported service account gets (tresor's service claim)
client_id = {"name": "client_id", "protocol": "openid-connect", "protocolMapper": "oidc-usersessionmodel-note-mapper",
             "config": {"user.session.note": "client_id", "claim.name": "client_id", "access.token.claim": "true",
                        "jsonType.label": "String"}}
def caller(cid, secret, audience):
    return {"clientId": cid, "secret": secret, "publicClient": False, "standardFlowEnabled": False,
            "serviceAccountsEnabled": True, "protocolMappers": [aud(audience), client_id]}
groups_claim = {"name": "groups", "protocol": "openid-connect", "protocolMapper": "oidc-group-membership-mapper",
                "config": {"claim.name": "groups", "full.path": "false", "access.token.claim": "true"}}
nodes, node_users = [], []
for g in groups:
    subject = f"system:serviceaccount:hugr:hugr-node-{g}"
    # the client is named by its subject: the flow sends client_id, and Keycloak's federated authentication takes
    # only the assertion's sub there
    nodes.append({"clientId": subject, "publicClient": False, "standardFlowEnabled": False,
                  "serviceAccountsEnabled": True, "clientAuthenticatorType": "federated-jwt",
                  "attributes": {"jwt.credential.issuer": "k8s", "jwt.credential.sub": subject},
                  "protocolMappers": [aud("duckdb-secrets"), groups_claim, client_id]})
    # every node's identity is in one group: tresor grants the nodes' secrets to it (the owner, 2026-10-18)
    node_users.append({"username": f"service-account-hugr-node-{g}", "enabled": True, "serviceAccountClientId": subject,
                       "groups": ["/hugr-nodes"]})
realm = {
    "realm": "hugr", "enabled": True, "eventsEnabled": True, "enabledEventTypes": ["CLIENT_LOGIN", "CLIENT_LOGIN_ERROR"],
    "roles": {"realm": [{"name": n} for n in ["analyst_a", "analyst_b", "platform_admin", "demo_seeder"]]},
    "groups": [{"name": "hugr-nodes", "path": "/hugr-nodes"}],
    # the cluster's ServiceAccount issuer: Keycloak reads its keys with its own ServiceAccount token
    "identityProviders": [{"alias": "k8s", "providerId": "kubernetes", "enabled": True, "config": {"issuer": sa_issuer}}],
    "clients": nodes + [
        caller("demo-seeder", os.environ["DEMO_SEEDER"], "duckdb-secrets"),
        caller("demo-analyst-a", os.environ["DEMO_ANALYST_A"], "hugr"),
        caller("demo-analyst-b", os.environ["DEMO_ANALYST_B"], "hugr"),
        caller("demo-admin", os.environ["DEMO_ADMIN"], "hugr"),
    ],
    "users": node_users + [
        {"username": "service-account-demo-seeder", "enabled": True, "serviceAccountClientId": "demo-seeder", "realmRoles": ["demo_seeder"]},
        {"username": "service-account-demo-analyst-a", "enabled": True, "serviceAccountClientId": "demo-analyst-a", "realmRoles": ["analyst_a"]},
        {"username": "service-account-demo-analyst-b", "enabled": True, "serviceAccountClientId": "demo-analyst-b", "realmRoles": ["analyst_b"]},
        {"username": "service-account-demo-admin", "enabled": True, "serviceAccountClientId": "demo-admin", "realmRoles": ["platform_admin"]},
    ],
}
json.dump(realm, open(out, "w"))
PY
	k -n idp delete secret kc-realm --ignore-not-found >/dev/null
	k -n idp create secret generic kc-realm --from-file=realm.json="$WORK/realm.json" >/dev/null
	# Keycloak imports a realm it does not have: a changed realm rolls its pod (start-dev keeps nothing)
	local realm_hash
	realm_hash="$(openssl dgst -sha256 -r "$WORK/realm.json" | cut -c1-16)"
	rm -f "$WORK/realm.json"
	k apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: keycloak, namespace: idp}
---
# Keycloak reads the cluster's ServiceAccount issuer (discovery, keys) with its own token
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: hugr-demo-kc-issuer-discovery}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: system:service-account-issuer-discovery}
subjects: [{kind: ServiceAccount, name: keycloak, namespace: idp}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: keycloak, namespace: idp}
spec:
  selector: {matchLabels: {app: keycloak}}
  template:
    metadata: {labels: {app: keycloak}, annotations: {hugr.demo/realm: "$realm_hash"}}
    spec:
      serviceAccountName: keycloak
      containers:
        - name: keycloak
          image: $KEYCLOAK_IMAGE
          args: [start-dev, --import-realm, --https-port=8443, --https-certificate-file=/tls/tls.crt,
                 --https-certificate-key-file=/tls/tls.key, --hostname=https://keycloak.idp.svc:8443,
                 --truststore-paths=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt]
          ports: [{containerPort: 8443}]
          readinessProbe: {httpGet: {path: /realms/hugr, port: 8443, scheme: HTTPS}, periodSeconds: 5, failureThreshold: 60}
          resources: {requests: {cpu: 100m, memory: 512Mi}}
          volumeMounts:
            - {name: tls, mountPath: /tls, readOnly: true}
            - {name: realm, mountPath: /opt/keycloak/data/import, readOnly: true}
      volumes:
        - {name: tls, secret: {secretName: keycloak-tls}}
        - {name: realm, secret: {secretName: kc-realm}}
---
apiVersion: v1
kind: Service
metadata: {name: keycloak, namespace: idp}
spec: {selector: {app: keycloak}, ports: [{port: 8443, targetPort: 8443}]}
YAML
	rollout idp keycloak 420s
	ok "Keycloak in idp: the realm hugr at $KC_ISSUER, the ServiceAccount issuer $sa_issuer"
}

# --- tresor-server: the nodes' secrets, the seeder its admin ---------------------------------------------------------
install_tresor() {
	step "tresor-server: the catalog's login and the source's, for the groups' identities"
	namespace tresor
	ca_cert tresor tresor.tresor.svc
	tls_secret tresor tresor-tls tresor
	k -n tresor delete secret kek --ignore-not-found >/dev/null
	# the KEK seals tresor's state in the cluster: the run's own, kept between runs, or the state no longer opens
	k -n tresor create secret generic kek --from-literal=key="$(secret_b64 tresor-kek 32)" >/dev/null
	k -n tresor delete configmap ca --ignore-not-found >/dev/null
	k -n tresor create configmap ca --from-file=ca.crt="$WORK/ca.crt" >/dev/null
	local actors="" g
	for g in "${GROUPS_[@]}"; do
		actors="$actors      - {principal: 'client:system:serviceaccount:hugr:hugr-node-$g', verbs: [use]}
"
	done
	cat >"$WORK/tresor.yaml" <<YAML
fullnameOverride: tresor
image: {repository: tresor-server, tag: demo, pullPolicy: Never}
localKEK: {secretName: kek}
tlsSecret: tresor-tls
env: [{name: SSL_CERT_FILE, value: /etc/tresor-ca/ca.crt}]
extraVolumes: [{name: ca, configMap: {name: ca}}]
extraVolumeMounts: [{name: ca, mountPath: /etc/tresor-ca, readOnly: true}]
resources: {requests: {cpu: 10m, memory: 32Mi}}
service: {port: 443}
config:
  public_url: https://$TRESOR_HOST
  issuers:
    - issuer: $KC_ISSUER
      audience: duckdb-secrets
      roles_claim: realm_access.roles
      groups_claim: groups
      service: {claim: client_id}
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
	ok "tresor-server in tresor at https://$TRESOR_HOST"
}

# --- kista: a channel for the node's duckdb, the release's extensions in it ------------------------------------------
# Its channel key is in OpenBao Transit, reached by kista's own ServiceAccount token (as kista's kind test): kista
# refuses a key file from a Secret, on purpose.
OPENBAO_IMAGE="${OPENBAO_IMAGE:-openbao/openbao:2.7.1}"

install_openbao() {
	ca_cert openbao openbao.kista.svc
	tls_secret kista openbao-tls openbao
	k -n kista delete secret openbao-root --ignore-not-found >/dev/null
	k -n kista create secret generic openbao-root --from-literal=token="$(secret_value openbao-root)" >/dev/null
	k -n kista delete configmap openbao-config --ignore-not-found >/dev/null
	k -n kista create configmap openbao-config --from-literal=bao.hcl='listener "tcp" {
  address       = "0.0.0.0:8200"
  tls_cert_file = "/tls/tls.crt"
  tls_key_file  = "/tls/tls.key"
}' >/dev/null
	k apply -f - >/dev/null <<'YAML'
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
          image: openbao/openbao:2.7.1
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
	k -n kista set image deploy/openbao openbao="$OPENBAO_IMAGE" >/dev/null
	rollout kista openbao
	# the root token stays in the pod's environment: the command reads it there, never on a command line here
	bao() { k -n kista exec -i deploy/openbao -- sh -c 'BAO_ADDR=http://127.0.0.1:8201 BAO_TOKEN="$BAO_DEV_ROOT_TOKEN_ID" bao "$@"' bao "$@"; }
	eventually 60 "OpenBao answers" bao status
	bao secrets list | grep -q '^transit/' || bao secrets enable transit >/dev/null
	bao read transit/keys/ext-demo >/dev/null 2>&1 || bao write -f transit/keys/ext-demo type=rsa-2048 >/dev/null
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
	ca_cert kista kista.kista.svc
	tls_secret kista kista-tls kista
	k -n kista delete secret kista-ca --ignore-not-found >/dev/null
	k -n kista create secret generic kista-ca --from-file=ca.crt="$WORK/ca.crt" >/dev/null
	cat >"$WORK/kista.yaml" <<YAML
image: {repository: kista, tag: demo, pullPolicy: Never}
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
	# kista's own failure stands (pipefail); its profile-dev warning does not
	ka() {
		k -n kista exec -i deploy/kista -c kista -- /kista admin -config /etc/kista/kista.yaml "$@" 2>&1 |
			{ grep -v 'WARNING: profile dev' || true; }
	}
	ka tenant list | grep -qw hugr || ka tenant create hugr >/dev/null
	ka channel list hugr | grep -qw demo || ka channel create hugr/demo -kind signed >/dev/null
	ka key list hugr/demo | grep -q 'ext-demo' || ka key add hugr/demo -signer bao:ext-demo:v1 -active >/dev/null
	local file meta name version
	for file in "$DIST"/*.duckdb_extension; do
		meta="$(python3 "$NODE_SRC/scripts/ext_footer.py" "$file")"
		name="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["name"])' "$meta")"
		version="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["duckdb_version"])' "$meta")"
		[ "$name" = hugr_node ] && continue # linked into the node
		ka version list | grep -qw -- "$version" || ka version add "$version" -kind dev >/dev/null
		ka channel versions hugr/demo | grep -qx -- "$version" || ka channel versions hugr/demo -add "$version" >/dev/null
		ka release list hugr/demo | grep -qw -- "$name" || ka release add hugr/demo - -name "$name" <"$file" >/dev/null
	done
	ok "kista in kista: $KISTA_PREFIX, its key in OpenBao Transit, the release's extensions in it"
}
