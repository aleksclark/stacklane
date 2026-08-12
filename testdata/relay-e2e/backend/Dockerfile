# Minimal echo backends for relay Docker E2E (HTTP, HTTPS, UDP).

FROM golang:1.24.3-alpine AS build
WORKDIR /src
COPY main.go .
RUN go mod init relay-e2e-backend && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/echo-backend .

FROM scratch
COPY --from=build /out/echo-backend /echo-backend
USER 65532:65532
ENTRYPOINT ["/echo-backend"]
