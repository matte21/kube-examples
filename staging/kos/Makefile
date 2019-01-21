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

deploy/50-rs-xs.yaml: deploy.m4/50-rs-xs.yaml.m4
	m4 -DDOCKER_PREFIX=${DOCKER_PREFIX} deploy.m4/50-rs-xs.yaml.m4 > deploy/50-rs-xs.yaml

deploy/50-rs-ipam.yaml: deploy.m4/50-rs-ipam.yaml.m4
	m4 -DDOCKER_PREFIX=${DOCKER_PREFIX} deploy.m4/50-rs-ipam.yaml.m4 > deploy/50-rs-ipam.yaml

deploy/50-ds-ca.yaml: deploy.m4/50-ds-ca.yaml.m4
	m4 -DDOCKER_PREFIX=${DOCKER_PREFIX} deploy.m4/50-ds-ca.yaml.m4 > deploy/50-ds-ca.yaml

.PHONY: deploy
deploy: deploy/50-rs-xs.yaml deploy/50-rs-ipam.yaml deploy/50-ds-ca.yaml
	kubectl apply -f deploy

.PHONY: undeploy
undeploy: deploy/50-rs-xs.yaml deploy/50-rs-ipam.yaml deploy/50-ds-ca.yaml
	kubectl delete -f deploy
