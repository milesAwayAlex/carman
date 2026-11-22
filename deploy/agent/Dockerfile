# The agent image: a harness (Claude Code), the memory client, the routines
# an agent can be asked to run, and the script that backs the library up.
# The cmem binary (as cmem-ARCH) and the routines are in the build context
# (scripts/deploy.sh, .github/workflows/images.yml).
#
# BASE is what it is built on: Debian with node. An installation that wants
# its own shell and tools in an interactive agent's pod builds them into an
# image of its own and names it here (scripts/deploy.sh has a setting for it).
# What that image sets in ENV stays, and whatever it puts in
# /usr/local/lib/agent-start.d is run when an interactive agent starts.
ARG BASE=node:22-slim
FROM ${BASE}
ARG CLAUDE_CODE_VERSION=2.1.323
# An agent installs the harness when it starts, with its vendor's installer,
# so it is never older than the vendor's stable channel (agent-env). The copy
# here is what an agent falls back on when the installer cannot be reached.
# The harness is its vendor's, under the vendor's terms, and not this
# project's to hand out: "included" puts the copy in, for an image that
# stays where it was built; "at-start" leaves it out, for one that is
# published.
ARG HARNESS=included
# What an agent that works on code and clusters reaches for besides the harness.
ARG GH_VERSION=2.63.2
ARG KUBECTL_VERSION=1.33.0
ARG TOFU_VERSION=1.10.6
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends ca-certificates curl git jq gnupg tmux less vim-tiny procps \
 && curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg | gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" > /etc/apt/sources.list.d/google-cloud-sdk.list \
 && apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends python3 google-cloud-cli google-cloud-cli-gke-gcloud-auth-plugin \
 && rm -rf /var/lib/apt/lists/* /usr/lib/google-cloud-sdk/platform/bundledpythonunix \
 && gcloud --version | head -1 && gcloud info --format='value(basic.python_location)' \
 && arch="$(dpkg --print-architecture)" \
 && curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${arch}.tar.gz" \
    | tar -xz -C /usr/local/bin --strip-components=2 "gh_${GH_VERSION}_linux_${arch}/bin/gh" \
 && curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${arch}/kubectl" \
 && chmod 0755 /usr/local/bin/kubectl && gh --version && kubectl version --client \
 && curl -fsSL "https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}/tofu_${TOFU_VERSION}_linux_${arch}.tar.gz" \
    | tar -xz -C /usr/local/bin tofu \
 && tofu version | head -1 \
 && if [ "$HARNESS" = included ]; then npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && claude --version; fi \
 && { id agent >/dev/null 2>&1 || useradd --uid 10001 --create-home --shell /bin/bash agent; }
ARG TARGETARCH
COPY cmem-${TARGETARCH} /usr/local/bin/cmem
COPY run-routine backup-library agent-session agent-attach agent-resume agent-open renew-credential /usr/local/bin/
COPY agent-env /usr/local/lib/agent-env
COPY attention.md /usr/local/lib/agent-attention.md
# The memory hook, which agent-env puts in the harness's settings when an
# agent starts. The same as adapters/claude-code/settings.example.json;
# change one, change the other. An agent that is not doing ordinary work
# sets CMEM_HOOK=off.
COPY settings.json /usr/local/lib/agent-settings.json
# A login shell, such as the one in a tmux window, starts from /etc/profile,
# which sets PATH anew: give it the agent's environment again.
RUN ln -s /usr/local/lib/agent-env /etc/profile.d/agent-env.sh
COPY routines /routines
RUN chmod 0755 /usr/local/bin/cmem /usr/local/bin/run-routine /usr/local/bin/backup-library /usr/local/bin/agent-session /usr/local/bin/agent-attach /usr/local/bin/agent-resume /usr/local/bin/agent-open /usr/local/bin/renew-credential && cmem help > /dev/null
USER 10001
ENV HOME=/home/agent LANG=C.UTF-8
ENTRYPOINT ["run-routine"]
