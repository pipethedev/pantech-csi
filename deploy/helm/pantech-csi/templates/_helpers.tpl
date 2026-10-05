{{- define "pantech-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pantech-csi.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "pantech-csi.name" . -}}
{{- end -}}
{{- end -}}

{{- define "pantech-csi.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end -}}

{{- define "pantech-csi.labels" -}}
app.kubernetes.io/name: {{ include "pantech-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "pantech-csi.controllerServiceAccountName" -}}
{{- if .Values.serviceAccount.controller.create -}}
{{- default (printf "%s-controller" (include "pantech-csi.fullname" .)) .Values.serviceAccount.controller.name -}}
{{- else -}}
{{- required "serviceAccount.controller.name is required when serviceAccount.controller.create=false" .Values.serviceAccount.controller.name -}}
{{- end -}}
{{- end -}}

{{- define "pantech-csi.nodeServiceAccountName" -}}
{{- if .Values.serviceAccount.node.create -}}
{{- default (printf "%s-node" (include "pantech-csi.fullname" .)) .Values.serviceAccount.node.name -}}
{{- else -}}
{{- required "serviceAccount.node.name is required when serviceAccount.node.create=false" .Values.serviceAccount.node.name -}}
{{- end -}}
{{- end -}}

{{- define "pantech-csi.secretName" -}}
{{- if .Values.pantech.existingSecret -}}
{{- .Values.pantech.existingSecret -}}
{{- else -}}
{{- default (include "pantech-csi.fullname" .) .Values.pantech.secretName -}}
{{- end -}}
{{- end -}}

{{- define "pantech-csi.controllerImage" -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.controllerTag -}}
{{- end -}}

{{- define "pantech-csi.nodeImage" -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.nodeTag -}}
{{- end -}}
