# The memory service: the cmem binary and git, which it keeps its library in.
# The binary is built outside, once for each architecture, and is in the
# build context as cmem-ARCH (scripts/deploy.sh, .github/workflows/images.yml).
FROM alpine:3.22
ARG TARGETARCH
RUN apk add --no-cache git && adduser -D -u 10001 cmem
COPY cmem-${TARGETARCH} /usr/local/bin/cmem
USER 10001
# git wants a home directory for its configuration; nothing is kept there.
ENV HOME=/tmp
EXPOSE 8377
ENTRYPOINT ["cmem", "serve", "--listen", "0.0.0.0:8377"]
