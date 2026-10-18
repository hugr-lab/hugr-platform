# shellcheck shell=bash
# AKS, by hand (spec 004): what only Azure proves - Entra ID logging the nodes in by their ServiceAccount tokens through
# managed identities. Nothing of Azure is kept between runs (the owner, 2026-10-18): a run makes its resource group
# (the cluster, its registry, the identities) and its Entra apps, and deletes them at its end, a failed check
# included. up / check / down alone are for debugging; leftovers lists what a run left (a killed shell). Each resource
# carries the tags purpose=hugr-platform-demo and expires=<the run's day + 1>.
#   HUGR_AKS_RG (hugr-platform-demo), HUGR_AKS_LOCATION (westeurope), HUGR_AKS_VM (Standard_D4s_v6: a family with
#   quota), HUGR_AKS_NODES (1). The signed-in az account makes app registrations and role assignments.
AKS_RG="${HUGR_AKS_RG:-hugr-platform-demo}"
AKS_LOCATION="${HUGR_AKS_LOCATION:-westeurope}"
AKS_CLUSTER=hugr-demo
DEMO_CONTEXT=hugr-demo-aks
# the cluster's credentials in the run's state: the developer's kubeconfig is never touched
export KUBECONFIG="$WORK/kubeconfig"
ARCH=amd64
PULL=Always
ACR=""

provider_need() {
	need az
	az account show -o none 2>/dev/null || fail "az is not signed in (az login)"
	# the run's registry: a name of its own per subscription and group
	ACR="hugrdemo$(printf '%s' "$(az account show --query id -o tsv)$AKS_RG" | openssl dgst -sha256 -r | cut -c1-12)"
	REGISTRY="$ACR.azurecr.io/"
}

aks_tags() { echo "purpose=hugr-platform-demo expires=$(days_from_now 1 date)"; }

provider_up() {
	step "AKS: the group $AKS_RG, the registry $ACR, the cluster $AKS_CLUSTER (several minutes)"
	# shellcheck disable=SC2046 # the tags are words
	az group create -n "$AKS_RG" -l "$AKS_LOCATION" --tags $(aks_tags) -o none || fail "the resource group $AKS_RG"
	if ! az acr show -n "$ACR" -o none 2>/dev/null; then
		# shellcheck disable=SC2046
		az acr create -g "$AKS_RG" -n "$ACR" --sku Basic --tags $(aks_tags) -o none || fail "the registry $ACR"
	fi
	if ! az aks show -g "$AKS_RG" -n "$AKS_CLUSTER" -o none 2>/dev/null; then
		# shellcheck disable=SC2046
		az aks create -g "$AKS_RG" -n "$AKS_CLUSTER" --tier free --node-count "${HUGR_AKS_NODES:-1}" \
			--node-vm-size "${HUGR_AKS_VM:-Standard_D4s_v6}" --enable-oidc-issuer --attach-acr "$ACR" --no-ssh-key \
			--tags $(aks_tags) -o none || fail "AKS did not make the cluster"
	fi
	az aks get-credentials -g "$AKS_RG" -n "$AKS_CLUSTER" --file "$KUBECONFIG" --context "$DEMO_CONTEXT" \
		--overwrite-existing -o none || fail "the cluster's credentials"
	chmod 600 "$KUBECONFIG"
	# the push's token lives in the run's state, not the developer's keychain, and dies with the registry
	mkdir -p "$WORK/docker"
	az acr login -n "$ACR" --expose-token --query accessToken -o tsv 2>/dev/null |
		docker --config "$WORK/docker" login "$ACR.azurecr.io" -u 00000000-0000-0000-0000-000000000000 \
			--password-stdin >/dev/null 2>&1 || fail "a push token for $ACR"
	ok "AKS $AKS_CLUSTER runs (amd64), its images pulled from $ACR"
}

# provider_build <name> <context>: built here for amd64 (the Dockerfiles cross-compile on the build's own platform:
# BuildKit's platform arguments) and pushed to the run's registry
provider_build() {
	docker build -q --platform linux/amd64 -t "$(img "$1")" --build-arg VERSION=demo "$2" >"$WORK/build-$1.log" 2>&1 || {
		tail -30 "$WORK/build-$1.log" >&2
		fail "the image $1"
	}
	docker --config "$WORK/docker" push -q "$(img "$1")" >/dev/null || fail "pushing $1 to $ACR"
}
provider_import() { az acr import -n "$ACR" --source "$1" -t "$2:demo" --force -o none || fail "importing $1 into $ACR"; }
provider_load() { :; }

provider_down() {
	step "Azure: deleting the run - its Entra apps, the group $AKS_RG (the cluster, the registry, the identities)"
	idp_down
	if [ "$(az group exists -n "$AKS_RG")" = true ]; then
		az group delete -n "$AKS_RG" --yes -o none || fail "deleting $AKS_RG: test/demo/run.sh aks leftovers"
	fi
	rm -rf "$WORK"
	ok "deleted (AKS may have made NetworkWatcherRG in $AKS_LOCATION: Azure's, free, shared - left alone)"
}

provider_leftovers() {
	step "Azure: what runs of the demo left"
	echo "resource groups:"
	az group list --tag purpose=hugr-platform-demo --query '[].{name:name, location:location, expires:tags.expires}' -o table
	idp_leftovers
}
