{{/* The names of the platform's objects (spec 005): the namespaced ones as config/ has them, the cluster-scoped ones
after the release's namespace - one platform per namespace. The internal CA and its Issuer are the namespace's own:
only a pod or a Certificate of this namespace is issued from them (spec 001's boundary). */}}

{{- define "hugr.clusterName" -}}
{{- printf "%s-hugr-platform" .Release.Namespace -}}
{{- end -}}

{{- define "hugr.caName" -}}
hugr-internal-ca
{{- end -}}

{{- define "hugr.issuerName" -}}
hugr-internal
{{- end -}}

{{- define "hugr.bundleName" -}}
hugr-ca-bundle
{{- end -}}

{{- define "hugr.caSecret" -}}
{{- default (include "hugr.caName" .) .Values.internalCA.existingSecret -}}
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
kind: Issuer
group: cert-manager.io
{{- end -}}
