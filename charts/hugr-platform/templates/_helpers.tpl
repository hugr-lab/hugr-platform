{{/* The names of the platform's objects (spec 005): the namespaced ones as config/ has them, the cluster-scoped ones
and those in cert-manager's namespace after the release's namespace - one platform per namespace. */}}

{{- define "hugr.clusterName" -}}
{{- printf "%s-hugr-platform" .Release.Namespace -}}
{{- end -}}

{{- define "hugr.caName" -}}
{{- printf "hugr-%s-internal-ca" .Release.Namespace -}}
{{- end -}}

{{- define "hugr.issuerName" -}}
{{- printf "hugr-%s-internal" .Release.Namespace -}}
{{- end -}}

{{- define "hugr.bundleName" -}}
{{- printf "hugr-%s-ca" .Release.Namespace -}}
{{- end -}}

{{- define "hugr.caSecret" -}}
{{- default (include "hugr.caName" .) .Values.certManager.internalCA.existingSecret -}}
{{- end -}}

{{- define "hugr.labels" -}}
app.kubernetes.io/part-of: hugr-platform
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "hugr.operatorImage" -}}
{{- printf "%s:%s" .Values.operator.image.repository (default .Chart.AppVersion .Values.operator.image.tag) -}}
{{- end -}}

{{/* the internal issuer, as a cert-manager issuerRef */}}
{{- define "hugr.internalIssuerRef" -}}
name: {{ include "hugr.issuerName" . }}
kind: ClusterIssuer
group: cert-manager.io
{{- end -}}
