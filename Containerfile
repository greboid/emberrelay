FROM ghcr.io/greboid/dockerfiles/golang as builder

WORKDIR /app
COPY . /app
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -trimpath -ldflags=-buildid= -o main .

FROM ghcr.io/greboid/dockerfiles/base

COPY --from=builder /app/main /emberrelay
CMD ["/emberrelay"]
