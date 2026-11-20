# The agent image: a harness (Claude Code), the memory client, and the
# routines an agent can be asked to run. The cmem binary and the routines are
# in the build context (scripts/deploy.sh).
FROM node:22-slim
ARG CLAUDE_CODE_VERSION=2.1.311
# What an agent that works on code and clusters reaches for besides the harness.
ARG GH_VERSION=2.63.2
ARG KUBECTL_VERSION=1.33.0
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends ca-certificates curl git jq \
 && rm -rf /var/lib/apt/lists/* \
 && arch="$(dpkg --print-architecture)" \
 && curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${arch}.tar.gz" \
    | tar -xz -C /usr/local/bin --strip-components=2 "gh_${GH_VERSION}_linux_${arch}/bin/gh" \
 && curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${arch}/kubectl" \
 && chmod 0755 /usr/local/bin/kubectl && gh --version && kubectl version --client \
 && npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
 && claude --version \
 && useradd --uid 10001 --create-home agent
COPY cmem /usr/local/bin/cmem
COPY run-routine /usr/local/bin/run-routine
COPY routines /routines
RUN chmod 0755 /usr/local/bin/cmem /usr/local/bin/run-routine && cmem help > /dev/null
USER 10001
ENV HOME=/home/agent
ENTRYPOINT ["run-routine"]
