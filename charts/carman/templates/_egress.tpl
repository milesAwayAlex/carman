{{/* What the egress proxy runs on: see egress.yaml and ADR 0031. */}}
{{- define "egress.envoy" -}}
{{- $creds := .Values.egress.credentials }}
{{- $renewed := list }}{{- range .Values.egress.renewals }}{{- $renewed = append $renewed .key }}{{- end }}
{{- if $renewed }}
# A credential that is read again when its file changes is fetched as a
# discovered secret, and the proxy has to have a name to ask for one.
node: {id: egress, cluster: egress}
{{- end }}
{{- if $creds }}
bootstrap_extensions:
  - name: envoy.bootstrap.internal_listener
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.bootstrap.internal_listener.v3.InternalListener
{{- end }}
static_resources:
  {{- if $creds }}
  secrets:
    {{- range $creds }}
    {{- if not (has .key $renewed) }}
    - name: credential-{{ .key }}
      generic_secret:
        secret: {filename: /etc/envoy/credentials/{{ .key }}}
    {{- end }}
    {{- end }}
  {{- end }}
  listeners:
    - name: egress
      address:
        socket_address: {address: 0.0.0.0, port_value: 3128}
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: egress
                # Python's standard library asks for its tunnel in HTTP/1.0,
                # which Envoy refuses unless told to accept it. Seen with a
                # cloud provider's command-line tool: "426 Upgrade Required".
                http_protocol_options: {accept_http_10: true}
                # One line for each connection an agent opened: when, to
                # where, how it ended, how long it was open and how much
                # went each way. Never what was said.
                access_log:
                  - name: envoy.access_loggers.stdout
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog
                      log_format:
                        text_format_source:
                          inline_string: "%START_TIME% egress from=%DOWNSTREAM_REMOTE_ADDRESS_WITHOUT_PORT% %REQ(:METHOD)% %REQ(:AUTHORITY)% status=%RESPONSE_CODE% flags=%RESPONSE_FLAGS% ms=%DURATION% sent=%BYTES_RECEIVED% received=%BYTES_SENT%\n"
                route_config:
                  name: out
                  virtual_hosts:
                    - name: anywhere
                      domains: ["*"]
                      routes:
                        {{- range $creds }}
                        # A tunnel to {{ .host }} ends here, at the listener
                        # that answers in its name and adds the credential.
                        - match:
                            connect_matcher: {}
                            headers:
                              - name: ":authority"
                                string_match: {exact: "{{ .host }}:443"}
                          route:
                            cluster: answer-{{ .key }}
                            timeout: 0s
                            upgrade_configs:
                              - upgrade_type: CONNECT
                                connect_config: {}
                        {{- end }}
                        # HTTPS: the client asks for a tunnel to a host.
                        - match: {connect_matcher: {}}
                          route:
                            cluster: outside
                            timeout: 0s
                            upgrade_configs:
                              - upgrade_type: CONNECT
                                connect_config: {}
                        - match: {prefix: "/"}
                          route: {cluster: outside, timeout: 0s}
                http_filters:
                  - name: envoy.filters.http.dynamic_forward_proxy
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_forward_proxy.v3.FilterConfig
                      dns_cache_config: {name: outside, dns_lookup_family: V4_ONLY}
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                upgrade_configs:
                  - upgrade_type: CONNECT
    {{- range $creds }}
    # In the name of {{ .host }}: the agent's TLS ends here, with a
    # certificate from an authority of this installation; the credential is added;
    # and the request goes on to the real host over TLS that is checked
    # against the public authorities.
    - name: answer-{{ .key }}
      internal_listener: {}
      filter_chains:
        - transport_socket:
            name: envoy.transport_sockets.tls
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
              common_tls_context:
                alpn_protocols: ["http/1.1"]
                tls_certificates:
                  - certificate_chain: {filename: /etc/envoy/tls/tls.crt}
                    private_key: {filename: /etc/envoy/tls/tls.key}
          filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: answer_{{ .key }}
                # With a credential attached, each request is on record:
                # what was asked of the host (the start of its path, which
                # may include a query), and how it answered. No headers,
                # no bodies.
                access_log:
                  - name: envoy.access_loggers.stdout
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog
                      log_format:
                        text_format_source:
                          inline_string: "%START_TIME% egress credential={{ .key }} %REQ(:METHOD)% %REQ(:AUTHORITY)%%REQ(:PATH):160% status=%RESPONSE_CODE% flags=%RESPONSE_FLAGS% ms=%DURATION%\n"
                route_config:
                  name: real-{{ .key }}
                  virtual_hosts:
                    - name: host
                      domains: ["{{ .host }}", "{{ .host }}:443"]
                      routes:
                        - match: {prefix: "/"}
                          route: {cluster: real-{{ .key }}, timeout: 0s}
                http_filters:
                  - name: envoy.filters.http.credential_injector
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.credential_injector.v3.CredentialInjector
                      # Whatever the agent sent in its place is replaced.
                      overwrite: true
                      credential:
                        name: envoy.http.injected_credentials.generic
                        typed_config:
                          "@type": type.googleapis.com/envoy.extensions.http.injected_credentials.generic.v3.Generic
                          {{- if has .key $renewed }}
                          # One that is renewed beside the proxy is read again
                          # whenever its file is replaced.
                          credential:
                            name: credential-{{ .key }}
                            sds_config:
                              path_config_source:
                                path: /etc/envoy/renewed-{{ .key }}.yaml
                                watched_directory: {path: /etc/envoy/renewed}
                          {{- else }}
                          credential: {name: credential-{{ .key }}}
                          {{- end }}
                          header: Authorization
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
    {{- end }}
  clusters:
    {{- range $creds }}
    - name: answer-{{ .key }}
      load_assignment:
        cluster_name: answer-{{ .key }}
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    envoy_internal_address:
                      server_listener_name: answer-{{ .key }}
    - name: real-{{ .key }}
      type: LOGICAL_DNS
      dns_lookup_family: V4_ONLY
      load_assignment:
        cluster_name: real-{{ .key }}
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: {address: {{ .host }}, port_value: 443}
      transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
          sni: {{ .host }}
          common_tls_context:
            validation_context:
              trusted_ca: {filename: /etc/ssl/certs/ca-certificates.crt}
              match_typed_subject_alt_names:
                - san_type: DNS
                  matcher: {exact: {{ .host }}}
    {{- end }}
    - name: outside
      lb_policy: CLUSTER_PROVIDED
      cluster_type:
        name: envoy.clusters.dynamic_forward_proxy
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_forward_proxy.v3.ClusterConfig
          dns_cache_config: {name: outside, dns_lookup_family: V4_ONLY}
{{- end }}
