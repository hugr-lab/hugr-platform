{{/* The umbrella's own objects (spec 005): labels, and the own parts' namespaces. */}}

{{- define "hugr.umbrella.labels" -}}
app.kubernetes.io/part-of: hugr
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/* the parts the umbrella runs itself (spec 005 step 3), each in its namespace with a CA of its own: name -> its
namespace */}}
{{- define "hugr.umbrella.services" -}}
{{- if .Values.install.tresor }}
tresor: {{ .Values.global.namespaces.tresor }}
{{- end }}
{{- if .Values.install.kista }}
kista: {{ .Values.global.namespaces.kista }}
{{- end }}
{{- if .Values.install.catalogDatabase }}
database: {{ .Values.global.namespaces.database }}
{{- end }}
{{- end -}}

{{/* kista's fullname, as its chart makes it: its Service and its ServiceAccount */}}
{{- define "hugr.umbrella.kistaFullname" -}}
{{- if contains "kista" .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-kista" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* an own part's address in the cluster: tresor's fullname is tresor, kista's <release>-kista, the database's postgres */}}
{{- define "hugr.umbrella.serviceHost" -}}
{{- $ns := index .root.Values.global.namespaces .service -}}
{{- if eq .service "kista" -}}
{{- printf "%s.%s.svc" (include "hugr.umbrella.kistaFullname" .root) $ns -}}
{{- else if eq .service "database" -}}
{{- printf "postgres.%s.svc" $ns -}}
{{- else -}}
{{- printf "%s.%s.svc" .service $ns -}}
{{- end -}}
{{- end -}}

{{/* the operator's image (hugr-init is in it), as the platform's chart names it: its tag, or the release's version */}}
{{- define "hugr.umbrella.operatorImage" -}}
{{- $img := (index .Values "hugr-platform").operator.image -}}
{{- printf "%s:%s" $img.repository (default .Chart.AppVersion $img.tag) -}}
{{- end -}}
