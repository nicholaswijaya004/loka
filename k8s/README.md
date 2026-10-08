# Loka on Kubernetes (kind)

The API on a local [kind](https://kind.sigs.k8s.io) cluster with a CPU
HorizontalPodAutoscaler. Postgres, Redis and Jaeger stay in docker compose;
pods reach them through `host.docker.internal`.

```bash
docker compose up -d postgres redis jaeger
kind create cluster --name loka

# metrics-server: the HPA's CPU numbers (kind needs --kubelet-insecure-tls)
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.8.0/components.yaml
kubectl -n kube-system patch deployment metrics-server --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'

# the image (built by docker-compose.replicas.yml) into the cluster
docker compose -f docker-compose.yml -f docker-compose.replicas.yml build api
kind load docker-image loka-api:dev --name loka

kubectl apply -f k8s/api.yaml -f k8s/hpa.yaml

# load from inside the cluster, so kube-proxy spreads it over the pods
kubectl create configmap loka-k6 --from-file=scripts/k6/hpa.js
kubectl apply -f k8s/load-job.yaml
kubectl get hpa loka-api -w
