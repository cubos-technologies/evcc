{{ define "mqtt" }}
broker: {{ .host }}:{{ .port }}
{{- if .user }}
user: {{ .user }}
{{- end }}
{{- if .password }}
password: {{ .password }}
{{- end }}
{{- if ne .timeout "30s" }}
timeout: {{ .timeout }}
{{- end }}
{{- if ne .ca_cert "" }}
ca_cert: {{ .ca_cert }}
{{- end }}
{{- if ne .client_cert "" }}
client_cert: {{ .client_cert }}
{{- end }}
{{- if ne .client_key "" }}
client_key: {{ .client_key }}
{{- end }}
{{- end }}
