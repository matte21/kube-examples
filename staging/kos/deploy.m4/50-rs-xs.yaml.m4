apiVersion: apps/v1
kind: ReplicaSet
metadata:
  name: network-apiserver
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
      - name: apiserver
        image: DOCKER_PREFIX/kos-network-apiserver:latest
        imagePullPolicy: Always
        command:
        - /network-apiserver
        - --etcd-servers=http://localhost:2379
        - -v=5
      - name: etcd
        image: quay.io/coreos/etcd:v3.3.9
