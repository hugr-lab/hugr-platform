# shellcheck shell=bash
# The operator and the platform (spec 004): the operator from this checkout (its certificate the run's), the
# HugrPlatform pointing at the demo's services, the groups a and b.

# cert-manager and its csi-driver: the platform chart's prerequisites (spec 005), at pinned versions
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.21.2}"
CSI_DRIVER_VERSION="${CSI_DRIVER_VERSION:-v0.16.0}"

install_cert_manager() {
	step "cert-manager and its csi-driver: the platform's certificates (spec 005)"
	local chart args
	for chart in "cert-manager $CERT_MANAGER_VERSION --set crds.enabled=true" "cert-manager-csi-driver $CSI_DRIVER_VERSION"; do
		# shellcheck disable=SC2086 # the chart, its version and its flags are words
		set -- $chart
		args=("${@:3}")
		if ! helm_ upgrade --install "$1" "oci://quay.io/jetstack/charts/$1" --version "$2" -n cert-manager \
			--create-namespace ${args[@]+"${args[@]}"} --wait --timeout 300s >"$WORK/$1-helm.log" 2>&1; then
			cat "$WORK/$1-helm.log" >&2
			fail "$1 did not install"
		fi
	done
	ok "cert-manager $CERT_MANAGER_VERSION, csi-driver $CSI_DRIVER_VERSION"
}

# chart_values: the umbrella's values for this run - no credential (spec 005): the images, the IdP, the vault, the
# database, the sizes, the groups. The CAs (public certificates) are given as files (--set-file)
chart_values() {
	local operator g replicas audience="" actors="" private=false
	operator="$(img hugr-platform)"
	[ -z "$IDP_FEDERATION_AUDIENCE" ] || audience="
      idp: {federationAudience: \"$IDP_FEDERATION_AUDIENCE\"}"
	# a private one (kind, minikube): tresor reads its discovery with its own token, by the cluster's CA
	case "$CLUSTER_ISSUER" in https://kubernetes.default.svc*) private=true ;; esac
	for g in "${GROUPS_[@]}"; do
		actors="$actors
        - {principal: 'client:$(group_client_id "$g")', verbs: [use]}"
	done
	cat <<YAML
global:
  database: {host: pg.db.svc}
  idp:
    issuer: "$IDP_ISSUER"
    tresorAudience: "$TRESOR_AUDIENCE"
    tresorRolesClaim: "$TRESOR_ROLES_CLAIM"
    tresorGroupsClaim: "$TRESOR_GROUPS_CLAIM"
    tresorServiceClaim: "$TRESOR_SERVICE_CLAIM"
    tresorScope: "$TRESOR_SCOPE"
  clusterIssuer: {url: "$CLUSTER_ISSUER", private: $private}
  keys:
    vault: {address: "$VAULT_ADDRESS"}
  duckdbVersion: "$DUCKDB_VERSION"
hugr-platform:
  operator:
    image: {repository: "${operator%:*}", tag: "${operator##*:}", pullPolicy: $PULL}
    replicas: 1
  internalCA: {existingSecret: hugr-internal-ca}
  # the checks and the console stand reach the console through a port-forward, at 127.0.0.1
  console: {dnsNames: [localhost], ipAddresses: [127.0.0.1]}
  front: {dnsNames: [localhost], ipAddresses: [127.0.0.1]}
  admissionPolicies: {enabled: true}
  platform:
    spec:
      node: {image: "$(img hugr-node)", imagePullPolicy: $PULL}$audience
      heartbeat: {intervalSeconds: 5}
      front: {replicas: 2}
      # the console (spec 003): the realm's public client hugr-console - a browser signs the person operator in through
      # the console stand (run.sh <provider> console); the checks call the API with a principal's token
      console:
        clientID: hugr-console
        audiences: {nodes: "$(console_audience "$HUGR_AUDIENCE")", tresor: "$(console_audience "$TRESOR_AUDIENCE")"}
      # the first policy (spec 005, hugr-node spec 015): the first node writes it into the catalog hugr-init made - the
      # issuer, the console's client, platform_admin for the realm's platform_admin, the groups a and b. The demo's own
      # data (the source, the analysts, the catalog demo) is the seed's, after
      firstPolicy:
        issuer: {name: idp, rolesClaim: "$HUGR_ROLES_CLAIM", audiences: ["$HUGR_AUDIENCE"]}
        administrators: platform_admin
      sizes:
        small:
          resources: {requests: {cpu: "$NODE_CPU", memory: 1Gi}, limits: {memory: 2Gi}}
          temp: {medium: local, size: 2Gi}
      plan: {maxNodes: 4, sizes: [small]}
  # hugr-init (spec 005): the catalog's secret in tresor for the groups' identities and the seeder (which writes the
  # demo's data into the catalog)
  hugrInit:
    tresor:
      grants: ["$NODES_PRINCIPAL", "role:demo_seeder"]
  # the run's CA lives 30 days: the doors' certificates are a day's
  groupCertificateDuration: 24h
  groups:
YAML
	for g in "${GROUPS_[@]}"; do
		replicas=1
		[ "$g" = a ] && replicas=2
		echo "    - {name: $g, spec: {replicas: $replicas, size: small, identity: {clientID: \"$(group_client_id "$g")\"}, drainSeconds: 10}}"
	done
	cat <<YAML
tresor:
  clusterIssuer: {enabled: $private}
  resources: {requests: {cpu: 10m, memory: 32Mi}}
  config:
    policy:
      # hugr-init's (the umbrella's), the seeder (the demo's data) and the console stand's person (spec 003: tresor's
      # console mounted)
      admins:
        - "subject:{{ .Values.global.clusterIssuer.url }}|system:serviceaccount:{{ .Release.Namespace }}:hugr-init"
        - role:demo_seeder
        - role:secrets_admin
      # the nodes act for their sessions (ACT_FOR_SESSIONS); an actor is a service, client:<id>
      actors:$actors
kista:
  resources: {requests: {cpu: 20m, memory: 64Mi}, limits: {memory: 512Mi}}
  blobVolume: {size: 2Gi}
  config:
    blob: {max_body: 256MiB}
YAML
}

# teardown: a run installs the umbrella afresh - the groups and the release go (while the operator still serves them),
# then the platform's and the services' namespaces with all they hold, and the three databases and logins; the
# cluster, the IdP, the vault, the PostgreSQL server and the images stay. The demo's layout before the umbrella
# (tresor, kista, a kustomize install) goes too.
teardown() {
	if k get crd hugrnodegroups.platform.hugr.io >/dev/null 2>&1; then
		k -n hugr delete hng --all --wait --timeout=300s >/dev/null 2>&1 || true
		k -n hugr delete hugrplatform --all --wait --timeout=120s >/dev/null 2>&1 || true
	fi
	if helm_ status hugr -n hugr >/dev/null 2>&1; then
		helm_ uninstall hugr -n hugr --wait --timeout 300s >"$WORK/uninstall.log" 2>&1 || {
			cat "$WORK/uninstall.log" >&2
			fail "the last run's release did not go"
		}
	fi
	helm_ uninstall tresor -n tresor >/dev/null 2>&1 || true
	k kustomize "$HERE/overlay" | k delete -f - --ignore-not-found --timeout=300s >/dev/null 2>&1 || true
	k delete -k "$ROOT/config/policy" --ignore-not-found >/dev/null 2>&1 || true
	k delete clusterrolebinding hugr-demo-tresor-issuer-discovery --ignore-not-found >/dev/null 2>&1 || true
	k delete namespace hugr "$TRESOR_NS" "$KISTA_NS" tresor kista --ignore-not-found --wait --timeout=300s >/dev/null ||
		fail "the last run's namespaces are still being deleted"
	drop_databases
}

install_chart() {
	install_cert_manager
	step "the umbrella chart hugr: the operator, tresor-server and kista in their namespaces, hugr-init, the groups"
	teardown
	namespace hugr
	# the run's CA is the platform's internal CA: what the platform's Issuer signs - the operator's certificate, the
	# console's and the front's, every door's - chains to the CA the checks trust
	k -n hugr delete secret hugr-internal-ca --ignore-not-found >/dev/null
	k -n hugr create secret tls hugr-internal-ca --cert "$WORK/ca.crt" --key "$WORK/ca.key" >/dev/null ||
		fail "the internal CA's Secret"
	# the database administrator's credential, once: hugr-init deletes it after use (spec 005)
	k -n hugr delete secret hugr-init-db-admin --ignore-not-found >/dev/null
	k -n hugr create secret generic hugr-init-db-admin --from-literal=username=postgres \
		--from-file=password="$(secret_file pg openssl rand -hex 24)" >/dev/null || fail "hugr-init's administrator Secret"
	CLUSTER_ISSUER="$(k get --raw /.well-known/openid-configuration |
		python3 -c 'import json, sys; print(json.load(sys.stdin)["issuer"])')" || fail "the cluster's ServiceAccount issuer"
	chart_values >"$WORK/chart-values.yaml"
	# the database, the vault and Keycloak are the run's CA's; Entra is the system roots'
	local cas=(--set-file "global.database.caCert=$WORK/ca.crt" --set-file "global.keys.vault.caCert=$WORK/ca.crt")
	[ "$IDP_NAME" != Keycloak ] || cas+=(--set-file "global.idp.caCert=$WORK/ca.crt")
	# the nodes' logs are kept from here on, each from its pod's start
	rm -rf "$WORK/logs"
	# this checkout's platform chart, packaged beside the pinned tresor-server and kista
	helm_ dependency build "$ROOT/charts/hugr" >"$WORK/chart-deps.log" 2>&1 || {
		cat "$WORK/chart-deps.log" >&2
		fail "the umbrella's dependencies"
	}
	if ! helm_ upgrade --install hugr "$ROOT/charts/hugr" -n hugr -f "$WORK/chart-values.yaml" "${cas[@]}" \
		--wait --timeout 900s >"$WORK/chart-helm.log" 2>&1; then
		cat "$WORK/chart-helm.log" >&2
		local ns
		for ns in hugr "$TRESOR_NS" "$KISTA_NS"; do k -n "$ns" get pods -o wide >&2 || true; done
		k -n hugr logs -l app.kubernetes.io/component=init --tail 40 --prefix 2>&1 | redact >&2 || true
		k -n "$TRESOR_NS" logs -l app.kubernetes.io/instance=hugr --tail 40 --prefix 2>&1 | redact >&2 || true
		k -n "$KISTA_NS" logs -l app.kubernetes.io/instance=hugr --tail 40 --prefix 2>&1 | redact >&2 || true
		fail "the umbrella chart did not install"
	fi
	rollout hugr hugr-platform
	# a group the webhook judges answers through the operator's Service: a dry run of one
	webhook_answers() {
		k -n hugr apply --dry-run=server -f - >/dev/null 2>"$WORK/webhook.err" <<'YAML'
apiVersion: platform.hugr.io/v1alpha1
kind: HugrNodeGroup
metadata: {name: probe, namespace: hugr}
spec: {platform: hugr, replicas: 0, size: small, identity: {}, tls: {csi: {issuerRef: {name: hugr-internal}}}}
YAML
		! grep -q 'failed calling webhook' "$WORK/webhook.err"
	}
	eventually 60 "the webhook answers" webhook_answers
	ok "the release hugr: the operator serves (the node API, the webhook, the console), tresor at https://$TRESOR_HOST, kista at $KISTA_PREFIX; hugr-init made the databases and the catalog's secret"
}

install_platform() {
	step "the platform: HugrPlatform, the groups a (2 nodes) and b (1)"
	local g
	follow_logs
	for g in "${GROUPS_[@]}"; do
		eventually 60 "the operator makes group $g's Deployment" k -n hugr get "deploy/hugr-node-$g"
		if ! k -n hugr rollout status "deploy/hugr-node-$g" --timeout 300s >/dev/null 2>&1; then
			k -n hugr get pods -o wide >&2 || true
			k -n hugr logs -l "platform.hugr.io/group=$g" --tail 60 --prefix 2>&1 | redact >&2 || true
			fail "group $g's nodes are not ready"
		fi
	done
	eventually 60 "the operator makes the front's Deployment" k -n hugr get deploy/hugr-front
	if ! k -n hugr rollout status deploy/hugr-front --timeout 180s >/dev/null 2>&1; then
		k -n hugr logs -l app.kubernetes.io/name=hugr-front --tail 40 --prefix 2>&1 | redact >&2 || true
		fail "the front is not ready"
	fi
	follow_logs
	ok "both groups' nodes serve, and the front"
}

# restart_group <g>: the group's restartedAt, as an operator sets it, and the roll it makes - waited for once the
# operator has put it into the Deployment's pod template (a rollout status before that sees the last roll, finished)
restart_group() {
	local at
	at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	k -n hugr patch hng "$1" --type merge -p "{\"spec\":{\"restartedAt\":\"$at\"}}" >/dev/null || fail "restarting group $1"
	eventually 120 "the operator puts group $1's restart into its Deployment" template_restarted "$1" "$at"
	k -n hugr rollout status "deploy/hugr-node-$1" --timeout 300s >/dev/null || fail "group $1 did not roll"
	follow_logs
}

# follow_logs: every running node pod's log, kept in $WORK/logs/<pod>.log from the pod's start while the run lasts -
# the pods that go away included: the drain checks and the secrets scan read them
follow_logs() {
	mkdir -p "$WORK/logs"
	local pod pid
	for pod in $(k -n hugr get pods -l 'platform.hugr.io/group' --field-selector=status.phase=Running \
		-o jsonpath='{.items[*].metadata.name}') $(k -n hugr get pods -l app.kubernetes.io/name=hugr-front \
		--field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}'); do
		pid="$(cat "$WORK/logs/$pod.pid" 2>/dev/null || true)"
		[ -n "$pid" ] && ps -p "$pid" -o command= 2>/dev/null | grep -q 'kubectl.*logs' && continue
		# kubectl itself, never the function k (forward says why)
		command kubectl --context "$DEMO_CONTEXT" -n hugr logs -f "$pod" </dev/null >"$WORK/logs/$pod.log" 2>&1 &
		echo "$!" >"$WORK/logs/$pod.pid"
		echo "$!" >>"$WORK/forwards.pid"
	done
}
template_restarted() {
	[ "$(k -n hugr get deploy "hugr-node-$1" -o jsonpath='{.spec.template.metadata.annotations.platform\.hugr\.io/restartedAt}')" = "$2" ]
}

# down_cluster_scoped: what the namespaces' deletion leaves in a kept cluster - the chart's cluster-scoped objects (its
# release went with the namespace's Secrets), its CRDs (crds/ is never deleted by Helm), the demo's own bindings
down_cluster_scoped() {
	k delete clusterrole hugr-hugr-platform-operator --ignore-not-found >/dev/null 2>&1 || true
	k delete clusterrolebinding hugr-hugr-platform-auth-delegator --ignore-not-found >/dev/null 2>&1 || true
	k delete validatingwebhookconfiguration hugr-hugr-platform --ignore-not-found >/dev/null 2>&1 || true
	k delete validatingadmissionpolicybinding hugr-hugr-platform-group-pods hugr-hugr-platform-group-replicasets \
		hugr-hugr-platform-group-deployments --ignore-not-found >/dev/null 2>&1 || true
	k delete validatingadmissionpolicy hugr-hugr-platform-group-pods hugr-hugr-platform-group-replicasets \
		hugr-hugr-platform-group-deployments --ignore-not-found >/dev/null 2>&1 || true
	# waited for: an up right after installs the CRDs again
	k delete -f "$ROOT/charts/hugr-platform/crds" --ignore-not-found --timeout=300s >/dev/null 2>&1 || true
	# before the chart: the kustomize install's own names
	k kustomize "$HERE/overlay" | k delete -f - --ignore-not-found --timeout=300s >/dev/null 2>&1 || true
	k delete -k "$ROOT/config/policy" --ignore-not-found >/dev/null 2>&1 || true
	k delete clusterrolebinding hugr-demo-openbao-tokenreview hugr-demo-kc-issuer-discovery \
		hugr-demo-tresor-issuer-discovery --ignore-not-found >/dev/null 2>&1 || true
}
