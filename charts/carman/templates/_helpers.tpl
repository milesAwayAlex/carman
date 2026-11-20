{{- /* What every pod of this chart carries, whatever it is for. */ -}}

{{- /* Labels the installation asks for, beside a pod's own. */ -}}
{{- define "pod.labels" -}}
{{- with .Values.podLabels }}{{ toYaml . }}{{- end }}
{{- end -}}

{{- /* Which nodes the pods may run on. */ -}}
{{- define "pod.placement" -}}
{{- with .Values.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{- /* A pod's own account, which is nothing to the cluster's API. */ -}}
{{- define "pod.account" -}}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ .name }}
  namespace: {{ .root.Release.Namespace }}
  {{- with .annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
automountServiceAccountToken: false
{{- end -}}
