# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
# Modules first, so this layer stays cached until go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
# Cross-compile natively on the build host; only the kubectl stage runs on the target platform.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/gitops ./cmd/gitops

FROM alpine:3.20 AS kubectl
ARG KUBECTL_VERSION=v1.36.5
ARG TARGETARCH
RUN apk add --no-cache curl \
&& curl -fsSL -o /kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH:?}/kubectl" \
&& chmod +x /kubectl \
&& /kubectl version --client

FROM alpine:3.20
RUN apk add --no-cache git ca-certificates \
&& adduser -D -u 10001 gitops
COPY --from=kubectl /kubectl /usr/local/bin/kubectl
COPY --from=build /out/gitops /usr/local/bin/gitops
USER 10001

ENV HOME=/tmp
ENTRYPOINT [ "/usr/local/bin/gitops" ]