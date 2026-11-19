# omtui-dash only. The VictoriaMetrics backend runs as a separate container
# (see examples/ssh); there is no kubectl and no victoria-metrics in here.
FROM golang:1.24.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /omtui-dash ./cmd/omtui-dash

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /omtui-dash /omtui-dash
USER 65532:65532
ENTRYPOINT ["/omtui-dash"]
