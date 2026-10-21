# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
# Modules first, so this layer stays cached until go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gitops ./cmd/gitops

FROM alpine:3.20 AS kubectl
ARG KUBECTL_VERSION=v1.35.9
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