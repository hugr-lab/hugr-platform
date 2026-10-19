# shellcheck shell=bash
# The operator and the platform (spec 004): the operator from this checkout (its certificate the run's), the
# HugrPlatform pointing at the demo's services, the groups a and b.

install_operator() {
	step "the operator: CRDs, its rights, the webhook, the node API"
	namespace hugr
	ca_cert operator hugr-platform.hugr.svc hugr-platform.hugr.svc.cluster.local
	k -n hugr delete secret hugr-platform-tls --ignore-not-found >/dev/null
	k -n hugr create secret tls hugr-platform-tls --cert "$WORK/operator.crt" --key "$WORK/operator.key" >/dev/null
	# the console's (spec 003): mounted at the operator's start - without it the operator serves no console
	ca_cert console hugr-platform-console.hugr.svc
	tls_secret hugr hugr-console console >/dev/null
	# the overlay names the image loaded into the cluster; on AKS it is the registry's, pulled
	k kustomize "$HERE/overlay" | sed -e "s#image: hugr-platform:demo#image: $(img hugr-platform)#" \
		-e "s#imagePullPolicy: Never#imagePullPolicy: $PULL#" | k apply -f - >"$WORK/operator-apply.log" 2>&1 || {
		cat "$WORK/operator-apply.log" >&2
		fail "the operator's install"
	}
	local bundle
	bundle="$(base64 <"$WORK/ca.crt" | tr -d '\n')"
	k patch validatingwebhookconfiguration hugr-platform --type json \
		-p "[{\"op\":\"add\",\"path\":\"/webhooks/0/clientConfig/caBundle\",\"value\":\"$bundle\"}]" >/dev/null
	k -n hugr rollout restart deploy/hugr-platform >/dev/null
	rollout hugr hugr-platform
	# the webhook answers through its Service once the new pod's endpoint is in: a dry run of a group, judged
	webhook_answers() {
		k -n hugr apply --dry-run=server -f - >/dev/null 2>"$WORK/webhook.err" <<'YAML'
apiVersion: platform.hugr.io/v1alpha1
kind: HugrNodeGroup
metadata: {name: probe, namespace: hugr}
spec: {platform: hugr, replicas: 0, size: small, identity: {}, tls: {secretRef: {name: hugr-node-probe-tls}}}
YAML
		! grep -q 'failed calling webhook' "$WORK/webhook.err"
	}
	eventually 60 "the webhook answers" webhook_answers
	ok "the operator serves: the node API on https://hugr-platform.hugr.svc:8443, the webhook, the console"
}

install_platform() {
	step "the platform: HugrPlatform, the groups a (2 nodes) and b (1)"
	local g audience="" scope=""
	[ -z "$IDP_FEDERATION_AUDIENCE" ] || audience=", federationAudience: \"$IDP_FEDERATION_AUDIENCE\""
	[ -z "$TRESOR_SCOPE" ] || scope=", scope: \"$TRESOR_SCOPE\""
	for g in "${GROUPS_[@]}"; do
		ca_cert "door-$g" "hugr-node-$g.hugr.svc"
		tls_secret hugr "hugr-node-$g-tls" "door-$g" >/dev/null # a rerun restarts the groups, the new one included
	done
	# the front's public certificate (spec 002): the run's CA here, the organisation's or cert-manager's in a cluster
	ca_cert front "hugr-front.hugr.svc"
	tls_secret hugr hugr-front front >/dev/null
	k apply -f - >/dev/null <<YAML
apiVersion: platform.hugr.io/v1alpha1
kind: HugrPlatform
metadata: {name: hugr, namespace: hugr}
spec:
  node: {image: "$(img hugr-node)", imagePullPolicy: $PULL}
  idp: {issuer: "$IDP_ISSUER"$audience}
  tresor: {host: "$TRESOR_HOST"$scope}
  repository: {name: hugr, prefix: "$KISTA_PREFIX"}
  caBundle: {configMapRef: {name: hugr-ca, key: ca.crt}}
  heartbeat: {intervalSeconds: 5}
  front: {replicas: 2, certificate: {secretName: hugr-front}}
  # the console (spec 003): the browser's public client is the installation's; the demo signs no browser in (the
  # console's own end to end does, test/console/e2e) - its checks call the API with a principal's token
  console:
    clientID: hugr-console
    audiences: {nodes: "$(console_audience "$HUGR_AUDIENCE")", tresor: "$(console_audience "$TRESOR_AUDIENCE")"}
  sizes:
    small:
      resources: {requests: {cpu: "$NODE_CPU", memory: 1Gi}, limits: {memory: 2Gi}}
      temp: {medium: local, size: 2Gi}
  plan: {maxNodes: 4, sizes: [small]}
YAML
	local replicas again=""
	k -n hugr get deploy hugr-node-a >/dev/null 2>&1 && again=1
	# the nodes' logs are kept from here on, each from its pod's start
	rm -rf "$WORK/logs"
	follow_logs
	for g in "${GROUPS_[@]}"; do
		replicas=1
		[ "$g" = a ] && replicas=2
		k apply -f - >/dev/null <<YAML
apiVersion: platform.hugr.io/v1alpha1
kind: HugrNodeGroup
metadata: {name: $g, namespace: hugr}
spec:
  platform: hugr
  replicas: $replicas
  size: small
  identity: {clientID: "$(group_client_id "$g")"}
  drainSeconds: 10
  tls: {secretRef: {name: hugr-node-$g-tls}}
YAML
	done
	# a run again: the catalog was made anew, so the nodes start anew (the group's restartedAt, as an operator would)
	if [ -n "$again" ]; then
		for g in "${GROUPS_[@]}"; do restart_group "$g"; done
	fi
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

# down_cluster_scoped: what the namespaces' deletion leaves in a kept cluster - the operator's CRDs, rights and
# webhook, the admission policies, the demo's own bindings
down_cluster_scoped() {
	# waited for: an up right after applies the CRDs again
	k kustomize "$HERE/overlay" | k delete -f - --ignore-not-found --timeout=300s >/dev/null 2>&1 || true
	k delete -k "$ROOT/config/policy" --ignore-not-found >/dev/null 2>&1 || true
	k delete clusterrolebinding hugr-demo-openbao-tokenreview hugr-demo-kc-issuer-discovery --ignore-not-found \
		>/dev/null 2>&1 || true
}
