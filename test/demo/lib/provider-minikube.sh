# shellcheck shell=bash
# minikube, a developer's machine (spec 004): the profile hugr-demo, made if missing and kept between runs. Its
# context lives in the run's own kubeconfig: the developer's ~/.kube/config and current context are never touched,
# whatever happens to the run.
DEMO_PROFILE="${HUGR_DEMO_PROFILE:-hugr-demo}"
DEMO_CONTEXT="$DEMO_PROFILE"
export KUBECONFIG="$WORK/kubeconfig"

provider_need() { need minikube; }

provider_up() {
	step "minikube: the profile $DEMO_PROFILE"
	if ! minikube -p "$DEMO_PROFILE" status >/dev/null 2>&1; then
		# calico: the NetworkPolicies the platform ships are enforced (the default bridge CNI ignores them)
		minikube start -p "$DEMO_PROFILE" --driver=docker --cni=calico --memory "${HUGR_DEMO_MEMORY:-12g}" \
			--cpus "${HUGR_DEMO_CPUS:-6}" >"$WORK/minikube.log" 2>&1 || {
			tail -20 "$WORK/minikube.log" >&2
			fail "minikube did not start"
		}
	fi
	# a profile made before writes its context here too
	minikube -p "$DEMO_PROFILE" update-context >/dev/null 2>&1 || fail "the profile's kube context"
	chmod 600 "$KUBECONFIG"
	ARCH="$(docker info --format '{{.Architecture}}' | sed -e 's/aarch64/arm64/' -e 's/x86_64/amd64/')"
	ok "minikube $DEMO_PROFILE runs ($ARCH)"
}

# provider_context: the cluster's context in the run's kubeconfig, for a command without an up
provider_context() { minikube -p "$DEMO_PROFILE" update-context >/dev/null 2>&1 || fail "the profile $DEMO_PROFILE's kube context"; }
provider_build() { local_build "$@"; }
provider_import() { local_import "$@"; }
provider_load() {
	# loaded again only when it changed (a node image is hundreds of MB), judged by its build time: the ids differ
	# between Docker Desktop's containerd store (an index's) and minikube's (a config's) whatever the image
	local here there
	here="$(docker image inspect -f '{{.Created}}' "$1")"
	there="$(minikube -p "$DEMO_PROFILE" ssh -- docker image inspect -f '{{.Created}}' "$1" 2>/dev/null | tr -d '\r' || true)"
	[ "$here" = "$there" ] && return 0
	# docker load moves the tag to the new image; minikube's image load --overwrite left the old one in place
	docker save "$1" | minikube -p "$DEMO_PROFILE" ssh --native-ssh=false -- docker load >/dev/null ||
		fail "loading $1 into minikube"
	there="$(minikube -p "$DEMO_PROFILE" ssh -- docker image inspect -f '{{.Created}}' "$1" 2>/dev/null | tr -d '\r' || true)"
	[ "$here" = "$there" ] || fail "minikube holds another $1 than the one built here"
}

# the end of a run: the namespaces go, the profile stays for the next run (HUGR_DEMO_DELETE=1 deletes it)
provider_down() {
	if [ -n "${HUGR_DEMO_DELETE:-}" ]; then
		minikube delete -p "$DEMO_PROFILE" >/dev/null || fail "deleting the profile $DEMO_PROFILE"
		ok "the profile $DEMO_PROFILE is deleted"
		return
	fi
	minikube -p "$DEMO_PROFILE" status >/dev/null 2>&1 || {
		ok "the profile $DEMO_PROFILE does not run: nothing to delete"
		return
	}
	minikube -p "$DEMO_PROFILE" update-context >/dev/null 2>&1 || fail "the profile's kube context"
	# waited for: an up right after finds no namespace being deleted
	k delete namespace hugr idp hugr-tresor hugr-kista openbao db tresor kista --ignore-not-found --timeout=300s \
		>/dev/null || fail "deleting the namespaces"
	down_cluster_scoped
	ok "the demo's namespaces and cluster-wide objects are deleted; the profile $DEMO_PROFILE stays"
}

# provider_metrics: the pods' usage for the console (metrics-server, the profile's addon)
provider_metrics() { minikube -p "$DEMO_PROFILE" addons enable metrics-server >/dev/null 2>&1 || true; }
