apiVersion: apps/v1
kind: ReplicaSet
metadata:
  name: network-apiserver
  namespace: example-com
spec:
  replicas: 2
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
        - --etcd-servers=http://the-etcd-cluster-client:2379
        - -v=5
