#!/usr/bin/env bash
set -euo pipefail

: "${K8S_VERSION:?}" "${KIND_VERSION:?}" "${HELM_VERSION:?}"
tools_dir=${LOCALBIN:?Run make install-tools to set LOCALBIN}

case "$(uname -s)" in
  Linux) tool_os=linux ;;
  Darwin) tool_os=darwin ;;
  *) echo "Unsupported operating system" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64) tool_arch=amd64 ;;
  arm64|aarch64) tool_arch=arm64 ;;
  *) echo "Unsupported architecture" >&2; exit 1 ;;
esac

# Official release download URLs, kept together for review.
readonly kubectl_url="https://dl.k8s.io/release/${K8S_VERSION}/bin/${tool_os}/${tool_arch}/kubectl"
readonly kubectl_checksum_url="${kubectl_url}.sha256"
readonly kind_url="https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-${tool_os}-${tool_arch}"
readonly kind_checksum_url="${kind_url}.sha256sum"
readonly helm_url="https://get.helm.sh/helm-${HELM_VERSION}-${tool_os}-${tool_arch}.tar.gz"
readonly helm_checksum_url="${helm_url}.sha256"

mkdir -p "$tools_dir"
download_dir=$(mktemp -d)
trap 'rm -rf "$download_dir"' EXIT

download_verified() {
  local url=$1 destination=$2 checksum_url=$3
  local archive="$download_dir/download" checksum="$download_dir/checksum"
  curl -fsSL --retry 3 "$url" -o "$archive"
  curl -fsSL --retry 3 "$checksum_url" -o "$checksum"
  local expected actual
  expected=$(awk '{print $1}' "$checksum")
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$archive" | awk '{print $1}')
  else
    actual=$(shasum -a 256 "$archive" | awk '{print $1}')
  fi
  if [[ "$actual" != "$expected" ]]; then
    echo "Checksum mismatch for $url" >&2
    exit 1
  fi
  mv "$archive" "$destination"
}

kubectl_file="kubectl-${K8S_VERSION}-${tool_os}-${tool_arch}"
if [[ ! -x "$tools_dir/$kubectl_file" ]]; then
  download_verified "$kubectl_url" "$tools_dir/$kubectl_file" "$kubectl_checksum_url"
  chmod +x "$tools_dir/$kubectl_file"
fi
ln -sf "$kubectl_file" "$tools_dir/kubectl"

kind_file="kind-${KIND_VERSION}-${tool_os}-${tool_arch}"
if [[ ! -x "$tools_dir/$kind_file" ]]; then
  download_verified "$kind_url" "$tools_dir/$kind_file" "$kind_checksum_url"
  chmod +x "$tools_dir/$kind_file"
fi
ln -sf "$kind_file" "$tools_dir/kind"

helm_file="helm-${HELM_VERSION}-${tool_os}-${tool_arch}"
if [[ ! -x "$tools_dir/$helm_file" ]]; then
  download_verified "$helm_url" "$download_dir/helm.tar.gz" "$helm_checksum_url"
  tar -xzf "$download_dir/helm.tar.gz" -C "$download_dir" "${tool_os}-${tool_arch}/helm"
  mv "$download_dir/${tool_os}-${tool_arch}/helm" "$tools_dir/$helm_file"
fi
ln -sf "$helm_file" "$tools_dir/helm"

"$tools_dir/kubectl" version --client
"$tools_dir/kind" version
"$tools_dir/helm" version --short
