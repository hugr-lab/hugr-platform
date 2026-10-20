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

# platform_values: the chart's values for this run - no credential (spec 005): images, the IdP, tresor, kista, the sizes
platform_values() {
	local operator g replicas audience="" scope=""
	operator="$(img hugr-platform)"
	[ -z "$IDP_FEDERATION_AUDIENCE" ] || audience="
      federationAudience: \"$IDP_FEDERATION_AUDIENCE\""
	[ -z "$TRESOR_SCOPE" ] || scope="
      scope: \"$TRESOR_SCOPE\""
	cat <<YAML
operator:
  image: {repository: "${operator%:*}", tag: "${operator##*:}", pullPolicy: $PULL}
  replicas: 1
internalCA: {existingSecret: hugr-internal-ca}
# the checks and the console stand reach the console through a port-forward, at 127.0.0.1
console: {dnsNames: [localhost], ipAddresses: [127.0.0.1]}
front: {dnsNames: [localhost], ipAddresses: [127.0.0.1]}
admissionPolicies: {enabled: true}
platform:
  name: hugr
  spec:
    node: {image: "$(img hugr-node)", imagePullPolicy: $PULL}
    idp:
      issuer: "$IDP_ISSUER"$audience
    tresor:
      host: "$TRESOR_HOST"$scope
    repository: {name: hugr, prefix: "$KISTA_PREFIX"}
    heartbeat: {intervalSeconds: 5}
    front: {replicas: 2}
    # the console (spec 003): the realm's public client hugr-console - a browser signs the person operator in through
    # the console stand (run.sh <provider> console); the checks call the API with a principal's token
    console:
      clientID: hugr-console
      audiences: {nodes: "$(console_audience "$HUGR_AUDIENCE")", tresor: "$(console_audience "$TRESOR_AUDIENCE")"}
    sizes:
      small:
        resources: {requests: {cpu: "$NODE_CPU", memory: 1Gi}, limits: {memory: 2Gi}}
        temp: {medium: local, size: 2Gi}
    plan: {maxNodes: 4, sizes: [small]}
# the run's CA lives 30 days: the doors' certificates are a day's
groupCertificateDuration: 24h
groups:
YAML
	for g in "${GROUPS_[@]}"; do
		replicas=1
		[ "$g" = a ] && replicas=2
		echo "  - {name: $g, spec: {replicas: $replicas, size: small, identity: {clientID: \"$(group_client_id "$g")\"}, drainSeconds: 10}}"
	done
}

# from_kustomize: a kept cluster from before the chart holds the operator and the platform kubectl applied - not
# Helm's, so the chart's install would refuse them: deleted once, with the groups (the nodes start anew anyway)
from_kustomize() {
	helm_ status hugr -n hugr >/dev/null 2>&1 && return 0
	k -n hugr get deploy hugr-platform >/dev/null 2>&1 || return 0
	k -n hugr delete hng --all --wait --timeout=300s >/dev/null 2>&1 || true
	k -n hugr delete hugrplatform --all --wait --timeout=120s >/dev/null 2>&1 || true
	k kustomize "$HERE/overlay" | k delete -f - --ignore-not-found --timeout=300s >/dev/null 2>&1 || true
	k delete -k "$ROOT/config/policy" --ignore-not-found >/dev/null 2>&1 || true
}

install_operator() {
	install_cert_manager
	step "the platform's chart: the operator, its rights, the webhook, the certificates, the HugrPlatform and its groups"
	namespace hugr
	from_kustomize
	# the run's CA is the platform's internal CA: what the platform's Issuer signs - the operator's certificate, the
	# console's and the front's, every door's - chains to the CA the checks trust
	k -n hugr delete secret hugr-internal-ca --ignore-not-found >/dev/null
	k -n hugr create secret tls hugr-internal-ca --cert "$WORK/ca.crt" --key "$WORK/ca.key" >/dev/null ||
		fail "the internal CA's Secret"
	platform_values >"$WORK/platform-values.yaml"
	local again=""
	k -n hugr get deploy hugr-node-a >/dev/null 2>&1 && again=1
	# the nodes' logs are kept from here on, each from its pod's start
	rm -rf "$WORK/logs"
	if ! helm_ upgrade --install hugr "$ROOT/charts/hugr-platform" -n hugr -f "$WORK/platform-values.yaml" \
		--wait --timeout 600s >"$WORK/platform-helm.log" 2>&1; then
		cat "$WORK/platform-helm.log" >&2
		k -n hugr get pods -o wide >&2 || true
		fail "the platform's chart did not install"
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
	# a run again: the catalog was made anew, so the nodes start anew (the group's restartedAt, as an operator would);
	# the chart made the groups at its install only, and leaves them to the console and kubectl after
	if [ -n "$again" ]; then
		for g in "${GROUPS_[@]}"; do restart_group "$g"; done
	fi
	ok "the operator serves: the node API on https://hugr-platform.hugr.svc:8443, the webhook, the console"
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
	k delete clusterrolebinding hugr-demo-openbao-tokenreview hugr-demo-kc-issuer-discovery --ignore-not-found \
		>/dev/null 2>&1 || true
}
