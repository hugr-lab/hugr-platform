# shellcheck shell=bash
# minikube, a developer's machine (spec 004): the profile hugr-demo, made if missing and kept between runs; the
# caller's current kube context is left as it was.
DEMO_PROFILE="${HUGR_DEMO_PROFILE:-hugr-demo}"
DEMO_CONTEXT="$DEMO_PROFILE"

provider_need() { need minikube; }

provider_up() {
	step "minikube: the profile $DEMO_PROFILE"
	if ! minikube -p "$DEMO_PROFILE" status >/dev/null 2>&1; then
		local before
		before="$(command kubectl config current-context 2>/dev/null || true)"
		minikube start -p "$DEMO_PROFILE" --driver=docker --memory "${HUGR_DEMO_MEMORY:-12g}" \
			--cpus "${HUGR_DEMO_CPUS:-6}" >"$WORK/minikube.log" 2>&1 || {
			tail -20 "$WORK/minikube.log" >&2
			fail "minikube did not start"
		}
		# minikube makes its context the current one: the caller's stays theirs
		[ -z "$before" ] || command kubectl config use-context "$before" >/dev/null
	fi
	ARCH="$(docker info --format '{{.Architecture}}' | sed -e 's/aarch64/arm64/' -e 's/x86_64/amd64/')"
	ok "minikube $DEMO_PROFILE runs ($ARCH)"
}

provider_load() {
	minikube -p "$DEMO_PROFILE" image load --overwrite=true "$1" >/dev/null || fail "loading $1 into minikube"
}

# the end of a run: the namespaces go, the profile stays for the next run (HUGR_DEMO_DELETE=1 deletes it)
provider_down() {
	if [ -n "${HUGR_DEMO_DELETE:-}" ]; then
		minikube delete -p "$DEMO_PROFILE" >/dev/null 2>&1 || true
		return
	fi
	k delete namespace hugr idp tresor kista db --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
