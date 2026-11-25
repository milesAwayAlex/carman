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

{{- /* Where warm images are kept (ADR 0041): agent.warm.repository, or
agent.image's own repository. */ -}}
{{- define "warm.repository" -}}
{{- $image := required "agent.image is required" .Values.agent.image }}
{{- $repository := .Values.agent.warm.repository | default (regexReplaceAll ":[^:/]+$" (regexReplaceAll "@.*$" $image "") "") }}
{{- $host := index (splitList "/" $repository) 0 }}
{{- if not (or (contains "." $host) (contains ":" $host) (eq $host "localhost")) }}
{{- fail (printf "agent.warm needs a registry: %s names none (set agent.warm.repository)" $repository) }}
{{- end }}
{{- $repository }}
{{- end -}}

{{- /* The image an agent's job starts from, and when it is pulled: the warm
image, asked for at each start since its tag moves, or agent.image. */ -}}
{{- define "agent.jobImage" -}}
{{- if .Values.agent.warm.enabled -}}
image: {{ include "warm.repository" . }}:{{ .Values.agent.warm.tag }}
imagePullPolicy: Always
{{- else -}}
image: {{ required "agent.image is required" .Values.agent.image }}
imagePullPolicy: IfNotPresent
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

{{- /* Whom a pod may ask for names: the cluster's name service, and the
addresses a node-local cache answers on. */ -}}
{{- define "dns.peers" -}}
- namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: kube-system
  podSelector:
    matchLabels:
      k8s-app: kube-dns
{{- range .Values.egress.nameServers }}
- ipBlock: {cidr: {{ . }}/32}
{{- end }}
{{- end -}}
