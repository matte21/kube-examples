apiVersion: apps/v1
kind: ReplicaSet
metadata:
  name: ipam-controller
  namespace: example-com
spec:
  replicas: 1
  selector:
    matchLabels:
      ipam-controller: "true"
  template:
    metadata:
      labels:
        ipam-controller: "true"
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9295"
    spec:
      serviceAccountName: ipam-controller
      containers:
      - name: ipam-controller
        image: DOCKER_PREFIX/kos-ipam-controller:latest
        imagePullPolicy: Always
        command:
        - /ipam-controller
        - -v=5
