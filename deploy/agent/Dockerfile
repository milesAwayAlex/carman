# The agent image: a harness (Claude Code), the memory client, and the
# routines an agent can be asked to run. The cmem binary and the routines are
# in the build context (scripts/deploy.sh).
FROM node:22-slim
ARG CLAUDE_CODE_VERSION=2.1.311
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends ca-certificates curl git \
 && rm -rf /var/lib/apt/lists/* \
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
