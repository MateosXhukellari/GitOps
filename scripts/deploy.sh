#!/usr/bin/env bash
# Deploy the controller image for a commit: set the tag in deploy/gitops.yaml,
# apply it, and wait for the rollout.
#
#   scripts/deploy.sh              # the image CI built for the current commit
#   scripts/deploy.sh sha-1a2b3c4  # a specific tag (for example, to roll back)
set -euo pipefail

IMAGE="mateosxh/gitops"
FILE="deploy/gitops.yaml"
NS="gitops-system"

cd "$(git rev-parse --show-toplevel)"

# CI tags images sha-<first 7 characters of the commit>.
TAG="${1:-sha-$(git rev-parse HEAD | cut -c1-7)}"

# Refuse to deploy an image that was never built (CI not finished, or failed).
code=$(curl -s -o /dev/null -w '%{http_code}' \
  "https://hub.docker.com/v2/repositories/${IMAGE}/tags/${TAG}")
if [ "$code" != "200" ]; then
  echo "error: ${IMAGE}:${TAG} is not on Docker Hub (HTTP ${code}). Did the CI run finish?" >&2
  exit 1
fi

# Replace only the image line for this repository.
sed -i "s#image: ${IMAGE}:.*#image: ${IMAGE}:${TAG}#" "$FILE"
grep -q "image: ${IMAGE}:${TAG}\$" "$FILE" || { echo "error: could not set the image in $FILE" >&2; exit 1; }

kubectl apply -f "$FILE"
kubectl -n "$NS" rollout status deploy/gitops --timeout=120s

echo "deployed ${IMAGE}:${TAG}"
echo "remember to commit $FILE so git records what is running"