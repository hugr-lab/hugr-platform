{{/* The umbrella's own objects (spec 005): labels, and the services' namespaces. */}}

{{- define "hugr.umbrella.labels" -}}
app.kubernetes.io/part-of: hugr
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/* the services the umbrella makes a namespace, a CA and a database for: name -> its namespace */}}
{{- define "hugr.umbrella.services" -}}
tresor: {{ .Values.global.namespaces.tresor }}
kista: {{ .Values.global.namespaces.kista }}
{{- end -}}

{{/* a service's address in the cluster: tresor's fullname is tresor, kista's <release>-kista */}}
{{- define "hugr.umbrella.serviceHost" -}}
{{- $ns := index .root.Values.global.namespaces .service -}}
{{- if eq .service "kista" -}}
{{- printf "%s-kista.%s.svc" .root.Release.Name $ns -}}
{{- else -}}
{{- printf "%s.%s.svc" .service $ns -}}
{{- end -}}
{{- end -}}
