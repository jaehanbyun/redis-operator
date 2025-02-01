# redis-operator

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.1](https://img.shields.io/badge/AppVersion-0.1.1-informational?style=flat-square)

A Helm chart for redis operator

**Homepage:** <https://github.com/jaehanbyun/redis-operator>

## Source Code

* <https://github.com/jaehanbyun/redis-operator>

## Requirements

| Repository | Name | Version |
|------------|------|---------|
| https://grafana.github.io/helm-charts | grafana | 8.5.* |
| https://prometheus-community.github.io/helm-charts | prometheus | 25.29.* |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| grafana.adminPassword | string | `"prom-operator"` |  |
| grafana.enabled | bool | `true` |  |
| grafana.sidecar.dashboards.enabled | bool | `true` |  |
| grafana.sidecar.datasources.enabled | bool | `true` |  |
| prometheus.alertmanager | object | `{}` |  |
| prometheus.enabled | bool | `true` |  |
| prometheus.kube-state-metrics.enabled | bool | `false` |  |
| prometheus.prometheus-node-exporter.enabled | bool | `false` |  |
| prometheus.prometheus-pushgateway.enabled | bool | `false` |  |
| prometheus.server.global.evaluation_interval | string | `"30s"` |  |
| prometheus.server.global.scrape_interval | string | `"5s"` |  |
| prometheus.server.global.scrape_timeout | string | `"5s"` |  |
| redisOperator.affinity | object | `{}` |  |
| redisOperator.imageName | string | `"awbrg789/redis-operator"` |  |
| redisOperator.imagePullPolicy | string | `"Always"` |  |
| redisOperator.imagePullSecrets | list | `[]` |  |
| redisOperator.imageTag | string | `""` |  |
| redisOperator.maxConcurrentReconciles | int | `5` |  |
| redisOperator.name | string | `"redis-operator"` |  |
| redisOperator.nodeSelector | object | `{}` |  |
| redisOperator.podAnnotations | object | `{}` |  |
| redisOperator.podLabels | object | `{}` |  |
| redisOperator.podSecurityContext | string | `nil` |  |
| redisOperator.reconcileInterval | string | `""` |  |
| redisOperator.replicaCount | int | `1` |  |
| redisOperator.resources.limits.cpu | string | `"500m"` |  |
| redisOperator.resources.limits.memory | string | `"500Mi"` |  |
| redisOperator.resources.requests.cpu | string | `"500m"` |  |
| redisOperator.resources.requests.memory | string | `"500Mi"` |  |
| redisOperator.securityContext | string | `nil` |  |
| redisOperator.service.type | string | `"ClusterIP"` |  |
| redisOperator.tolerations | list | `[]` |  |

