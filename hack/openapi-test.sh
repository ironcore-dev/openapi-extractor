#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
SAMPLE_DIR=sample

echo "Build openapi-extractor:"
go build -o "$SAMPLE_DIR/bin/openapi-extractor" "$SCRIPT_DIR/../cmd/openapi-extractor"

echo "Find api services:"
cd "$SAMPLE_DIR" && go mod download
IRONCORE_PATH=$(go list -f '{{.Dir}}' -m github.com/ironcore-dev/ironcore)

echo "Extract openapi specs from api-server:"
./bin/openapi-extractor --apiserver-package=github.com/ironcore-dev/ironcore/cmd/ironcore-apiserver \
  --apiserver-build-opts=mod \
  --apiservices="$IRONCORE_PATH/config/apiserver/apiservice/bases"

echo "Ensure api specs where extracted:"
[ -f ./swagger.json ] || exit 1
[ -d ./v3 ] || exit 1
echo "Listing generated files in v3:"
ls -l ./v3/
for json_file in ./v3/*.json; do
  [ -s "$json_file" ] || exit 1
done
