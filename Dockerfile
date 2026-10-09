# The builder always runs on the build host's platform and cross-compiles for
# the target, so multi-arch builds don't need QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
ARG TARGETOS TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o cs2mapserver .

# distroless/static ships CA certificates (needed for the Steam Web API) and
# runs as an unprivileged user (uid/gid 65532), so mounted secret files must
# be readable by that user.
FROM gcr.io/distroless/static-debian13:nonroot
WORKDIR /app
COPY --from=builder /app/cs2mapserver .
ENV PORT=16969
EXPOSE 16969
ENTRYPOINT ["./cs2mapserver"]
