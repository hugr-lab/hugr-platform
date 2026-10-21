# shellcheck shell=bash
# AKS, by hand (spec 004): what only Azure proves - Entra ID logging the nodes in by their ServiceAccount tokens through
# managed identities. Nothing of Azure is kept between runs (the owner, 2026-10-18): a run makes its resource group
# (the cluster, its registry, the identities) and its Entra apps, and deletes them at its end, a failed check
# included. up / check / down alone are for debugging; leftovers lists what any run left (a killed shell).
#
# A run is its id: the state ($WORK/aks.env: the id, the subscription, the tenant) names the group
# hugr-platform-demo-<id>, the registry hugrdemo<id> and the Entra apps hugr-platform-demo-<id>-<name>, each tagged
# purpose=hugr-platform-demo, run=<id>, expires=<the run's day + 1>. up refuses a group of another run, down deletes
# only the run's group and the apps whose ids it recorded - never anything found by a name.
#   HUGR_AKS_LOCATION (westeurope), HUGR_AKS_VM (Standard_D4s_v6: a family with quota), HUGR_AKS_NODES (1). The
#   signed-in az account makes app registrations, assigns app roles (Application Administrator or the like) and
#   role assignments in the run's group.
AKS_LOCATION="${HUGR_AKS_LOCATION:-westeurope}"
AKS_CLUSTER=hugr-demo
DEMO_CONTEXT=hugr-demo-aks
# the cluster's credentials in the run's state: the developer's kubeconfig is never touched
export KUBECONFIG="$WORK/kubeconfig"
ARCH=amd64
PULL=Always
# the node pods of check 8 and a roll's surge on one 4-core VM
[ -n "${HUGR_DEMO_NODE_CPU:-}" ] || NODE_CPU=250m
RUN_ID=""
SUBSCRIPTION=""
AKS_RG=""
ACR=""

provider_need() {
	need az curl
	az account show -o none 2>/dev/null || fail "az is not signed in (az login)"
	if [ -f "$WORK/aks.env" ]; then
		# shellcheck disable=SC1091
		. "$WORK/aks.env"
	elif [ "$COMMAND" = up ] || [ "$COMMAND" = all ]; then
		RUN_ID="$(openssl rand -hex 4)"
		SUBSCRIPTION="$(az account show --query id -o tsv)"
		printf 'RUN_ID=%s\nSUBSCRIPTION=%s\nTENANT=%s\n' "$RUN_ID" "$SUBSCRIPTION" \
			"$(az account show --query tenantId -o tsv)" >"$WORK/aks.env"
	fi
	if [ -n "$RUN_ID" ]; then
		AKS_RG="hugr-platform-demo-$RUN_ID"
		ACR="hugrdemo$RUN_ID"
		REGISTRY="$ACR.azurecr.io/"
	fi
}

provider_context() {
	[ -n "$RUN_ID" ] || fail "no run here: test/demo/run.sh aks up"
	[ -f "$KUBECONFIG" ] || azs aks get-credentials -g "$AKS_RG" -n "$AKS_CLUSTER" --file "$KUBECONFIG" \
		--context "$DEMO_CONTEXT" -o none || fail "the cluster's credentials"
}

# azs: az on the run's subscription, whatever the account's default is now
azs() { az "$@" --subscription "$SUBSCRIPTION"; }
aks_tags() { echo "purpose=hugr-platform-demo run=$RUN_ID expires=$(days_from_now 1 date)"; }
# ours: the group is this run's (its run tag)
ours() { [ "$(azs group show -n "$AKS_RG" --query tags.run -o tsv 2>/dev/null)" = "$RUN_ID" ]; }

provider_up() {
	step "AKS: the group $AKS_RG, the registry $ACR, the cluster $AKS_CLUSTER (several minutes)"
	if [ "$(azs group exists -n "$AKS_RG")" = true ]; then
		ours || fail "the resource group $AKS_RG is not this run's (its run tag): nothing is done to it"
	else
		# shellcheck disable=SC2046 # the tags are words
		azs group create -n "$AKS_RG" -l "$AKS_LOCATION" --tags $(aks_tags) -o none || fail "the resource group $AKS_RG"
	fi
	if ! azs acr show -n "$ACR" -o none 2>/dev/null; then
		# shellcheck disable=SC2046
		azs acr create -g "$AKS_RG" -n "$ACR" --sku Basic --tags $(aks_tags) -o none || fail "the registry $ACR"
	fi
	if ! azs aks show -g "$AKS_RG" -n "$AKS_CLUSTER" -o none 2>/dev/null; then
		# the OIDC issuer: Entra trusts the ServiceAccount tokens a node presents (the node exchanges its own projected
		# token; no workload-identity webhook); cilium: the platform's NetworkPolicies are enforced
		# shellcheck disable=SC2046
		azs aks create -g "$AKS_RG" -n "$AKS_CLUSTER" --tier free --node-count "${HUGR_AKS_NODES:-1}" \
			--node-vm-size "${HUGR_AKS_VM:-Standard_D4s_v6}" --enable-oidc-issuer --attach-acr "$ACR" --no-ssh-key \
			--network-plugin azure --network-plugin-mode overlay --network-dataplane cilium \
			--tags $(aks_tags) -o none || fail "AKS did not make the cluster"
	fi
	azs aks get-credentials -g "$AKS_RG" -n "$AKS_CLUSTER" --file "$KUBECONFIG" --context "$DEMO_CONTEXT" \
		--overwrite-existing -o none || fail "the cluster's credentials"
	chmod 600 "$KUBECONFIG"
	docker_login
	ok "AKS $AKS_CLUSTER runs (amd64), its images pulled from $ACR"
}

# docker_login: a push token for the run's registry, in a docker config of the run's state - never the developer's
# keychain (an auths entry written first keeps docker on the file store), and the daemon the developer's context uses
docker_login() {
	DOCKER_HOST="$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)"
	[ -n "$DOCKER_HOST" ] && export DOCKER_HOST
	mkdir -p "$WORK/docker" && chmod 700 "$WORK/docker"
	(umask 077 && printf '{"auths": {"%s.azurecr.io": {}}}\n' "$ACR" >"$WORK/docker/config.json")
	azs acr login -n "$ACR" --expose-token --query accessToken -o tsv 2>/dev/null |
		docker --config "$WORK/docker" login "$ACR.azurecr.io" -u 00000000-0000-0000-0000-000000000000 \
			--password-stdin >/dev/null 2>&1 || fail "a push token for $ACR"
	grep -q credsStore "$WORK/docker/config.json" && fail "docker put the push token in a credential store"
	return 0
}

# provider_build <name> <context>: built here for amd64 and pushed to the run's registry: the operator's image
# cross-compiles (BuildKit's platform arguments), the node's builds emulated on an arm64 machine
provider_build() {
	docker build -q --platform linux/amd64 -t "$(img "$1")" --build-arg VERSION=demo "$2" >"$WORK/build-$1.log" 2>&1 || {
		tail -30 "$WORK/build-$1.log" >&2
		fail "the image $1"
	}
	docker --config "$WORK/docker" push -q "$(img "$1")" >/dev/null || fail "pushing $1 to $ACR"
}
provider_import() { azs acr import -n "$ACR" --source "$1" -t "$2:demo" --force -o none || fail "importing $1 into $ACR"; }
provider_load() { :; }

provider_down() {
	step "Azure: deleting the run $RUN_ID - its Entra apps, the group $AKS_RG (the cluster, the registry, the identities)"
	[ -n "$RUN_ID" ] || {
		ok "no run here (nothing recorded in $WORK): leftovers lists what other runs left"
		return 0
	}
	idp_down
	if [ "$(azs group exists -n "$AKS_RG")" = true ]; then
		ours || fail "the resource group $AKS_RG is not this run's (its run tag): left alone"
		azs group delete -n "$AKS_RG" --yes -o none || fail "deleting $AKS_RG: test/demo/run.sh aks leftovers"
	fi
	rm -rf "$WORK"
	ok "deleted (AKS may have made NetworkWatcherRG in $AKS_LOCATION: Azure's, free, shared - left alone)"
}

provider_leftovers() {
	step "Azure: what runs of the demo left (every run, by the purpose tag and the name prefix)"
	echo "resource groups:"
	az group list --tag purpose=hugr-platform-demo --query '[].{name:name, run:tags.run, expires:tags.expires}' -o table
	idp_leftovers
}

# provider_metrics: AKS runs metrics-server
provider_metrics() { :; }
