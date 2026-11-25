# The dispatcher: its binary, and pg_dump of the same major version as the
# Postgres the chart runs for it (dispatcher.database.image), with which it
# makes the dump the nightly backup copies. The binary is built outside, once
# for each architecture, and is in the build context as dispatcher-ARCH
# (scripts/deploy.sh).
FROM alpine:3.22
ARG TARGETARCH
RUN apk add --no-cache postgresql17-client && adduser -D -u 10001 dispatcher
COPY dispatcher-${TARGETARCH} /usr/local/bin/dispatcher
USER 10001
EXPOSE 8379
ENTRYPOINT ["dispatcher"]
CMD ["serve"]
