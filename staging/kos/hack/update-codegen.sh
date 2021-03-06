#!/bin/bash

# Copyright 2018 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT=$(dirname ${BASH_SOURCE})/..
CODEGEN_PKG=${CODEGEN_PKG:-$(cd ${SCRIPT_ROOT}; ls -d -1 ./vendor/k8s.io/code-generator 2>/dev/null || echo ../code-generator)}

# generate the code with:
# --output-base    because this script should also be able to run inside the vendor dir of
#                  k8s.io/kubernetes. The output-base is needed for the generators to output into the vendor dir
#                  instead of the $GOPATH directly. For normal projects this can be dropped.
${CODEGEN_PKG}/generate-internal-groups.sh all \
  k8s.io/examples/staging/kos/pkg/client k8s.io/examples/staging/kos/pkg/apis k8s.io/examples/staging/kos/pkg/apis \
  network:v1alpha1 \
  --output-base "$(dirname ${BASH_SOURCE})/../../../../.." \
  --go-header-file ${SCRIPT_ROOT}/hack/boilerplate.go.txt

openapi-gen --logtostderr \
  -i k8s.io/examples/staging/kos/pkg/apis/network/v1alpha1,k8s.io/apimachinery/pkg/apis/meta/v1,k8s.io/apimachinery/pkg/version,k8s.io/apimachinery/pkg/runtime \
  -p k8s.io/examples/staging/kos/pkg/generated/openapi \
  -h ${SCRIPT_ROOT}/hack/boilerplate.go.txt \
  -O zz_generated.openapi -r report

# To use your own boilerplate text use:
#   --go-header-file ${SCRIPT_ROOT}/hack/custom-boilerplate.go.txt
