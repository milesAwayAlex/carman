# The agent image: a harness (Claude Code), the memory client, the routines
# an agent can be asked to run, and the script that backs the library up.
# The cmem binary (as cmem-ARCH) and the routines are in the build context
# (scripts/deploy.sh, .github/workflows/images.yml).
FROM node:22-slim
ARG CLAUDE_CODE_VERSION=2.1.311
# The harness is its vendor's, under the vendor's terms, and not this
# project's to hand out. "included" puts it in the image, for an image that
# stays where it was built. "at-start" leaves it out, for an image that is
# published: the pod fetches that version from the vendor's own package when
# it starts (run-routine).
ARG HARNESS=included
# What an agent that works on code and clusters reaches for besides the harness.
ARG GH_VERSION=2.63.2
ARG KUBECTL_VERSION=1.33.0
ARG TOFU_VERSION=1.10.6
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends ca-certificates curl git jq gnupg \
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
 && useradd --uid 10001 --create-home agent
ARG TARGETARCH
COPY cmem-${TARGETARCH} /usr/local/bin/cmem
COPY run-routine backup-library /usr/local/bin/
COPY routines /routines
RUN chmod 0755 /usr/local/bin/cmem /usr/local/bin/run-routine /usr/local/bin/backup-library && cmem help > /dev/null
USER 10001
ENV HOME=/home/agent HARNESS_VERSION=${CLAUDE_CODE_VERSION}
ENTRYPOINT ["run-routine"]
