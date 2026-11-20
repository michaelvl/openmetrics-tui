# Two image targets, both without kubectl:
#
#   docker build -t omtui-dash:dev .                      omtui-dash only; the
#       VictoriaMetrics backend runs as a separate container (examples/ssh).
#   docker build --target single -t omtui-dash-single:dev .   omtui-dash and a
#       pinned victoria-metrics, for --managed --in-cluster (examples/ssh-single).
FROM golang:1.24.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /omtui-dash ./cmd/omtui-dash
# A directory the non-root user may write, for --data-dir; distroless has none.
RUN mkdir /data

# The VictoriaMetrics release image holds a static binary at
# /victoria-metrics-prod. Keep the version in step with examples/ssh.
FROM victoriametrics/victoria-metrics:v1.156.0 AS victoria-metrics

FROM gcr.io/distroless/static-debian12:nonroot AS single
COPY --from=build /omtui-dash /omtui-dash
COPY --from=victoria-metrics /victoria-metrics-prod /usr/local/bin/victoria-metrics
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
ENTRYPOINT ["/omtui-dash"]

# Last, so a plain `docker build .` still gives the omtui-dash-only image.
FROM gcr.io/distroless/static-debian12:nonroot AS dash
COPY --from=build /omtui-dash /omtui-dash
USER 65532:65532
ENTRYPOINT ["/omtui-dash"]
