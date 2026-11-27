#!/usr/bin/env bash
# What reaches the installation's pods, tried from where each caller is:
# a pod elsewhere on the cluster, a pod of the namespace, an agent's pod, a
# pod of the agents' sandbox, and this machine through the load balancer.
# Run from the owner's machine, which may make pods in the installation's
# namespace and a throwaway namespace.
#
#   scripts/check-ingress.sh CONTEXT NAMESPACE source   BEFORE isolating: what address a pod sees for a
#                                        connection that came in through the load balancer
#                                        (what a service's admitFrom has to name)
#   scripts/check-ingress.sh CONTEXT NAMESPACE          the check: each way in open to whom it is for
#                                        and closed to the rest
#
# MEMORY_URL (http://127.0.0.1:8377) and DISPATCHER_URL (http://127.0.0.1:8379)
# are where this machine reaches the two services. If isolating has locked
# this machine out, take the rules away (a deploy puts them back; the value
# isolated: false keeps them away):
#
#   kubectl --context CONTEXT -n NAMESPACE delete networkpolicy only-this-namespace-comes-in \
#       memory-admits-the-owners-machine dispatcher-admits-the-owners-machine
#
# and, if agents cannot reach the proxy: ... delete networkpolicy proxy-admits-only-agents
set -euo pipefail

die() { echo "check-ingress: $*" >&2; exit 1; }
context="${1:-}" ns="${2:-}" action="${3:-check}"
[ -n "$context" ] && [ -n "$ns" ] || die "say: scripts/check-ingress.sh CONTEXT NAMESPACE [source]"
k=(kubectl --context "$context")
image="$("${k[@]}" -n "$ns" get deployment dispatcher -o 'jsonpath={.spec.template.spec.containers[0].image}')" || die "no dispatcher in $ns"
memory_url="${MEMORY_URL:-http://127.0.0.1:8377}" dispatcher_url="${DISPATCHER_URL:-http://127.0.0.1:8379}"
undo="kubectl --context $context -n $ns delete networkpolicy only-this-namespace-comes-in memory-admits-the-owners-machine dispatcher-admits-the-owners-machine"

pod() {  # NAMESPACE NAME LABELS-JSON COMMAND
    "${k[@]}" -n "$1" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $2
  labels: $3
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
    - name: main
      image: $image
      command: ["sh", "-c", "$4"]
      resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {memory: 64Mi}}
YAML
    "${k[@]}" -n "$1" wait --for=condition=ready "pod/$2" --timeout=120s >/dev/null
}

if [ "$action" = source ]; then
    port=8390
    trap '"${k[@]}" -n "$ns" delete pod/ingress-source service/ingress-source --ignore-not-found --wait=false >/dev/null 2>&1' EXIT
    pod "$ns" ingress-source '{"app": "ingress-source"}' "nc -lk -v -p $port -e true 2>&1 || while true; do nc -l -v -p $port </dev/null 2>&1; done"
    "${k[@]}" -n "$ns" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Service
metadata: {name: ingress-source}
spec:
  type: LoadBalancer
  selector: {app: ingress-source}
  ports: [{port: $port}]
YAML
    echo "a pod listens behind a load balancer on port $port; connecting to it from this machine ..."
    for _ in $(seq 30); do
        curl -s -m 2 -o /dev/null "http://127.0.0.1:$port/" 2>/dev/null || true
        seen="$("${k[@]}" -n "$ns" logs ingress-source 2>/dev/null | sed -n 's/.* from \[\{0,1\}\(::ffff:\)\{0,1\}\([0-9.]*\)\]\{0,1\}:.*/\2/p' | sort -u | tr '\n' ' ')"
        [ -z "$seen" ] || break
        sleep 2
    done
    [ -n "${seen:-}" ] || die "nothing arrived: is a LoadBalancer's port forwarded to this machine at 127.0.0.1:$port? The pod's log: $("${k[@]}" -n "$ns" logs ingress-source 2>&1 | tail -3)"
    echo "the pod saw the connection come from: $seen"
    for a in $seen; do
        who="$("${k[@]}" get pods -A -o wide --no-headers 2>/dev/null | awk -v a="$a" '$7 == a {print $1 "/" $2}')"
        if [ -n "$who" ]; then
            echo "  $a is the pod $who, with labels: $("${k[@]}" -n "${who%%/*}" get pod "${who##*/}" -o 'jsonpath={.metadata.labels}')"
            echo "  admit it by its labels: admitFrom: [{namespace: ${who%%/*}, podLabels: {...}}]"
        else
            echo "  $a is no pod's: the node's own address, most likely. Admit it as a CIDR: admitFrom: [\"$a/32\"]"
        fi
    done
    exit 0
fi

fails=0
note() { echo "ok: $*"; }
bad() { echo "WRONG: $*" >&2; fails=$((fails + 1)); }
# tried NAMESPACE POD HOST PORT: open or closed, as the pod itself says; a
# try is ended after four seconds (the image's nc waits two minutes on a
# connection that is dropped, whatever it is told). Nothing, if the pod
# could not be asked: that is not "closed".
tried() { "${k[@]}" -n "$1" exec "$2" -- sh -c "if timeout 4 nc -w 2 $3 $4 </dev/null >/dev/null 2>&1; then echo open; else echo closed; fi" 2>/dev/null | tail -1; }
expect() {  # open|closed NAMESPACE POD WHO HOST PORT WHAT
    local got
    got="$(tried "$2" "$3" "$5" "$6" || true)"
    case "$got" in
    "$1") note "$4: $7 is $1" ;;
    open | closed) bad "$4: $7 is $got, and should be $1" ;;
    *) bad "$4: $7 could not be tried: the pod $3 in $2 did not answer ($("${k[@]}" -n "$2" get pod "$3" -o 'jsonpath={.status.phase}' 2>&1))" ;;
    esac
}

stranger="ingress-check-$(date +%H%M%S)"
# The agents' sandbox: SANDBOX, or the namespace the agents' own rule lets them reach.
sandbox="${SANDBOX:-}"
[ -n "$sandbox" ] || sandbox="$("${k[@]}" -n "$ns" get networkpolicy agents-leave-through-the-proxy -o json 2>/dev/null | sed -n 's/.*"kubernetes.io\/metadata.name": *"\([^"]*\)".*/\1/p' | grep -v "^$ns$" | tail -1 || true)"
trials="$("${k[@]}" -n "$ns" get service egress -o 'jsonpath={.spec.ports[?(@.name=="model-trials")].port}' 2>/dev/null || true)"
cleanup() {
    "${k[@]}" delete namespace "$stranger" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    "${k[@]}" -n "$ns" delete pod ingress-check-agent ingress-check-other --ignore-not-found --wait=false >/dev/null 2>&1 || true
    [ -z "${sandbox_pods:-}" ] || "${k[@]}" -n "$sandbox" delete pod ingress-check-trial ingress-check-plain --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT
"${k[@]}" create namespace "$stranger" >/dev/null
pod "$stranger" stranger '{"app": "stranger"}' "sleep 3600"
# The agent's pod listens too, so that its own address can be tried.
pod "$ns" ingress-check-agent '{"app": "ingress-check", "app.kubernetes.io/component": "agent"}' "(while true; do nc -l -p 9999 </dev/null >/dev/null 2>&1; done) & sleep 3600"
pod "$ns" ingress-check-other '{"app": "ingress-check"}' "sleep 3600"
sleep 3   # a pod's own rules are in place a moment after it starts

echo "from a pod elsewhere on the cluster (namespace $stranger, no rules of its own):"
expect closed "$stranger" stranger "a stranger" "egress.$ns.svc" 3128 "the proxy"
expect closed "$stranger" stranger "a stranger" "cmem.$ns.svc" 8377 "the memory"
expect closed "$stranger" stranger "a stranger" "dispatcher.$ns.svc" 8379 "the dispatcher"
expect closed "$stranger" stranger "a stranger" "dispatcher-db.$ns.svc" 5432 "the dispatcher's database"
[ -z "$trials" ] || expect closed "$stranger" stranger "a stranger" "egress.$ns.svc" "$trials" "the meter's port for trials"
agent_ip="$("${k[@]}" -n "$ns" get pod ingress-check-agent -o 'jsonpath={.status.podIP}')"
expect closed "$stranger" stranger "a stranger" "$agent_ip" 9999 "an agent's pod"

echo "from an agent's pod of the namespace:"
expect open "$ns" ingress-check-agent "an agent" egress 3128 "the proxy"
expect open "$ns" ingress-check-agent "an agent" cmem 8377 "the memory"
expect open "$ns" ingress-check-agent "an agent" dispatcher 8379 "the dispatcher"
expect closed "$ns" ingress-check-agent "an agent" dispatcher-db 5432 "the dispatcher's database"

echo "from a pod of the namespace that is no agent's:"
expect closed "$ns" ingress-check-other "another pod" egress 3128 "the proxy"
expect open "$ns" ingress-check-other "another pod" cmem 8377 "the memory"
expect open "$ns" ingress-check-other "another pod" dispatcher 8379 "the dispatcher"

if [ -n "$sandbox" ] && "${k[@]}" get namespace "$sandbox" >/dev/null 2>&1; then
    sandbox_pods=1
    pod "$sandbox" ingress-check-plain '{"app": "ingress-check"}' "sleep 3600"
    [ -z "$trials" ] || pod "$sandbox" ingress-check-trial '{"app": "ingress-check", "reaches-the-model": "true"}' "sleep 3600"
    sleep 3
    echo "from the agents' sandbox ($sandbox):"
    expect closed "$sandbox" ingress-check-plain "a sandbox pod" "egress.$ns.svc" 3128 "the proxy"
    expect closed "$sandbox" ingress-check-plain "a sandbox pod" "cmem.$ns.svc" 8377 "the memory"
    expect closed "$sandbox" ingress-check-plain "a sandbox pod" "dispatcher.$ns.svc" 8379 "the dispatcher"
    if [ -n "$trials" ]; then
        expect closed "$sandbox" ingress-check-plain "a sandbox pod" "egress.$ns.svc" "$trials" "the meter's port for trials"
        expect open "$sandbox" ingress-check-trial "a sandbox pod that says it reaches the model" "egress.$ns.svc" "$trials" "the meter's port for trials"
        expect closed "$sandbox" ingress-check-trial "a sandbox pod that says it reaches the model" "egress.$ns.svc" 3128 "the proxy"
    fi
fi

echo "from this machine, through the load balancer:"
for pair in "the memory=$memory_url" "the dispatcher=$dispatcher_url"; do
    if curl -fsS -m 5 -o /dev/null "${pair#*=}/healthz" 2>/dev/null; then note "this machine: ${pair%%=*} answers at ${pair#*=}"; else bad "this machine: ${pair%%=*} does not answer at ${pair#*=}"; fi
done
echo "and the node's own checks of the pods:"
sleep 20
unready="$("${k[@]}" -n "$ns" get pods --field-selector=status.phase=Running -o 'jsonpath={range .items[*]}{.metadata.name}={range .status.containerStatuses[*]}{.ready} {end}{"\n"}{end}' | grep false | grep -v '^ingress-' || true)"
if [ -z "$unready" ]; then note "every running pod of $ns is ready"; else bad "not ready: $(echo $unready)"; fi

if [ "$fails" -gt 0 ]; then
    echo "$fails thing(s) are not as they should be. To take the namespace's rules away: $undo" >&2
    exit 1
fi
echo "every way in is open to whom it is for and closed to the rest"
