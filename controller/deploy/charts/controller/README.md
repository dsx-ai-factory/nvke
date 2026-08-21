# controller

![Version: 2.9.2](https://img.shields.io/badge/Version-2.9.2-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.0.490](https://img.shields.io/badge/AppVersion-v0.0.490-informational?style=flat-square)

A Helm chart to deploy the cloud-workflow-engine service

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| additionalInitContainers | list | `[]` | Additional init containers on the output Deployment definition, rendered ahead of the app container. |
| affinity | object | `{}` |  |
| appDb.dbName | string | `"cloud-workflow"` |  |
| appDb.postgresPassword | string | `"postgres"` |  |
| appDb.postgresUser | string | `"postgres"` |  |
| appDb.url | string | `"postgresql:5432"` |  |
| autoscaling.enabled | bool | `false` |  |
| autoscaling.maxReplicas | int | `100` |  |
| autoscaling.minReplicas | int | `1` |  |
| autoscaling.targetCPUUtilizationPercentage | int | `80` |  |
| capi.enabled | bool | `false` | Enable Cluster API controllers in CWE. |
| capi.forgeCapiEnabled | bool | `false` | Enable the Forge-CAPI bridge controllers. |
| capi.rbac.create | bool | `false` | Create RBAC for CWE to watch and patch CAPI/NICo resources. |
| cni.imageRegistry | string | `""` | Optional OCI registry prefix for CNI component images. Empty uses chart defaults. |
| cni.installAfterInit | bool | `false` | Install CNI after kubeadm init through the agent's InstallCNI RPC, which reads manifests from a Debian package rather than a bundle server. Needs the CNI charts package on the node that runs kubeadm init: nke-cni-ovn-charts-<stream> for OVN, or nke-cni-calico-manifests-<stream> for Calico. |
| env | list | `[]` | Additional environment variables to inject into the CWE container. |
| fullnameOverride | string | `""` |  |
| image.pullPolicy | string | `"IfNotPresent"` | This sets the pull policy for images. |
| image.repository | string | `"ghcr.io/nvidia/nke/controller"` |  |
| image.tag | string | `""` | Overrides the image tag whose default is the chart appVersion. |
| imagePullSecrets | list | `[]` | Must match image.repository, which defaults to ghcr.io. The NVCR-published chart gets the NGC secret put back at package time. Supply your own for a private GHCR package. |
| ingress.annotations | object | `{}` |  |
| ingress.className | string | `""` |  |
| ingress.enabled | bool | `false` |  |
| ingress.hosts[0].host | string | `"chart-example.local"` |  |
| ingress.hosts[0].paths[0].path | string | `"/"` |  |
| ingress.hosts[0].paths[0].pathType | string | `"ImplementationSpecific"` |  |
| ingress.tls | list | `[]` |  |
| kubeadm.imageRepository | string | `"registry.k8s.io"` | OCI registry kubeadm uses for control-plane component images. |
| livenessProbe | object | `{}` |  |
| metrics.listenAddress | string | `"0.0.0.0:9090"` | listenAddress controls which interface the metrics server binds. Set to 0.0.0.0:<port> when a NetworkPolicy is in place to restrict access. |
| metrics.networkPolicy.additionalNamespaces | list | `[]` | additionalNamespaces allows extra namespaces to scrape the metrics port. |
| metrics.networkPolicy.enabled | bool | `true` | enabled controls whether a NetworkPolicy is created to restrict metrics port access to the Prometheus and Alloy scraper namespaces. |
| metrics.port | int | `9090` | Port for the dedicated Prometheus metrics server. Must match METRICS_LISTEN_ADDRESS in the container env. |
| tracing.enabled | bool | `false` | Enable OpenTelemetry tracing in the CWE binary (TracerProvider + transport hooks). Keep false in chart defaults; enable per environment against an OTLP collector. |
| tracing.operatorInject.enabled | bool | `false` | When true (with tracing.enabled), add OpenTelemetry Operator pod annotations (`inject-sdk` / sidecar inject). Prefer `inject-sdk` over `inject-go` when using the manual Go SDK. |
| tracing.operatorInject.injectSdk | string | `""` | Value for `instrumentation.opentelemetry.io/inject-sdk` (namespace/name). Set per environment. |
| tracing.operatorInject.injectSidecar | string | `""` | Value for `sidecar.opentelemetry.io/inject` (namespace/name). Set per environment. |
| tracing.endpoint | string | `""` | Optional OTLP gRPC endpoint override (host:port). Leave empty when operatorInject.enabled is true so the operator-injected localhost sidecar endpoint is used. |
| tracing.insecure | bool | `true` | Use insecure (plaintext) OTLP gRPC. Required for localhost sidecar / most in-cluster collectors. |
| tracing.serviceName | string | `"cloud-workflow-engine"` | Resource attribute service.name when chart sets OTEL env (non-operatorInject or endpoint override). |
| nameOverride | string | `""` |  |
| namespaceOverride | string | `""` | Deploy into this namespace instead of the release namespace (umbrella use). |
| nodeSelector | object | `{}` |  |
| nkeOIDCAuth.caBundleSecret.key | string | `""` | Key in the Secret containing the OIDC CA bundle. |
| nkeOIDCAuth.caBundleSecret.name | string | `""` | Name of the Secret containing the OIDC CA bundle. |
| nkeOIDCAuth.caBundleSecret.namespace | string | `""` | Namespace of the Secret containing the OIDC CA bundle. |
| nkeOIDCAuth.discoveryURL | string | `""` | Optional discovery URL override used when issuerURL is not directly reachable by tenant kube-apiservers. |
| nkeOIDCAuth.enabled | bool | `false` | Enable OIDC authentication from NKE management identities to tenant cluster kube-apiservers. |
| nodeTeardown.allowEtcdSafetyOverrides | bool | `false` | Allow the DeleteNode API to accept the dangerous etcd safety overrides (allowQuorumLoss / allowSingleVoter). Disabled by default so production site controllers reject them; enable on development site controllers that need manual single-node control-plane rotation. |
| nkeOIDCAuth.issuerURL | string | `""` | OIDC issuer URL trusted by tenant kube-apiservers. |
| deploymentAnnotations | object | `{}` | Annotations on the Deployment `metadata` (evaluated with `tpl`; when the Forge Secret is enabled, the chart adds a default Reloader annotation unless one is provided) |
| podAnnotations | object | `{}` |  |
| podLabels.app | string | `"cloud-workflow-engine"` |  |
| podLabels.project | string | `"nke"` |  |
| podLabels.team | string | `"nke"` |  |
| podSecurityContext.runAsNonRoot | bool | `true` |  |
| profiles.path | string | `""` | Directory mounted/read as PROFILES_DIR |
| profiles.image.enabled | bool | `false` | Copy `/profiles` from a content image into profiles.path |
| profiles.image.repository | string | `"nvcr.io/j7sbcjl3qgta/cwe-profiles"` | Profiles content image repository |
| profiles.image.tag | string | `""` | Image tag (used when digest is empty) |
| profiles.image.digest | string | `""` | Image digest pin (preferred; e.g. `sha256:…`) |
| readinessProbe | object | `{}` |  |
| replicaCount | int | `1` |  |
| resources | object | `{}` |  |
| secrets.createDbSecret | bool | `true` | Create the DB connection Secret from appDb values. Set false when the connection Secret is managed externally, e.g. by Vault Secrets Operator. |
| secrets.dbSecretKey | string | `""` | secretKey with db connectionString. If not specified, defaults to "database-connection-string" |
| secrets.dbSecretName | string | `""` | If specified, secret is created with the name provided. otherwise defaults to name based on fullname template "<fulName>-db" |
| secrets.forgeSecret.enabled | bool | `true` | Read FWE_API_KEY from a Kubernetes Secret. |
| secrets.cnpgDbPasswordsSecretName | string | `""` | Name of the Kubernetes Secret providing CNPG database passwords (CNPG_SUPERUSER_PASSWORD, CNPG_CLOUD_PASSWORD, CNPG_TEMPORAL_PASSWORD). When empty, CNPG_* env vars are not injected. When set, all three keys are required to exist in the Secret. |
| secrets.forgeSecretKey | string | `""` | secretKey with forge-key. If not specified, will defaults to "forge-api-key" |
| secrets.forgeSecretName | string | `""` | secret with forge-creds. If not specified, will default to secret with "<fulName>-forge-creds" |
| securityContext.readOnlyRootFilesystem | bool | `true` |  |
| service.annotations | object | `{}` | Additional annotations for the service |
| service.labels | object | `{}` | Additional labels for the service |
| service.port | int | `8080` | apiserver port |
| service.type | string | `"ClusterIP"` |  |
| serviceAccount.annotations | object | `{}` |  |
| serviceAccount.automount | bool | `true` |  |
| serviceAccount.create | bool | `true` |  |
| serviceAccount.name | string | `"cloud-workflow-engine"` | The name of the service account to use. If not set and create is true, a name is generated using the fullname template |
| serviceMonitor.enabled | bool | `true` | Enable/disable ServiceMonitor creation |
| serviceMonitor.labels | object | `{}` | Additional labels for the ServiceMonitor |
| serviceMonitor.matchLabels | object | `{"app.kubernetes.io/name":"cloud-workflow-engine"}` | Selector labels to match the service |
| serviceMonitor.name | string | `"cloud-workflow-engine-metrics"` | Name of the ServiceMonitor |
| scheduledMaintenance.certRenewal.enabled | bool | `true` | Enable CWE-owned periodic Kubernetes control-plane certificate renewal. |
| scheduledMaintenance.certRenewal.interval | string | `"12h"` | Interval between certificate renewal checks for each cluster. |
| scheduledMaintenance.certRenewal.thresholdDays | int | `30` | Days before expiry that the runbook renews certificates. |
| spiffe.enabled | bool | `false` | Enable SPIFFE identity for mTLS to tenant clusters |
| spiffe.endpointSocket | string | `"unix:///spiffe-workload-api/spire-agent.sock"` |  |
| spiffe.spire.className | string | `"spire-mgmt-spire"` |  |
| spiffe.spire.joinTokenTTL | string | `"1h"` | TTL used when creating SPIRE join tokens through the SPIRE server API. |
| spiffe.spire.serverAddress | string | `""` | SPIRE server address. When unset, serverAddressFrom is used. |
| spiffe.spire.serverAddressFrom | object | `{"fieldRef":{"fieldPath":"status.hostIP"}}` | Kubernetes valueFrom source for SPIRE server address when serverAddress is unset. |
| spiffe.spire.serverNamespace | string | `"spire-server"` |  |
| spiffe.spire.serverPort | string | `"30440"` |  |
| spiffe.spire.tpmPubhashesConfigMapName | string | `"tpm-pubhashes"` |  |
| teleport.enabled | bool | `false` | Enable Teleport identity for CWE to manage Teleport resources (tokens, nodes) |
| teleport.environment | string | `"non-prod"` | Teleport environment label: "prod" or "non-prod" |
| teleport.identityFile | string | `"/var/run/teleport/identity"` | Path where the tbot identity file is mounted |
| teleport.identitySecretName | string | `"cloud-workflow-engine-teleport"` | Name of the Kubernetes secret containing the tbot identity (written by tbot output) |
| teleport.proxyAddress | string | `""` | Teleport proxy address (site-specific, must be set per environment) |
| temporal.cloudWorkflow.namespace | string | `"cloud-workflow-engine"` |  |
| temporal.cloudWorkflow.queue | string | `"cloud-workflow-engine-tasks"` |  |
| temporal.host | string | `"temporal-frontend.temporal.svc.cluster.local"` |  |
| temporal.mtls.enabled | bool | `false` | Enable mTLS when connecting to Temporal. |
| temporal.mtls.secretName | string | `""` | Secret containing ca.crt, tls.crt, and tls.key for the Temporal client. |
| temporal.mtls.mountPath | string | `"/var/secrets/temporal/certs/client"` | Mount path for the Temporal client certificate secret. |
| temporal.port | int | `7233` |  |
| temporal.publicTemporalHostPort | string | `""` |  |
| tenantAccess.enabled | bool | `true` | Publish each cluster's kubeconfig as a Secret in the nke-system namespace, and remove those Secrets during cluster teardown. Gates both the controller and its cleanup activity, and the Secret grant they depend on. |
| tolerations | list | `[]` |  |
| volumeMounts | list | `[]` |  |
| volumes | list | `[]` |  |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
