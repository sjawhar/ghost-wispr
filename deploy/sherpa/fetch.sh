#!/usr/bin/env bash
# Fetches what a build with the paused listener (go build -tags kws) links and embeds: sherpa-onnx's
# static libraries for linux/GOARCH and its GigaSpeech keyword-spotting model. Each download is
# checked against its pinned sha256 before it is unpacked into gitignored paths:
#   third_party/sherpa-onnx/linux_<goarch>/lib/*.a
#   internal/wake/local/model/{encoder.int8.onnx,decoder.onnx,joiner.int8.onnx,tokens.txt,README.md}
# Downloads are kept in SHERPA_CACHE (default ~/.cache/ghost-wispr-sherpa), so a second run fetches
# nothing. A build without -tags kws needs none of this: it has no listener.
#
# Usage: deploy/sherpa/fetch.sh [amd64|arm64]   (default: go env GOARCH)
set -euo pipefail

readonly version=v2.0.1
readonly release_url=https://github.com/k2-fsa/sherpa-onnx/releases/download
readonly model=sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01
readonly model_sha256=f170013b4716e41b62b9bfd809687c207cef798ef9bc6534d524e17af9b6561a

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
goarch=${1:-$(go env GOARCH)}
case $goarch in
  amd64)
    libs=sherpa-onnx-$version-linux-x64-static-lib
    libs_sha256=55cfadf65ba50d0afc1a802b7a337942819e51c94a5b5a384def1978f7c70871
    ;;
  arm64)
    libs=sherpa-onnx-$version-linux-aarch64-static-lib
    libs_sha256=b6c6090e40fd18d6812f04fe21260602cd1223d7b5e83ad39dab87ad7c1c031d
    ;;
  *)
    printf 'fetch.sh: no sherpa-onnx static libraries for GOARCH %s\n' "$goarch" >&2
    exit 1
    ;;
esac
readonly goarch libs libs_sha256
cache=${SHERPA_CACHE:-$HOME/.cache/ghost-wispr-sherpa}
readonly cache
mkdir -p "$cache"

# fetch downloads url into the cache as name, unless the cache holds it with sum, and fails unless
# what it holds then has sum.
fetch() {
  local url=$1 name=$2 sum=$3
  local path=$cache/$name
  if [[ -f $path ]] && sha256sum --check --status <<<"$sum  $path"; then
    return
  fi
  rm -f -- "$path"
  curl --fail --silent --show-error --location --retry 3 --connect-timeout 10 --max-time 600 \
    --output "$path.partial" "$url"
  if ! sha256sum --check --status <<<"$sum  $path.partial"; then
    rm -f -- "$path.partial"
    printf 'fetch.sh: %s does not have the pinned sha256 %s\n' "$url" "$sum" >&2
    exit 1
  fi
  mv -- "$path.partial" "$path"
}

fetch "$release_url/$version/$libs.tar.bz2" "$libs.tar.bz2" "$libs_sha256"
fetch "$release_url/kws-models/$model.tar.bz2" "$model.tar.bz2" "$model_sha256"

lib_dir=$repo_root/third_party/sherpa-onnx/linux_$goarch/lib
rm -rf -- "$lib_dir"
mkdir -p "$lib_dir"
tar -xjf "$cache/$libs.tar.bz2" -C "$lib_dir" --strip-components=2 --wildcards "$libs/lib/*.a"

model_dir=$repo_root/internal/wake/local/model
rm -rf -- "$model_dir"
mkdir -p "$model_dir"
unpack=$(mktemp -d)
trap 'rm -rf -- "$unpack"' EXIT
tar -xjf "$cache/$model.tar.bz2" -C "$unpack" \
  "$model/encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx" \
  "$model/decoder-epoch-12-avg-2-chunk-16-left-64.onnx" \
  "$model/joiner-epoch-12-avg-2-chunk-16-left-64.int8.onnx" \
  "$model/tokens.txt" \
  "$model/README.md"
mv -- "$unpack/$model/encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx" "$model_dir/encoder.int8.onnx"
mv -- "$unpack/$model/decoder-epoch-12-avg-2-chunk-16-left-64.onnx" "$model_dir/decoder.onnx"
mv -- "$unpack/$model/joiner-epoch-12-avg-2-chunk-16-left-64.int8.onnx" "$model_dir/joiner.int8.onnx"
mv -- "$unpack/$model/tokens.txt" "$model_dir/tokens.txt"
mv -- "$unpack/$model/README.md" "$model_dir/README.md"

printf 'fetch.sh: sherpa-onnx %s static libraries for linux/%s in %s, and %s in %s\n' \
  "$version" "$goarch" "${lib_dir#"$repo_root"/}" "$model" "${model_dir#"$repo_root"/}"
