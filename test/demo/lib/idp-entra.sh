# shellcheck shell=bash
# The IdP on AKS (spec 004): Entra ID, as the Azure Managed Application will.
#   - a user-assigned managed identity per group, federated from the group's ServiceAccount by the cluster's OIDC
#     issuer: a node logs in to tresor with no secret anywhere;
#   - tresor's app and the node's app (hugr), each with the app roles its callers hold, on v2 tokens;
#   - the demo's principals as app registrations, their client secrets two days long and in the run's state only.
# Made by up (install_idp), deleted by down (idp_down); provider_leftovers lists what a run left. The ids are kept in
# the run's state ($WORK/entra, nothing secret there). The interface is idp-keycloak.sh's.
ENTRA_PREFIX=hugr-platform-demo
IDP_NAME=Entra
IDP_FEDERATION_AUDIENCE="" # the platform's default, api://AzureADTokenExchange
TRESOR_ROLES_CLAIM=roles
TRESOR_GROUPS_CLAIM=""
TRESOR_SERVICE_CLAIM=azp
NODES_PRINCIPAL=role:hugr_node
HUGR_ROLES_CLAIM=roles

entra_get() { cat "$WORK/entra/$1" 2>/dev/null || true; }
entra_put() { mkdir -p "$WORK/entra" && echo "$2" >"$WORK/entra/$1"; }
entra_vars() {
	IDP_ISSUER="https://login.microsoftonline.com/$(entra_get tenant)/v2.0"
	TRESOR_AUDIENCE="$(entra_get app-tresor)"
	TRESOR_SCOPE="api://$TRESOR_AUDIENCE/.default"
	HUGR_AUDIENCE="$(entra_get app-hugr)"
}
entra_vars

group_client_id() { entra_get "identity-$1"; }
principal_id() { entra_get "app-$1"; }
idp_checker_up() { :; }
token_flags() {
	echo "-token-url https://login.microsoftonline.com/$(entra_get tenant)/oauth2/v2.0/token -scope api://$HUGR_AUDIENCE/.default"
}

# days_from_now <n> [date]: an instant n days ahead (UTC), or its day
days_from_now() {
	python3 -c 'import datetime, sys
t = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=int(sys.argv[1]))
print(t.strftime("%Y-%m-%d" if sys.argv[2:] else "%Y-%m-%dT%H:%M:%SZ"))' "$@"
}

graph() { az rest --uri "https://graph.microsoft.com/v1.0/$2" --method "$1" "${@:3}"; }

# entra_app <name> [app role...]: the app registration $ENTRA_PREFIX-<name> with its service principal, made once
entra_app() {
	local name="$1" app object roles
	shift
	app="$(az ad app list --display-name "$ENTRA_PREFIX-$name" --query '[0].appId' -o tsv)"
	if [ -z "$app" ]; then
		roles="$(python3 -c 'import json, sys, uuid
print(json.dumps([{"allowedMemberTypes": ["Application"], "description": r, "displayName": r, "isEnabled": True,
                   "value": r, "id": str(uuid.uuid4())} for r in sys.argv[1:]]))' "$@")"
		app="$(az ad app create --display-name "$ENTRA_PREFIX-$name" --sign-in-audience AzureADMyOrg \
			--app-roles "$roles" --query appId -o tsv)" || fail "the app registration $ENTRA_PREFIX-$name"
		object="$(az ad app show --id "$app" --query id -o tsv)"
		# its tokens are v2 (the issuer .../v2.0, the audience the app id), its identifier api://<app id>
		graph PATCH "applications/$object" --headers Content-Type=application/json --body "{
			\"identifierUris\": [\"api://$app\"], \"api\": {\"requestedAccessTokenVersion\": 2},
			\"tags\": [\"purpose=$ENTRA_PREFIX\", \"expires=$(days_from_now 1 date)\"]}" -o none ||
			fail "the app $ENTRA_PREFIX-$name's identifier and token version"
		az ad sp create --id "$app" -o none || fail "the service principal of $ENTRA_PREFIX-$name"
	fi
	entra_put "app-$name" "$app"
}

# entra_assign <service principal id> <app name> <app role>: the principal holds the app's role (retried: a principal
# made a moment ago reaches Graph in a while)
entra_assign() {
	local principal="$1" resource role
	resource="$(az ad sp show --id "$(entra_get "app-$2")" --query id -o tsv)"
	role="$(az ad sp show --id "$(entra_get "app-$2")" --query "appRoles[?value=='$3'].id | [0]" -o tsv)"
	[ -n "$resource" ] && [ -n "$role" ] || fail "the app role $3 of $ENTRA_PREFIX-$2"
	assigned() {
		graph GET "servicePrincipals/$resource/appRoleAssignedTo" \
			--query "value[?principalId=='$principal' && appRoleId=='$role'] | [0].id" -o tsv | grep -q .
	}
	assign() {
		assigned || graph POST "servicePrincipals/$resource/appRoleAssignedTo" --headers Content-Type=application/json \
			--body "{\"principalId\": \"$principal\", \"resourceId\": \"$resource\", \"appRoleId\": \"$role\"}" -o none
	}
	eventually 120 "the role $3 of $ENTRA_PREFIX-$2 assigned" assign
}

# entra_principal <p>: a principal's app registration and its client secret (two days), written to the run's state
entra_principal() {
	local file="$WORK/secrets/principal-$1"
	entra_app "$1"
	if [ ! -s "$file" ]; then
		mkdir -p "$WORK/secrets" && chmod 700 "$WORK/secrets"
		(umask 077 && az ad app credential reset --id "$(entra_get "app-$1")" --append --display-name demo \
			--end-date "$(days_from_now 2)" --query password -o tsv >"$file" 2>/dev/null) ||
			fail "the client secret of $ENTRA_PREFIX-$1"
	fi
}

# entra_token_has <p> <scope> <role>: the principal's token for the scope is v2 and carries the role (assignments
# reach the tokens in a while). The secret goes to curl on stdin, the token is read here and never printed.
entra_token_has() {
	python3 -c 'import sys, urllib.parse
print(urllib.parse.urlencode({"grant_type": "client_credentials", "client_id": sys.argv[1],
                              "client_secret": open(sys.argv[2]).read().strip(), "scope": sys.argv[3]}))' \
		"$(principal_id "$1")" "$WORK/secrets/principal-$1" "$2" |
		curl -sf --data-binary @- -H 'Content-Type: application/x-www-form-urlencoded' \
			"https://login.microsoftonline.com/$(entra_get tenant)/oauth2/v2.0/token" |
		python3 -c 'import base64, json, sys
part = json.load(sys.stdin)["access_token"].split(".")[1]
claims = json.loads(base64.urlsafe_b64decode(part + "=" * (-len(part) % 4)))
sys.exit(0 if sys.argv[1] in claims.get("roles", []) and claims.get("iss", "").endswith("/v2.0") else 1)' "$3"
}

install_idp() {
	step "Entra ID: the groups' managed identities federated from their ServiceAccounts, tresor's and the node's apps, the principals"
	local oidc g principal
	entra_put tenant "$(az account show --query tenantId -o tsv)"
	oidc="$(az aks show -g "$AKS_RG" -n "$AKS_CLUSTER" --query oidcIssuerProfile.issuerUrl -o tsv)"
	[ -n "$oidc" ] || fail "the cluster has no OIDC issuer"
	entra_app tresor hugr_node demo_seeder
	entra_app hugr analyst_a analyst_b platform_admin
	for g in "${GROUPS_[@]}"; do
		az identity show -g "$AKS_RG" -n "hugr-node-$g" -o none 2>/dev/null ||
			az identity create -g "$AKS_RG" -n "hugr-node-$g" --tags "purpose=$ENTRA_PREFIX" -o none ||
			fail "the managed identity hugr-node-$g"
		entra_put "identity-$g" "$(az identity show -g "$AKS_RG" -n "hugr-node-$g" --query clientId -o tsv)"
		az identity federated-credential show -g "$AKS_RG" --identity-name "hugr-node-$g" -n sa -o none 2>/dev/null ||
			az identity federated-credential create -g "$AKS_RG" --identity-name "hugr-node-$g" -n sa -o none \
				--issuer "$oidc" --subject "system:serviceaccount:hugr:hugr-node-$g" \
				--audiences api://AzureADTokenExchange || fail "the federated credential of hugr-node-$g"
		entra_assign "$(az identity show -g "$AKS_RG" -n "hugr-node-$g" --query principalId -o tsv)" tresor hugr_node
	done
	for principal in seeder analyst-a analyst-b admin; do
		entra_principal "$principal"
	done
	entra_assign "$(az ad sp show --id "$(principal_id seeder)" --query id -o tsv)" tresor demo_seeder
	entra_assign "$(az ad sp show --id "$(principal_id analyst-a)" --query id -o tsv)" hugr analyst_a
	entra_assign "$(az ad sp show --id "$(principal_id analyst-b)" --query id -o tsv)" hugr analyst_b
	entra_assign "$(az ad sp show --id "$(principal_id admin)" --query id -o tsv)" hugr platform_admin
	entra_vars
	eventually 300 "the seeder's token for tresor carries demo_seeder" entra_token_has seeder "$TRESOR_SCOPE" demo_seeder
	eventually 300 "analyst_a's token for the nodes carries analyst_a" entra_token_has analyst-a "api://$HUGR_AUDIENCE/.default" analyst_a
	ok "Entra: hugr-node-a and -b federated from the cluster's issuer, the apps tresor and hugr, the principals"
}

# idp_down: the run's app registrations and their service principals, out of the recycle bin too (the identities
# go with the resource group)
idp_down() {
	local id kind
	for id in $(az ad app list --filter "startswith(displayName,'$ENTRA_PREFIX-')" --query '[].id' -o tsv); do
		az ad app delete --id "$id" -o none || true
	done
	for kind in application servicePrincipal; do
		for id in $(graph GET "directory/deletedItems/microsoft.graph.$kind?\$filter=startswith(displayName,'$ENTRA_PREFIX-')" \
			--query 'value[].id' -o tsv 2>/dev/null); do
			graph DELETE "directory/deletedItems/$id" -o none 2>/dev/null || true
		done
	done
	rm -rf "$WORK/entra"
}

idp_leftovers() {
	echo "Entra app registrations:"
	az ad app list --filter "startswith(displayName,'$ENTRA_PREFIX-')" --query '[].{name:displayName, appId:appId}' -o table
	echo "deleted, still in the recycle bin:"
	graph GET "directory/deletedItems/microsoft.graph.application?\$filter=startswith(displayName,'$ENTRA_PREFIX-')" \
		--query 'value[].{name:displayName, deleted:deletedDateTime}' -o table
}
