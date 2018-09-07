apiVersion: apps/v1
kind: ReplicaSet
metadata:
  name: network-server
  namespace: example-com
spec:
  replicas: 1
  selector:
    matchLabels:
      network-apiserver: "true"
  template:
    metadata:
      labels:
        network-apiserver: "true"
    spec:
      serviceAccountName: network-apiserver
      containers:
      - name: network-server
        image: DOCKER_PREFIX/kos-network-apiserver:latest
        imagePullPolicy: IfNotPresent
        command:
        - /network-apiserver
        - --etcd-servers=http://localhost:2379
        - -v=5
        - --enable-admission-plugins=CheckSubnets
      - name: etcd
        image: quay.io/coreos/etcd:v3.3.9
