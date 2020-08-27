# Copyright © 2020 The OpenEBS Authors
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

#!/usr/bin/env bash

set -ex

echo "Install openebs-operator 1.11.0"

kubectl apply -f ./ci/migrate/openebs-operator.yaml
sleep 5

echo "Wait for maya-apiserver to start"

kubectl wait --for=condition=available --timeout=600s deployment/maya-apiserver -n openebs

echo "Label the node"

kubectl label nodes --all nodetype=storage

echo "Create application with cStor volume on SPC"

bdname=$(kubectl -n openebs get blockdevices -o jsonpath='{.items[*].metadata.name}')
sed "s/SPCBD/$bdname/" ./ci/migrate/application.tmp.yaml > ./ci/migrate/application.yaml
kubectl apply -f ./ci/migrate/application.yaml
sleep 5
kubectl wait --for=condition=Ready pod -l lkey=lvalue --timeout=600s

echo "Install cstor & csi operators"

kubectl apply -f https://raw.githubusercontent.com/openebs/charts/gh-pages/cstor-operator.yaml
sleep 5
kubectl wait --for=condition=available --timeout=600s deployment/cspc-operator -n openebs