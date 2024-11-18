# redis-operator
redis-operator는 Golang 기반의 Operator-SDK를 이용하여 만든, Redis Cluster를 관리하는 Kubernetes Operator입니다. 

redis-operator를 사용하면 Redis 클러스터를 손쉽게 생성, 삭제, 확장 및 축소, 모니터링할 수 있습니다.

## How It Works

### Kubernetes Operator Pattern ?

Kubernetes Operator는 Kubernetes의 확장 가능한 API를 활용하여 CRD의 명세를 정의하고, 해당 리소스를 관리하는 패턴입니다. Operator는 CRD에 정의된 리소스를 생성하고 관리하며, 필요한 작업을 수행하여 원하는 상태를 유지합니다.

redis-operator는 Redis Cluster의 생성, 삭제, 확장/축소, 모니터링과 같은 작업을 자동화합니다. Operator는 Redis 클러스터의 상태를 지속적으로 감시하고, 사용자가 정의한 원하는 상태에 맞게 클러스터를 조정합니다. 

![works](assets/works-img.png)

### <a name="crd">Redis Cluster CRD</a>

Redis Cluster의 CRD의 Parameter는 아래와 같이 구성되어 있습니다.
```yaml
apiVersion: redis.redis/v1beta1
kind: RedisCluster
metadata:
  name: rediscluster1
  namespace: cache
spec:
  image: awbrg789/redis:latest
  masters: 3
  replicas: 1
  basePort: 10000
  maxMemory: 500mb
  resources:
    limits:
      cpu: 500m
      memory: 500Mi
    requests:
      cpu: 100m
      memory: 100Mi
  exporterResources:
    limits:
      cpu: 100m
      memory: 100Mi
    requests:
      cpu: 100m
      memory: 100Mi
```

| Parameter | Description | 
| --- | --- | 
| image | Redis 이미지 이름 | 
| masters | Redis Master Node 수 | 
| replicas | Redis Master Node 당 Replica 수 | 
| basePort | Redis Node의 시작 Port 번호 |
| maxMemory | Redis Node의 Max Memory 설정 |
| resources | Redis Node Container의 리소스 설정 | 
| exporterResources | Redis Node Exporter Container의 리소스 설정 | 


## Getting Started

```bash
# Add Helm Repo
$ helm repo add operator https://jaehanbyun.github.io/redis-operator
```

```bash
# Deploy Redis Operator
$ helm upgrade redis-operator operator/redis-operator \
  --install --create-namespace --namespace <your_namespace> 
```

redis-operator가 helm으로 배포되고 나면, <a href="crd">Redis Cluster CRD</a>의 yaml 형식의 manifest 파일을 사용하여 Redis Cluster를 배포할 수 있습니다.

또한, 기본적으로, Helm 배포 시 Prometheus와 Grafana를 Sub Chart로 Enable되어 operator와 함께 배포됩니다.

## Demo Video

