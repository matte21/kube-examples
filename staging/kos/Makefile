DOCKER_PREFIX=${LOGNAME}

publish:: publish-network-apiserver
publish:: publish-ipam-controller
publish:: publish-connection-agent

build:: build-network-apiserver
build:: build-ipam-controller
build:: build-connection-agent
build:: build-attachment-tput-driver

clean:
	rm -f images/*/*r
	rm -f images/*/*/*[rt]
	rm -rf local-binaries
	rm -f deploy/main/50-*

build-network-apiserver:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o images/network-apiserver/network-apiserver k8s.io/examples/staging/kos/cmd/network-apiserver

build-ipam-controller:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o images/controllers/ipam/ipam-controller k8s.io/examples/staging/kos/cmd/controllers/ipam

build-connection-agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o images/controllers/connectionagent/connection-agent k8s.io/examples/staging/kos/cmd/controllers/connectionagent

build-attachment-tput-driver:
	go build -a -o local-binaries/attachment-tput-driver k8s.io/examples/staging/kos/cmd/attachment-tput-driver

publish-network-apiserver:
	cd images/network-apiserver && docker build -t ${DOCKER_PREFIX}/kos-network-apiserver:latest . && docker push ${DOCKER_PREFIX}/kos-network-apiserver:latest

publish-ipam-controller:
	cd images/controllers/ipam && docker build -t ${DOCKER_PREFIX}/kos-ipam-controller:latest . && docker push ${DOCKER_PREFIX}/kos-ipam-controller:latest

publish-connection-agent:
	cp -R cmd/attachment-tput-driver/test-scripts images/controllers/connectionagent
	cd images/controllers/connectionagent && docker build -t ${DOCKER_PREFIX}/kos-connection-agent:latest . && docker push ${DOCKER_PREFIX}/kos-connection-agent:latest

deploy/main/50-rs-xs.yaml: deploy.m4/main/50-rs-xs.yaml.m4
	m4 -DDOCKER_PREFIX=${DOCKER_PREFIX} deploy.m4/main/50-rs-xs.yaml.m4 > deploy/main/50-rs-xs.yaml

deploy/main/50-rs-ipam.yaml: deploy.m4/main/50-rs-ipam.yaml.m4
	m4 -DDOCKER_PREFIX=${DOCKER_PREFIX} deploy.m4/main/50-rs-ipam.yaml.m4 > deploy/main/50-rs-ipam.yaml

deploy/main/50-ds-ca.yaml: deploy.m4/main/50-ds-ca.yaml.m4
	m4 -DDOCKER_PREFIX=${DOCKER_PREFIX} deploy.m4/main/50-ds-ca.yaml.m4 > deploy/main/50-ds-ca.yaml

.PHONY: deploy
deploy: deploy/main/50-rs-xs.yaml deploy/main/50-rs-ipam.yaml deploy/main/50-ds-ca.yaml
	kubectl apply -f deploy/ns && \
	kubectl apply -f deploy/etcd-operator-rbac && \
	kubectl apply -f deploy/etcd-operator && \
	while ! kubectl get EtcdCluster ; do sleep 5 ; done && \
	kubectl apply -f deploy/etcd-cluster && \
	kubectl apply -f deploy/main

.PHONY: undeploy
undeploy: deploy/main/50-rs-xs.yaml deploy/main/50-rs-ipam.yaml deploy/main/50-ds-ca.yaml
	kubectl delete --ignore-not-found -f deploy/main && \
	! kubectl get EtcdCluster || kubectl delete --ignore-not-found -f deploy/etcd-cluster && \
	while kubectl get EtcdCluster -n example-com the-etcd-cluster ; do sleep 15; done && \
	kubectl delete --ignore-not-found -f deploy/etcd-operator && \
	kubectl delete --ignore-not-found Endpoints etcd-operator && \
	kubectl delete --ignore-not-found crd etcdclusters.etcd.database.coreos.com && \
	kubectl delete --ignore-not-found -f deploy/etcd-operator-rbac && \
	kubectl delete --ignore-not-found -f deploy/ns

# The following just document how these files were originally made.
# FYI, this what when master pointed to commit aeb3e3e0835ec5135cfe50340f59853b5b6fc407

deploy/etcd-operator-rbac/47-eo-role.yaml:
	curl https://raw.githubusercontent.com/coreos/etcd-operator/master/example/rbac/cluster-role-template.yaml | sed -e "s/<ROLE_NAME>/kos-etcd-operator/g" > deploy/etcd-operator-rbac/47-eo-role.yaml

deploy/etcd-operator-rbac/47-eo-rolebind.yaml:
	curl https://raw.githubusercontent.com/coreos/etcd-operator/master/example/rbac/cluster-role-binding-template.yaml | sed -e "s/<ROLE_NAME>/kos-etcd-operator/g" -e "s/<ROLE_BINDING_NAME>/kos-etcd-operator/g" -e "s/<NAMESPACE>/example-com/g" > deploy/etcd-operator-rbac/47-eo-rolebind.yaml

