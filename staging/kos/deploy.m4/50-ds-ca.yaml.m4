apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ca-controller
  namespace: example-com
spec:
  selector:
    matchLabels:
      ca-controller: "true"
  template:
    metadata:
      labels:
        ca-controller: "true"
    spec:
      serviceAccountName: ca-controller
      hostNetwork: true
      containers:
      - name: ca-controller
        image: DOCKER_PREFIX/kos-ca-controller:latest
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
        - /ca-controller
        - -v=5
        - -nodename=$(NODE_NAME)
        - -hostip=$(HOST_IP)
        - -netfabric="logger"
