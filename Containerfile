FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY v1alpha1 ./v1alpha1
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /reprise ./cmd/reprise

FROM gcr.io/distroless/static:nonroot
COPY --from=build /reprise /reprise
USER 65532:65532
ENTRYPOINT ["/reprise"]
