# Standalone image build: docker build -t twinstub .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/twinstub/twinstub/internal/version.Version=${VERSION}" \
    -o /twinstub ./cmd/twinstub

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /twinstub /twinstub
# Private targets stay allowed by default for local development; set to
# false when the container can reach internal networks you want protected.
ENV TWINSTUB_ALLOW_PRIVATE_TARGETS=true
ENV TWINSTUB_LOG_FORMAT=json
WORKDIR /work
EXPOSE 8080 9090
USER nonroot
ENTRYPOINT ["/twinstub"]
CMD ["serve"]
