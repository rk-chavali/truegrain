{{/*
Shared naming, labels and the argument list.

The arguments live here rather than in each Deployment because the REST
server and the wire protocol take the same warehouse, governance and audit
flags, and two copies of that list would drift: one would gain a policy file
the other did not, and the BI surface would quietly enforce less than the
API. One builder, two callers.
*/}}

{{- define "truegrain.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "truegrain.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "truegrain.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "truegrain.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "truegrain.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "truegrain.selectorLabels" -}}
app.kubernetes.io/name: {{ include "truegrain.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "truegrain.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "truegrain.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "truegrain.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{/*
truegrain.commonArgs renders every flag both surfaces share.

Rendered as a YAML list fragment, to be included inside an `args:` block.
*/}}
{{- define "truegrain.commonArgs" -}}
- -models={{ .Values.model.mountPath }}
- -dialect={{ .Values.warehouse.dialect }}
{{- if eq .Values.warehouse.dialect "bigquery" }}
{{- with .Values.warehouse.bigquery.project }}
- -project={{ . }}
{{- end }}
{{- with .Values.warehouse.bigquery.location }}
- -location={{ . }}
{{- end }}
{{- if .Values.warehouse.bigquery.maxBytesBilled }}
- -max-bytes-billed={{ .Values.warehouse.bigquery.maxBytesBilled }}
{{- end }}
{{- if .Values.warehouse.bigquery.impersonate }}
- -impersonate
{{- end }}
{{- end }}
{{- if eq .Values.warehouse.dialect "postgres" }}
{{- if .Values.warehouse.postgres.existingSecret }}
- -dsn-env=TRUEGRAIN_DSN
{{- end }}
{{- end }}
{{- if .Values.maxConcurrentQueries }}
- -max-concurrent-queries={{ .Values.maxConcurrentQueries }}
{{- end }}
{{- with .Values.governance.policyPath }}
- -policy={{ $.Values.model.mountPath }}/{{ . }}
{{- end }}
{{- if .Values.governance.policyTags }}
- -policy-tags
{{- end }}
{{- with .Values.audit.filePath }}
- -audit={{ . }}
{{- end }}
{{- with .Values.telemetry.otlpEndpoint }}
- -otlp-endpoint={{ . }}
{{- end }}
{{- end -}}

{{/*
truegrain.authArgs renders the authentication flags.

Shared for the same reason: an authentication mode configured on one surface
and not the other is the kind of gap nobody notices until it is found from
the outside.
*/}}
{{- define "truegrain.authArgs" -}}
{{- if eq .Values.auth.mode "none" }}
{{- fail "auth.mode is none, and this chart cannot render a Deployment that would start. A Service fronts the engine, so it binds 0.0.0.0, and the engine refuses to serve every interface unauthenticated rather than come up quietly open. Pick oidc, google or token." }}
{{- end }}
{{- if not (has .Values.auth.mode (list "oidc" "google" "token")) }}
{{- fail (printf "auth.mode is %q; it must be one of oidc, google or token" .Values.auth.mode) }}
{{- end }}
{{- if eq .Values.auth.mode "oidc" }}
- -oidc-issuer={{ required "auth.oidc.issuer is required when auth.mode is oidc" .Values.auth.oidc.issuer }}
- -oidc-audience={{ required "auth.oidc.audience is required: without it every token this issuer ever minted is accepted here" .Values.auth.oidc.audience }}
{{- with .Values.auth.oidc.subjectClaim }}
- -subject-claim={{ . }}
{{- end }}
{{- with .Values.auth.oidc.groupsClaim }}
- -groups-claim={{ . }}
{{- end }}
{{- else if eq .Values.auth.mode "google" }}
- -google-audience={{ required "auth.google.audience is required when auth.mode is google" .Values.auth.google.audience }}
{{- else if eq .Values.auth.mode "token" }}
{{- if not .Values.auth.token.existingSecret }}
{{- fail "auth.mode is token but auth.token.existingSecret names no Secret, so TRUEGRAIN_TOKEN would be empty and the engine would refuse to start. Create the Secret and name it here; the chart never takes a literal token, because a value passed with --set is in shell history and one in a values file is in git." }}
{{- end }}
- -token-env=TRUEGRAIN_TOKEN
- -identity={{ required "auth.token.identity is required with auth.mode token: the engine refuses a shared token with nobody behind it, because every audited decision would record an empty subject" .Values.auth.token.identity }}
{{- with .Values.auth.token.groups }}
- -groups={{ join "," . }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
truegrain.envFromSecrets projects credentials in from Secrets you created.

Never a literal. A value passed with --set is in shell history and a value
in a committed values file is in git; a secretKeyRef is neither.
*/}}
{{- define "truegrain.envFromSecrets" -}}
{{- if and (eq .Values.warehouse.dialect "postgres") .Values.warehouse.postgres.existingSecret }}
- name: TRUEGRAIN_DSN
  valueFrom:
    secretKeyRef:
      name: {{ .Values.warehouse.postgres.existingSecret }}
      key: {{ .Values.warehouse.postgres.secretKey }}
{{- end }}
{{- if and (eq .Values.auth.mode "token") .Values.auth.token.existingSecret }}
- name: TRUEGRAIN_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ .Values.auth.token.existingSecret }}
      key: {{ .Values.auth.token.secretKey }}
{{- end }}
{{- end -}}

{{/*
truegrain.modelVolume resolves where the model comes from.
*/}}
{{- define "truegrain.modelVolume" -}}
- name: model
{{- if .Values.model.volume }}
  {{- toYaml .Values.model.volume | nindent 2 }}
{{- else if .Values.model.existingConfigMap }}
  configMap:
    name: {{ .Values.model.existingConfigMap }}
{{- else }}
  configMap:
    name: {{ include "truegrain.fullname" . }}-model
{{- end }}
{{- end -}}
