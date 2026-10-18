# shellcheck shell=bash
# kind, CI (spec 004): a cluster made and deleted by the run (HUGR_DEMO_KEEP=1 keeps it).
DEMO_CLUSTER="${HUGR_DEMO_CLUSTER:-hugr-demo}"
DEMO_CONTEXT="kind-$DEMO_CLUSTER"
# the cluster's context in the run's own kubeconfig: the developer's ~/.kube/config is never touched
export KUBECONFIG="$WORK/kubeconfig"

provider_need() { need kind; }

provider_up() {
	step "kind: the cluster $DEMO_CLUSTER"
	if ! kind get clusters 2>/dev/null | grep -qx "$DEMO_CLUSTER"; then
		kind create cluster --name "$DEMO_CLUSTER" --wait 120s >"$WORK/kind.log" 2>&1 || {
			tail -20 "$WORK/kind.log" >&2
			fail "kind did not make the cluster"
		}
	fi
	kind export kubeconfig --name "$DEMO_CLUSTER" >/dev/null 2>&1 || fail "the cluster's kube context"
	chmod 600 "$KUBECONFIG"
	ARCH="$(docker info --format '{{.Architecture}}' | sed -e 's/aarch64/arm64/' -e 's/x86_64/amd64/')"
	ok "kind $DEMO_CLUSTER runs ($ARCH)"
}

provider_context() { kind export kubeconfig --name "$DEMO_CLUSTER" >/dev/null 2>&1 || fail "the cluster's kube context"; }
provider_build() { local_build "$@"; }
provider_import() { local_import "$@"; }
provider_load() { kind load docker-image "$1" --name "$DEMO_CLUSTER" >/dev/null || fail "loading $1 into kind"; }

provider_down() {
	[ -n "${HUGR_DEMO_KEEP:-}" ] || kind delete cluster --name "$DEMO_CLUSTER" >/dev/null 2>&1 || true
}
