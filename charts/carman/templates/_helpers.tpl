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
image: {{ include "agent.jobImageName" . }}
imagePullPolicy: {{ ternary "Always" "IfNotPresent" (default false .Values.agent.warm.enabled) }}
{{- end -}}

{{- define "agent.jobImageName" -}}
{{- if .Values.agent.warm.enabled -}}
{{ include "warm.repository" . }}:{{ .Values.agent.warm.tag }}
{{- else -}}
{{ required "agent.image is required" .Values.agent.image }}
{{- end }}
{{- end -}}

{{- /* The image an agent's pod runs, for it to know: AGENT_IMAGE. Given
the root and whether the pod is a standing one. */ -}}
{{- define "agent.imageEnv" -}}
- name: AGENT_IMAGE
  value: {{ ternary (required "agent.image is required" .root.Values.agent.image) (include "agent.jobImageName" .root) .standing | quote }}
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
{{- /* A schedule s with the after and afterWithin of the values v, when
set (ADR 0062). */ -}}
{{- define "dispatcher.after" -}}
{{- $s := index . 0 }}{{- $v := index . 1 }}
{{- if $v.after }}{{- $_ := set $s "after" $v.after }}{{ end }}
{{- if $v.afterWithin }}{{- $_ := set $s "afterWithin" $v.afterWithin }}{{ end }}
{{- toJson $s }}
{{- end -}}

{{- define "dispatcher.schedules" -}}
{{- $out := list }}
{{- range .Values.dispatcher.schedules }}{{ $out = append $out . }}{{ end }}
{{- range .Values.agents }}
{{- if and .dispatched (not .interactive) }}
{{- $s := dict "name" (printf "agent-%s" .name) "class" (printf "agent-%s" .name) "cron" .schedule "timeZone" "UTC" "startWithin" (printf "%ds" (int (.catchUpSeconds | default 86400))) "timeout" (printf "%ds" (add (int (.deadlineSeconds | default 3600)) 600)) "priority" (int (.priority | default 0)) }}
{{- $out = append $out (include "dispatcher.after" (list $s .) | fromJson) }}
{{- end }}
{{- end }}
{{- /* The installation's own nightly and twice-daily jobs, each when its
value says (ADR 0059); the dreamer only while it is switched on. */ -}}
{{- with .Values.dreamer }}
{{- if and .dispatched (not .suspend) }}
{{- $s := dict "name" "dreamer" "class" "dreamer" "cron" .schedule "timeZone" "UTC" "startWithin" (printf "%ds" (int .catchUpSeconds)) "timeout" (printf "%ds" (add (int .deadlineSeconds) 600)) "priority" (int .priority) }}
{{- $out = append $out (include "dispatcher.after" (list $s .) | fromJson) }}
{{- end }}
{{- end }}
{{- with .Values.backup }}
{{- if and .enabled .dispatched }}
{{- $s := dict "name" "backup" "class" "backup" "cron" .schedule "timeZone" "UTC" "startWithin" (printf "%ds" (int .catchUpSeconds)) "timeout" (printf "%ds" (add (int .deadlineSeconds) 600)) "priority" (int .priority) }}
{{- $out = append $out (include "dispatcher.after" (list $s .) | fromJson) }}
{{- end }}
{{- end }}
{{- with .Values.agent.warm }}
{{- if and .enabled .dispatched }}
{{- $s := dict "name" "agent-warm" "class" "agent-warm" "cron" .schedule "timeZone" "UTC" "startWithin" (printf "%ds" (int .catchUpSeconds)) "timeout" (printf "%ds" (add (int .deadlineSeconds) 600)) "priority" (int .priority) }}
{{- $out = append $out (include "dispatcher.after" (list $s .) | fromJson) }}
{{- end }}
{{- end }}
{{- /* A watch asks its source on its own schedule (ADR 0063). */ -}}
{{- range .Values.dispatcher.watches }}
{{- $out = append $out (dict "name" (printf "watch-%s" .name) "class" (printf "watch-%s" .name) "cron" (required "a watch needs a cron" .cron) "timeZone" (.timeZone | default "UTC") "startWithin" "10m" "timeout" "15m" "priority" (int (.priority | default 60))) }}
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
{{- if eq . "dreamer" }}
{{- fail (printf "agents: %s: writtenBy names the dreamer, which reads all of memory and so writes only to the owner and answers what it is sent (ADR new-the-dreamer-is-a-party)" $agent) }}
{{- end }}
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

{{- /* The improvement loop's settings (ADR 0058), as JSON. The surveyor and
the agents of the list are agents of the installation. */ -}}
{{- define "dispatcher.improvement" -}}
{{- $names := list }}
{{- range .Values.agents }}{{ $names = append $names .name }}{{ end }}
{{- with .Values.dispatcher.improvement }}
{{- range (.agents | default list) }}
{{- if not (has . $names) }}
{{- fail (printf "dispatcher.improvement.agents names %q, which is not an agent here (the agents are %s)" . (join ", " $names)) }}
{{- end }}
{{- end }}
{{- if and .surveyor (not (has .surveyor $names)) }}
{{- fail (printf "dispatcher.improvement.surveyor is %q, which is not an agent here (the agents are %s)" .surveyor (join ", " $names)) }}
{{- end }}
{{- if and (eq (.usage.rule | default "below") "room") (not $.Values.dispatcher.pace.room) }}
{{- fail "dispatcher.improvement.usage.rule: room needs dispatcher.pace.room, which records the room pacing expects (ADR 0060)" }}
{{- end }}
{{- if and (or .send .notify) (not .surveyor) }}
{{- fail "dispatcher.improvement: send and notify need a surveyor" }}
{{- end }}
{{- toJson . }}
{{- end }}
{{- end }}

{{- /* The names of the watches (ADR 0063), with commas. An agent may not be
named as a watch's job is: the dispatcher knows a watch's job by that name. */ -}}
{{- define "dispatcher.watches" -}}
{{- range .Values.agents }}
{{- if hasPrefix "watch-" .name }}{{ fail (printf "agents: %s: a name that starts with watch- is a watch's job's (dispatcher.watches)" .name) }}{{ end }}
{{- end }}
{{- $names := list }}
{{- range .Values.dispatcher.watches }}
{{- if not (regexMatch "^[a-z0-9][a-z0-9-]{0,30}$" .name) }}{{ fail (printf "dispatcher.watches: %q: a watch's name is lowercase letters, digits and -" .name) }}{{ end }}
{{- if eq (empty .url) (empty .command) }}{{ fail (printf "dispatcher.watches: %s has a url or a command, one of them" .name) }}{{ end }}
{{- $names = append $names .name }}
{{- end }}
{{- join "," $names }}
{{- end -}}

{{- /* The peers a service reached from outside the namespace admits
(SERVICE.service.admitFrom): each a CIDR, as the pod sees the caller, or
pods of another namespace by their labels ({namespace, podLabels}), which
is what a cluster's own load balancer is where it runs as pods. */ -}}
{{- define "admit.peers" -}}
{{- range . }}
{{- if kindIs "string" . }}
- ipBlock: {cidr: {{ . | quote }}}
{{- else }}
- namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: {{ required "an admitFrom entry that is not a CIDR names a namespace" .namespace | quote }}
  podSelector:
    matchLabels:
      {{- toYaml (required "an admitFrom entry that names a namespace names the pods' labels" .podLabels) | nindent 6 }}
{{- end }}
{{- end }}
{{- end -}}
