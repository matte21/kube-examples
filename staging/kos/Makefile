DOCKER_PREFIX=${LOGNAME}

publish:: publish-network-apiserver
publish:: publish-ipam-controller
publish:: publish-ca-controller

build:: build-network-apiserver
build:: build-ipam-controller
build:: build-ca-controller

clean:
	rm -f images/*/*r
	rm -f images/*/*/*r

build-network-apiserver:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o images/network-apiserver/network-apiserver k8s.io/examples/staging/kos/cmd/network-apiserver

build-ipam-controller:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o images/controllers/ipam/ipam-controller k8s.io/examples/staging/kos/cmd/controllers/ipam

build-ca-controller:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o images/controllers/connectionagent/ca-controller k8s.io/examples/staging/kos/cmd/controllers/connectionagent

publish-network-apiserver:
	m4 -DDOCKER_PREFIX=${DOCKER_PREFIX} deploy.m4/50-rs-xs.yaml.m4 > deploy/50-rs-xs.yaml
	cd images/network-apiserver && docker build -t ${DOCKER_PREFIX}/kos-network-apiserver:latest . && docker push ${DOCKER_PREFIX}/kos-network-apiserver:latest

publish-ipam-controller:
	m4 -DDOCKER_PREFIX=${DOCKER_PREFIX} deploy.m4/50-rs-ipam.yaml.m4 > deploy/50-rs-ipam.yaml
	cd images/controllers/ipam && docker build -t ${DOCKER_PREFIX}/kos-ipam-controller:latest . && docker push ${DOCKER_PREFIX}/kos-ipam-controller:latest

publish-ca-controller:
	m4 -DDOCKER_PREFIX=${DOCKER_PREFIX} deploy.m4/50-ds-ca.yaml.m4 > deploy/50-ds-ca.yaml
	cd images/controllers/connectionagent && docker build -t ${DOCKER_PREFIX}/kos-ca-controller:latest . && docker push ${DOCKER_PREFIX}/kos-ca-controller:latest

.PHONY: deploy
deploy:
	kubectl apply -f deploy

.PHONY: undeploy
undeploy:
	kubectl delete -f deploy
