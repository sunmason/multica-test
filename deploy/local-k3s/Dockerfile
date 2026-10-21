FROM golang:1.27-alpine@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -o /out/gsb-server ./cmd/server

FROM docker:29.8.2-cli@sha256:35cfcf893c20f2931f4909151b5407aaaeab711277de7ce8b8f521f095d54c32
COPY --from=build /out/gsb-server /usr/local/bin/gsb-server
USER 10001:10001
ENV GSB_RUNNER=docker GSB_LISTEN=:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gsb-server"]
