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

{{- /* The schedules the dispatcher keeps, as JSON: those listed under
dispatcher.schedules, each agent with dispatched: true, and the usage
reading. An agent's run may start up to its catch-up late before it counts
as not started, and is given ten minutes beyond its Job's own deadline, so
that the Job's failure, not an overrun, is what is told. */ -}}
{{- define "dispatcher.schedules" -}}
{{- $out := list }}
{{- range .Values.dispatcher.schedules }}{{ $out = append $out . }}{{ end }}
{{- range .Values.agents }}
{{- if and .dispatched (not .interactive) }}
{{- $out = append $out (dict "name" (printf "agent-%s" .name) "class" (printf "agent-%s" .name) "cron" .schedule "timeZone" "UTC" "startWithin" (printf "%ds" (int (.catchUpSeconds | default 86400))) "timeout" (printf "%ds" (add (int (.deadlineSeconds | default 3600)) 600)) "priority" (int (.priority | default 0))) }}
{{- end }}
{{- end }}
{{- /* The installation's own nightly and twice-daily jobs, each when its
value says (ADR 0059); the dreamer only while it is switched on. */ -}}
{{- with .Values.dreamer }}
{{- if and .dispatched (not .suspend) }}
{{- $out = append $out (dict "name" "dreamer" "class" "dreamer" "cron" .schedule "timeZone" "UTC" "startWithin" (printf "%ds" (int .catchUpSeconds)) "timeout" (printf "%ds" (add (int .deadlineSeconds) 600)) "priority" (int .priority)) }}
{{- end }}
{{- end }}
{{- with .Values.backup }}
{{- if and .enabled .dispatched }}
{{- $out = append $out (dict "name" "backup" "class" "backup" "cron" .schedule "timeZone" "UTC" "startWithin" (printf "%ds" (int .catchUpSeconds)) "timeout" (printf "%ds" (add (int .deadlineSeconds) 600)) "priority" (int .priority)) }}
{{- end }}
{{- end }}
{{- with .Values.agent.warm }}
{{- if and .enabled .dispatched }}
{{- $out = append $out (dict "name" "agent-warm" "class" "agent-warm" "cron" .schedule "timeZone" "UTC" "startWithin" (printf "%ds" (int .catchUpSeconds)) "timeout" (printf "%ds" (add (int .deadlineSeconds) 600)) "priority" (int .priority)) }}
{{- end }}
{{- end }}
{{- if .Values.usage.enabled }}
{{- with .Values.usage }}
{{- $out = append $out (dict "name" "usage" "class" "agent-usage" "cron" .schedule "timeZone" "UTC" "startWithin" "30m" "timeout" (printf "%ds" (add (int .deadlineSeconds) 600)) "priority" (int .priority)) }}
{{- end }}
{{- end }}
{{- toJson $out }}
{{- end -}}

{{- /* The models the usage reading reads (ADR 0043), as MODEL=HOURS,...:
every model the installation's definitions name (each agent's ladder, or
its default; a standing agent's model where the values give one; the
dreamer's ladders) and the reading's own, with usage.addModels and less
usage.skipModels. The reading's own model is read every hour, the others
every usage.everyHours, unless usage.everyHoursFor says otherwise. */ -}}
{{- define "usage.models" -}}
{{- $u := .Values.usage }}
{{- $ms := list $u.model }}
{{- range .Values.agents }}
{{- if .interactive }}
{{- with .model }}{{ $ms = append $ms . }}{{ end }}
{{- range .models }}{{ $ms = append $ms . }}{{ end }}
{{- else }}
{{- range (.models | default (list "claude-sonnet-5-5")) }}{{ $ms = append $ms . }}{{ end }}
{{- end }}
{{- end }}
{{- range .Values.dreamer.models }}{{ $ms = append $ms . }}{{ end }}
{{- range $routine, $ladder := .Values.dreamer.modelsFor }}{{ range $ladder }}{{ $ms = append $ms . }}{{ end }}{{ end }}
{{- range $u.addModels }}{{ $ms = append $ms . }}{{ end }}
{{- $out := list }}
{{- range uniq $ms }}
{{- if not (has . $u.skipModels) }}
{{- $every := ternary 1 $u.everyHours (eq . $u.model) }}
{{- with index ($u.everyHoursFor | default dict) . }}{{ $every = . }}{{ end }}
{{- $out = append $out (printf "%s=%d" . (int $every)) }}
{{- end }}
{{- end }}
{{- join "," $out }}
{{- end -}}

{{- /* Who may write to each agent and its sessions besides the owner, its
own sessions and an answer (ADR 0047), as JSON: each agent's writtenBy.
A name there that is not an agent of the installation fails the render. */ -}}
{{- define "dispatcher.writers" -}}
{{- $names := list }}
{{- range .Values.agents }}{{ $names = append $names .name }}{{ end }}
{{- $out := dict }}
{{- range .Values.agents }}
{{- $agent := .name }}
{{- range (.writtenBy | default list) }}
{{- if not (has . $names) }}
{{- fail (printf "agents: %s: writtenBy names %q, which is not an agent here (the agents are %s)" $agent . (join ", " $names)) }}
{{- end }}
{{- end }}
{{- with .writtenBy }}{{ $_ := set $out $agent . }}{{ end }}
{{- end }}
{{- with .Values.dispatcher.host }}
{{- if has .place $names }}
{{- fail (printf "dispatcher.host.place: %q is the name of an agent here; the owner's machine needs a name of its own" .place) }}
{{- end }}
{{- range (.writtenBy | default list) }}
{{- if not (has . $names) }}
{{- fail (printf "dispatcher.host.writtenBy names %q, which is not an agent here (the agents are %s)" . (join ", " $names)) }}
{{- end }}
{{- end }}
{{- if and .place .writtenBy }}{{ $_ := set $out .place .writtenBy }}{{ end }}
{{- end }}
{{- toJson $out }}
{{- end -}}
