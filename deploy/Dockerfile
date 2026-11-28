# Базовые образы закреплены по digest, обновляет Dependabot.
FROM golang:1.26-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION — тег релиза: брокер сообщает его клиентам, и те подсказывают обновиться
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/broker ./cmd/broker

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN adduser -D -u 10001 broker && mkdir /data && chown broker /data
COPY --from=build /out/broker /usr/local/bin/broker
USER broker
VOLUME /data
EXPOSE 8080
ENV BROKER_DB=/data/broker.db BROKER_ADDR=:8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["broker"]
CMD ["serve"]
