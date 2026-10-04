FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go openapi.json ./
RUN CGO_ENABLED=0 go test ./... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/anaf-cui-lookup .

FROM alpine:3.20 AS certs
RUN apk add --no-cache ca-certificates

FROM scratch
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/anaf-cui-lookup /anaf-cui-lookup
USER 65532:65532
ENV PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/anaf-cui-lookup", "-healthcheck"]
ENTRYPOINT ["/anaf-cui-lookup"]
