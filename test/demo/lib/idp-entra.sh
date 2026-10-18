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

# entra_app <name> [app role...]: the run's app registration $ENTRA_PREFIX-<run>-<name> with its service principal.
# Made once and its ids recorded (app-<name>, object-<name>, sp-<name>): a run never takes an app it did not make.
entra_app() {
	local name="$1" app object roles
	shift
	app="$(entra_get "app-$name")"
	if [ -z "$app" ]; then
		roles="$(python3 -c 'import json, sys, uuid
print(json.dumps([{"allowedMemberTypes": ["Application"], "description": r, "displayName": r, "isEnabled": True,
                   "value": r, "id": str(uuid.uuid4())} for r in sys.argv[1:]]))' "$@")"
		app="$(az ad app create --display-name "$ENTRA_PREFIX-$RUN_ID-$name" --sign-in-audience AzureADMyOrg \
			--app-roles "$roles" --query appId -o tsv)" || fail "the app registration $ENTRA_PREFIX-$RUN_ID-$name"
		entra_put "app-$name" "$app"
	fi
	app_object() { object="$(az ad app show --id "$app" --query id -o tsv)" && [ -n "$object" ]; }
	eventually 120 "the app $name in Graph" app_object
	entra_put "object-$name" "$object"
	# its tokens are v2 (the issuer .../v2.0, the audience the app id), its identifier api://<app id>
	patched() {
		graph PATCH "applications/$object" --headers Content-Type=application/json --body "{
			\"identifierUris\": [\"api://$app\"], \"api\": {\"requestedAccessTokenVersion\": 2},
			\"tags\": [\"purpose=$ENTRA_PREFIX\", \"run=$RUN_ID\", \"expires=$(days_from_now 1 date)\"]}" -o none
	}
	eventually 120 "the app $name's identifier and token version" patched
	sp() { [ -n "$(az ad sp show --id "$app" --query id -o tsv 2>/dev/null)" ] || az ad sp create --id "$app" -o none; }
	eventually 120 "the service principal of $name" sp
	entra_put "sp-$name" "$(az ad sp show --id "$app" --query id -o tsv)"
}

# entra_resource <name>: only the principals assigned one of the app's roles get a token for it
entra_resource() {
	required() { az ad sp update --id "$(entra_get "app-$1")" --set appRoleAssignmentRequired=true -o none; }
	eventually 120 "the app $1 requires an assignment" required "$1"
}

# entra_assign <principal> <app name> <app role>: the principal (a service principal's object id, or the command that
# prints it) holds the app's role; retried, a principal made a moment ago reaches Graph in a while
entra_assign() {
	local who="$1" resource role principal
	resource="$(entra_get "sp-$2")"
	role="$(az ad sp show --id "$(entra_get "app-$2")" --query "appRoles[?value=='$3'].id | [0]" -o tsv)"
	[ -n "$resource" ] && [ -n "$role" ] || fail "the app role $3 of $ENTRA_PREFIX-$RUN_ID-$2"
	assign() {
		principal="$($who)" && [ -n "$principal" ] || return 1
		[ -n "$(graph GET "servicePrincipals/$resource/appRoleAssignedTo" \
			--query "value[?principalId=='$principal' && appRoleId=='$role'] | [0].id" -o tsv)" ] && return 0
		graph POST "servicePrincipals/$resource/appRoleAssignedTo" --headers Content-Type=application/json \
			--body "{\"principalId\": \"$principal\", \"resourceId\": \"$resource\", \"appRoleId\": \"$role\"}" -o none
	}
	eventually 180 "the role $3 of $2 assigned" assign
}
identity_principal() { azs identity show -g "$AKS_RG" -n "hugr-node-$1" --query principalId -o tsv; }
principal_sp() { entra_get "sp-$1"; }

# entra_principal <p>: a principal's app registration and its client secret (two days), written to the run's state
entra_principal() {
	local file="$WORK/secrets/principal-$1"
	entra_app "$1"
	if [ ! -s "$file" ]; then
		mkdir -p "$WORK/secrets" && chmod 700 "$WORK/secrets"
		(umask 077 && az ad app credential reset --id "$(entra_get "app-$1")" --append --display-name demo \
			--end-date "$(days_from_now 2)" --query password -o tsv 2>/dev/null | tr -d '\n' >"$file") ||
			fail "the client secret of $ENTRA_PREFIX-$RUN_ID-$1"
		[ -s "$file" ] || fail "the client secret of $ENTRA_PREFIX-$RUN_ID-$1"
	fi
	mask "$file"
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
	oidc="$(azs aks show -g "$AKS_RG" -n "$AKS_CLUSTER" --query oidcIssuerProfile.issuerUrl -o tsv)"
	[ -n "$oidc" ] || fail "the cluster has no OIDC issuer"
	entra_app tresor hugr_node demo_seeder
	entra_app hugr analyst_a analyst_b platform_admin node_breakglass
	entra_resource tresor
	entra_resource hugr
	for g in "${GROUPS_[@]}"; do
		if ! azs identity show -g "$AKS_RG" -n "hugr-node-$g" -o none 2>/dev/null; then
			# shellcheck disable=SC2046 # the tags are words
			azs identity create -g "$AKS_RG" -n "hugr-node-$g" --tags $(aks_tags) -o none ||
				fail "the managed identity hugr-node-$g"
		fi
		entra_put "identity-$g" "$(azs identity show -g "$AKS_RG" -n "hugr-node-$g" --query clientId -o tsv)"
		azs identity federated-credential show -g "$AKS_RG" --identity-name "hugr-node-$g" -n sa -o none 2>/dev/null ||
			azs identity federated-credential create -g "$AKS_RG" --identity-name "hugr-node-$g" -n sa -o none \
				--issuer "$oidc" --subject "system:serviceaccount:hugr:hugr-node-$g" \
				--audiences api://AzureADTokenExchange || fail "the federated credential of hugr-node-$g"
		entra_assign "identity_principal $g" tresor hugr_node
	done
	for principal in seeder analyst-a analyst-b admin breakglass; do
		entra_principal "$principal"
	done
	entra_assign "principal_sp seeder" tresor demo_seeder
	entra_assign "principal_sp analyst-a" hugr analyst_a
	entra_assign "principal_sp analyst-b" hugr analyst_b
	entra_assign "principal_sp admin" hugr platform_admin
	entra_assign "principal_sp breakglass" hugr node_breakglass
	entra_vars
	# an assignment reaches the tokens in a while: every principal's is waited for
	eventually 300 "the seeder's token for tresor carries demo_seeder" entra_token_has seeder "$TRESOR_SCOPE" demo_seeder
	eventually 300 "analyst_a's token carries analyst_a" entra_token_has analyst-a "api://$HUGR_AUDIENCE/.default" analyst_a
	eventually 300 "analyst_b's token carries analyst_b" entra_token_has analyst-b "api://$HUGR_AUDIENCE/.default" analyst_b
	eventually 300 "the admin's token carries platform_admin" entra_token_has admin "api://$HUGR_AUDIENCE/.default" platform_admin
	eventually 300 "the break-glass token carries node_breakglass" \
		entra_token_has breakglass "api://$HUGR_AUDIENCE/.default" node_breakglass
	ok "Entra: hugr-node-a and -b federated from the cluster's issuer, the apps tresor and hugr (assignment required), the principals"
}

# idp_down: the apps this run recorded - deleted, then purged from the recycle bin with their service principals,
# each until it is gone (the identities go with the resource group). Nothing found by a name is touched.
idp_down() {
	local f name object sp
	for f in "$WORK"/entra/object-*; do
		[ -f "$f" ] || continue
		name="${f##*/object-}"
		object="$(cat "$f")"
		sp="$(entra_get "sp-$name")"
		app_gone() { ! graph GET "applications/$object" -o none 2>/dev/null || graph DELETE "applications/$object" -o none; }
		eventually 120 "the app $name deleted" app_gone
		# a deleted app reaches the recycle bin in a while: it is waited for there, then purged (a 404 right after the
		# delete is a bin not caught up, not a purge)
		purged() {
			[ -f "$WORK/entra/purged-$object" ] && return 0
			graph GET "directory/deletedItems/$object" -o none 2>/dev/null || return 1
			graph DELETE "directory/deletedItems/$object" -o none 2>/dev/null || return 1
			: >"$WORK/entra/purged-$object"
		}
		eventually 300 "the app $name purged from the recycle bin" purged
		# its service principal, if the bin holds it (leftovers lists one that arrives later)
		if [ -n "$sp" ] && graph GET "directory/deletedItems/$sp" -o none 2>/dev/null; then
			graph DELETE "directory/deletedItems/$sp" -o none 2>/dev/null || true
		fi
		rm -f "$f"
	done
	rm -rf "$WORK/entra"
}

# idp_leftovers: every run's apps, live or in the recycle bin, by the name prefix
idp_leftovers() {
	local kind
	echo "Entra app registrations:"
	az ad app list --filter "startswith(displayName,'$ENTRA_PREFIX-')" --query '[].{name:displayName, appId:appId}' -o table
	for kind in application servicePrincipal; do
		echo "deleted ${kind}s, still in the recycle bin:"
		graph GET "directory/deletedItems/microsoft.graph.$kind?\$filter=startswith(displayName,'$ENTRA_PREFIX-')&\$count=true" \
			--headers ConsistencyLevel=eventual --query 'value[].{name:displayName, deleted:deletedDateTime}' -o table
	done
}
