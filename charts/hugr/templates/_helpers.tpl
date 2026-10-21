{{/* The umbrella's own objects (spec 005): labels, and the services' namespaces. */}}

{{- define "hugr.umbrella.labels" -}}
app.kubernetes.io/part-of: hugr
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/* the services the umbrella makes a namespace, a CA and a database for: name -> its namespace; the internal key
store's OpenBao among them with keys.mode internal */}}
{{- define "hugr.umbrella.services" -}}
tresor: {{ .Values.global.namespaces.tresor }}
kista: {{ .Values.global.namespaces.kista }}
{{- if eq .Values.global.keys.mode "internal" }}
openbao: {{ .Values.global.namespaces.openbao }}
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

{{/* a service's address in the cluster: tresor's fullname is tresor, kista's <release>-kista, OpenBao's openbao */}}
{{- define "hugr.umbrella.serviceHost" -}}
{{- $ns := index .root.Values.global.namespaces .service -}}
{{- if eq .service "kista" -}}
{{- printf "%s.%s.svc" (include "hugr.umbrella.kistaFullname" .root) $ns -}}
{{- else -}}
{{- printf "%s.%s.svc" .service $ns -}}
{{- end -}}
{{- end -}}

{{/* the internal OpenBao's configuration (openbao.yaml): its ConfigMap and the pods' checksum */}}
{{- define "hugr.umbrella.openbaoConfig" -}}
ui = false
disable_mlock = true
disable_standby_reads = true
listener "tcp" {
  address         = "0.0.0.0:8200"
  cluster_address = "0.0.0.0:8201"
  tls_cert_file   = "/openbao/tls/tls.crt"
  tls_key_file    = "/openbao/tls/tls.key"
  # cert-manager renews the certificate in place: read again when it changes, no restart
  tls_auto_reload = true
}
# the break-glass, on the pod's own loopback only (kubectl exec): a root token from the recovery key
# (openbao-recovery) by the legacy, unauthenticated generation
listener "tcp" {
  address     = "127.0.0.1:8210"
  tls_disable = true
  disable_unauthed_generate_root_endpoints = false
}
# the connection from BAO_PG_CONNECTION_URL, the password from PGPASSWORD
storage "postgresql" {
  ha_enabled = "true"
}
seal "static" {
  current_key_id = "hugr-1"
  current_key    = "file:///openbao/seal/key"
}
{{- end -}}
