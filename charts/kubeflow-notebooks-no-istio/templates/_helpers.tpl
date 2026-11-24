{{/* Chart name. */}}
{{- define "kni.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified release name. */}}
{{- define "kni.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Name of a component: <fullname>-<component>. */}}
{{- define "kni.componentName" -}}
{{- printf "%s-%s" (include "kni.fullname" .root) .component | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kni.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/part-of: {{ include "kni.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* Selector labels of a component; call with (dict "root" $ "component" "x"). */}}
{{- define "kni.selectorLabels" -}}
app.kubernetes.io/name: {{ .component }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end }}

{{- define "kni.componentLabels" -}}
{{ include "kni.labels" .root }}
app.kubernetes.io/name: {{ .component }}
{{- end }}

{{- define "kni.gatewayNamespace" -}}
{{- default .Release.Namespace .Values.gateway.namespace }}
{{- end }}

{{/* parentRef/targetRef of the gateway listener. */}}
{{- define "kni.gatewayRef" -}}
group: gateway.networking.k8s.io
kind: Gateway
name: {{ .Values.gateway.name }}
{{- with .Values.gateway.sectionName }}
sectionName: {{ . }}
{{- end }}
{{- end }}

{{- define "kni.oidcSecretName" -}}
{{- if .Values.auth.oidc.clientSecret.existingSecret }}
{{- .Values.auth.oidc.clientSecret.existingSecret }}
{{- else }}
{{- include "kni.componentName" (dict "root" . "component" "oidc-client") }}
{{- end }}
{{- end }}

{{- define "kni.redirectURL" -}}
{{- printf "https://%s/oauth2/callback" .Values.gateway.hostname }}
{{- end }}

{{- define "kni.routeControllerImage" -}}
{{- $img := .Values.routeController.image }}
{{- printf "%s:%s" $img.repository (default .Chart.AppVersion $img.tag) }}
{{- end }}

{{/*
Fail early on settings without which the deployment would be insecure or
non-functional. Values that the JSON schema cannot express are checked here.
*/}}
{{- define "kni.validate" -}}
{{- $o := .Values.auth.oidc }}
{{- if not .Values.gateway.hostname }}{{ fail "gateway.hostname is required" }}{{ end }}
{{- if not $o.issuer }}{{ fail "auth.oidc.issuer is required" }}{{ end }}
{{- if not $o.clientID }}{{ fail "auth.oidc.clientID is required" }}{{ end }}
{{- if not $o.jwksURI }}{{ fail "auth.oidc.jwksURI is required" }}{{ end }}
{{- if and (not $o.clientSecret.existingSecret) (not $o.clientSecret.value) }}
{{- fail "set auth.oidc.clientSecret.existingSecret or auth.oidc.clientSecret.value" }}
{{- end }}
{{- if and $o.clientSecret.existingSecret $o.clientSecret.value }}
{{- fail "set only one of auth.oidc.clientSecret.existingSecret and auth.oidc.clientSecret.value" }}
{{- end }}
{{- $cs := .Values.oauth2Proxy.cookieSecret }}
{{- if and $cs.existingSecret $cs.value }}
{{- fail "set only one of oauth2Proxy.cookieSecret.existingSecret and oauth2Proxy.cookieSecret.value" }}
{{- end }}
{{- if and $cs.value (not (has (len $cs.value) (list 16 24 32))) }}
{{- fail "oauth2Proxy.cookieSecret.value must be exactly 16, 24 or 32 bytes" }}
{{- end }}
{{- $ss := .Values.oauth2Proxy.sessionStore }}
{{- $h := .Values.oauth2Proxy.headersToExtAuth }}
{{- if or (not $h) (not (has "cookie" $h)) }}
{{- fail "oauth2Proxy.headersToExtAuth must be non-empty and contain \"cookie\": without it oauth2-proxy never receives the session cookie and every request redirects to login" }}
{{- end }}
{{- if not (has $ss.type (list "cookie" "redis")) }}
{{- fail "oauth2Proxy.sessionStore.type must be one of: cookie, redis" }}
{{- end }}
{{- if and (eq $ss.type "redis") (not $ss.redis.create) (not $ss.redis.connectionURL) }}
{{- fail "oauth2Proxy.sessionStore.type=redis with redis.create=false requires oauth2Proxy.sessionStore.redis.connectionURL" }}
{{- end }}
{{- if and .Values.networkPolicy.enabled (not (or .Values.networkPolicy.gatewayPeers .Values.networkPolicy.extraPeers)) }}
{{- fail "networkPolicy.enabled requires networkPolicy.gatewayPeers" }}
{{- end }}
{{- end }}

{{/*
Ext_authz backend: oauth2-proxy answers the Envoy ext_authz check. Without a
session it redirects the user to the IdP (the 302 reaches the client); after
login it returns the ID token in the Authorization header, which
headersToBackend passes to the backend of the matched route.
Rendered once as JSON and used by the per-route SecurityPolicies.
*/}}
{{- define "kni.extAuth" -}}
{{- $http := dict
    "backendRefs" (list (dict
      "name" (include "kni.componentName" (dict "root" . "component" "oauth2-proxy"))
      "namespace" .Release.Namespace
      "port" 4180))
    "headersToBackend" (list "authorization") }}
{{- dict "http" $http "headersToExtAuth" .Values.oauth2Proxy.headersToExtAuth | toJson }}
{{- end }}

{{/*
JWT provider validating the ID token returned by oauth2-proxy. Rendered once
as JSON and used by the per-route SecurityPolicies and by the
notebook-route-controller, which repeats it in every per-notebook policy.
*/}}
{{- define "kni.jwtProvider" -}}
{{- $o := .Values.auth.oidc }}
{{- $j := .Values.auth.jwt }}
{{- dict
    "name" $j.providerName
    "issuer" $o.issuer
    "audiences" (default (list $o.clientID) $j.audiences)
    "remoteJWKS" (dict "uri" $o.jwksURI)
    "claimToHeaders" (list (dict "header" .Values.auth.userIdHeader "claim" $j.userClaim))
  | toJson }}
{{- end }}
