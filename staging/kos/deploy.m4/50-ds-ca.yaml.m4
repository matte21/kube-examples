apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: connection-agent
  namespace: example-com
spec:
  selector:
    matchLabels:
      connection-agent: "true"
  template:
    metadata:
      labels:
        connection-agent: "true"
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9294"
    spec:
      serviceAccountName: connection-agent
      containers:
      - name: connection-agent
        image: DOCKER_PREFIX/kos-connection-agent:latest
        imagePullPolicy: Always
        env:
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: HOST_IP
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        command:
        - /connection-agent
        - -v=5
        - -nodename=$(NODE_NAME)
        - -hostip=$(HOST_IP)
        - -netfabric=logger
        - -allowed-programs=/usr/local/kos/bin/TestByPing,/usr/local/kos/bin/RemoveNetNS
