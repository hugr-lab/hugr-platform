# shellcheck shell=bash
# The IdP on kind and minikube (spec 004): Keycloak in the cluster. A node's client is authenticated by its
# ServiceAccount token (federated-jwt, no secret); the demo's principals are confidential clients.
#
# What every IdP file defines for the rest of the run:
#   IDP_NAME, IDP_ISSUER                    who issues the nodes' and the principals' tokens
#   IDP_FEDERATION_AUDIENCE                 a node's ServiceAccount token's audience (empty: the platform's default)
#   TRESOR_AUDIENCE, TRESOR_SCOPE           the nodes' and the seeder's tokens for tresor (the scope may be empty)
#   TRESOR_ROLES_CLAIM, TRESOR_GROUPS_CLAIM, TRESOR_SERVICE_CLAIM   how tresor reads them (the groups claim may be empty)
#   NODES_PRINCIPAL                         the tresor principal every node's identity is: the nodes' secrets go to it
#   HUGR_AUDIENCE, HUGR_ROLES_CLAIM         the principals' tokens for the nodes
#   group_client_id <g>                     the group's identity.clientID
#   principal_id <p>                        a principal's client id (p: seeder, analyst-a, analyst-b, admin)
#   install_idp, idp_checker_up, token_flags   the install; the checker's way to the token endpoint
KEYCLOAK_IMAGE="${KEYCLOAK_IMAGE:-quay.io/keycloak/keycloak:26.6.4}"
IDP_NAME=Keycloak
IDP_ISSUER="https://keycloak.idp.svc:8443/realms/hugr"
# Keycloak's federated client authentication takes the realm's issuer as the assertion's audience
IDP_FEDERATION_AUDIENCE="$IDP_ISSUER"
TRESOR_AUDIENCE=duckdb-secrets
TRESOR_SCOPE=""
TRESOR_ROLES_CLAIM=realm_access.roles
TRESOR_GROUPS_CLAIM=groups
TRESOR_SERVICE_CLAIM=client_id
# the nodes' identities are one Keycloak group (the owner, 2026-10-18): tresor grants to role: and group: only
NODES_PRINCIPAL=group:hugr-nodes
HUGR_AUDIENCE=hugr
HUGR_ROLES_CLAIM=realm_access.roles
KC_PORT=""

# the client is named by its subject: the flow sends client_id, and Keycloak's federated authentication takes only
# the assertion's sub there
group_client_id() { echo "system:serviceaccount:hugr:hugr-node-$1"; }
principal_id() { echo "demo-$1"; }

idp_checker_up() { KC_PORT="$(forward idp svc/keycloak 8443)"; }
token_flags() {
	echo "-token-url https://127.0.0.1:$KC_PORT/realms/hugr/protocol/openid-connect/token -token-host keycloak.idp.svc"
}

install_idp() {
	step "Keycloak: the realm hugr - the nodes by their ServiceAccount tokens, no secret of theirs"
	namespace idp
	ca_cert keycloak keycloak.idp.svc
	tls_secret idp keycloak-tls keycloak
	local sa_issuer
	sa_issuer="$(k get --raw /.well-known/openid-configuration | python3 -c 'import json,sys; print(json.load(sys.stdin)["issuer"])')"
	DEMO_SEEDER="$(secret_value principal-seeder)" DEMO_ANALYST_A="$(secret_value principal-analyst-a)" \
		DEMO_ANALYST_B="$(secret_value principal-analyst-b)" DEMO_ADMIN="$(secret_value principal-admin)" \
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
	ok "Keycloak in idp: the realm hugr at $IDP_ISSUER, the ServiceAccount issuer $sa_issuer"
}
