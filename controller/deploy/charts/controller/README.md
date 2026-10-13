# nke-controller

![Version: 0.0.0](https://img.shields.io/badge/Version-0.0.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.0](https://img.shields.io/badge/AppVersion-0.0.0-informational?style=flat-square)

A Helm chart to deploy the NVKE controller service

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| additionalInitContainers | list | `[]` | Additional init containers on the output Deployment definition, rendered ahead of the app container. |
| affinity | object | `{}` |  |
| apiAuth.clusterIssuerDiscovery | bool | `true` | Bind system:service-account-issuer-discovery so NVKE can read the cluster's JWKS. Only used by an issuer that points `jwks.uri` at that endpoint. Kubernetes already grants this to every service account, so a stock cluster needs nothing. Keep it for clusters that removed that default. |
| apiAuth.config | object | `{}` | The AuthenticationConfiguration itself, rendered verbatim into that ConfigMap. Carries `audiences` and `issuers` only: apiVersion and kind are supplied by the chart, which ships in lockstep with the build that accepts them.  Not validated here beyond both keys being present. internal/auth/config.go is a strict loader whose errors name the offending field, and a second copy of its rules in Helm would drift the first time an issuer gains a new key. The cost is that a bad value fails at pod start rather than at render.  config:   audiences: ["https://nke.example.internal"]   issuers:     - name: local-kubernetes       issuer: "https://kubernetes.default.svc.cluster.local"       jwks:         uri: "https://kubernetes.default.svc/openid/v1/jwks"         caBundlePath: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt         bearerTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token       algorithms: [RS256, ES256]       admit:         serviceAccounts:           - namespace: cloud-workflow-engine             name: forge-console |
| apiAuth.configMap.create | bool | `false` | Render `apiAuth.config` into the ConfigMap named below, rather than mounting one managed elsewhere. The two are alternatives: setting `config` without this, or this without `config`, fails the render rather than silently ignoring one of them. |
| apiAuth.configMap.extraFiles | object | `{}` | Extra files to place beside `auth.yaml`, keyed by filename. Only used when configMap.create=true. The whole ConfigMap is mounted, so each one lands in the same directory and can be named by a `jwks.caBundlePath` as /etc/nke/auth/<filename> -- which is how an issuer behind a private CA gets its bundle. A CA bundle is a public certificate, not a secret. The render fails if `caBundlePath` names a file in that directory that is not listed here, rather than leaving NVKE to crash-loop on a missing file.  extraFiles:   spire-ca.crt: \|     -----BEGIN CERTIFICATE-----     ...     -----END CERTIFICATE----- |
| apiAuth.configMap.name | string | `"nke-auth-config"` | ConfigMap holding the authentication configuration, keyed `auth.yaml`. Required when apiAuth.enabled=true; an empty name fails the render. Created by the chart when configMap.create=true, mounted either way. |
| apiAuth.enabled | bool | `false` | Enable NVKE API authentication. |
| apiAuth.permissive | bool | `false` | Count what enforcement would have refused but serve the request anyway. Watch nvke_auth_requests_total{outcome="would_reject"} before enforcing. |
| appDb.dbName | string | `"cloud-workflow"` |  |
| appDb.postgresPassword | string | `""` |  |
| appDb.postgresUser | string | `""` |  |
| appDb.sslMode | string | `""` | TLS mode for the DB connection string (e.g. disable, require, verify-full). |
| appDb.url | string | `"postgresql:5432"` |  |
| autoscaling.enabled | bool | `false` |  |
| autoscaling.maxReplicas | int | `100` |  |
| autoscaling.minReplicas | int | `1` |  |
| autoscaling.targetCPUUtilizationPercentage | int | `80` |  |
| capi.rbac.create | bool | `false` | Create RBAC for the controller to watch and patch CAPI/NICo resources. |
| cni.allowLegacyCalicoClusterCreation | bool | `false` | Permit new tenant clusters to use the legacy Calico CNI. Existing Calico clusters remain manageable when false. Legacy fleets that still create Calico clusters must set this before upgrading the controller. This compatibility setting is for legacy fleets only. New deployments must use OVN-Kubernetes. |
| cni.imageRegistry | string | `""` | Optional OCI registry prefix for CNI component images. Empty uses chart defaults. |
| cni.installAfterInit | bool | `false` | Install CNI after kubeadm init through the agent's InstallCNI RPC, which reads manifests from a Debian package rather than a bundle server. Needs the CNI charts package on the node that runs kubeadm init: nvke-cni-ovn-charts-<stream> for OVN, or nvke-cni-calico-manifests-<stream> for Calico. |
| controllers.nico.enabled | bool | `true` | Run controllers that watch NICo infrastructure resources. |
| deploymentAnnotations | object | `{}` | Annotations on the Deployment `metadata` (evaluated with `tpl`) |
| env | list | `[]` | Additional environment variables to inject into the controller container. |
| fullnameOverride | string | `""` |  |
| identity.fieldManagerMachineHealthIssueName | string | `"cloud-workflow-engine-machine-health-issue"` | Field manager used when patching a Machine's health-issue annotation. |
| identity.fieldManagerName | string | `"cloud-workflow-engine"` | Field manager for this controller's own CAPI/NICo Server-Side-Apply patches. |
| identity.fieldManagerRebootMachineName | string | `"cloud-workflow-engine-reboot-machine"` | Field manager scoped to the CAPI Machine reboot annotation alone. |
| identity.managedByLabelValue | string | `"cloud-workflow-engine"` | Value written under the nke.nvidia.com/managed-by label on objects this controller owns, and the value the tenant Secret reconciler filters on. |
| image.digest | string | `""` | Optional digest (sha256:...) for the nke-controller image. When set, the pod pulls by digest and the tag becomes informational only. Use this to pin an immutable image. |
| image.pullPolicy | string | `"IfNotPresent"` | This sets the pull policy for images. |
| image.repository | string | `"ghcr.io/dsx-ai-factory/nvke/nke-controller"` | Repository, including registry, for the nke-controller image (the controller service) that this chart deploys. Override to pull it from a different registry (e.g. a private mirror); pair any change with a matching `imagePullSecrets` entry. Not to be confused with `profiles.image.repository` below, a separate optional content image. |
| image.tag | string | `""` | Overrides the tag for the nke-controller image; default is the chart appVersion. |
| imagePullSecrets | list | `[]` | Must match image.repository, which defaults to ghcr.io. The NVCR-published chart gets the NGC secret put back at package time. Supply your own for a private GHCR package. |
| ingress.annotations | object | `{}` |  |
| ingress.className | string | `""` |  |
| ingress.enabled | bool | `false` |  |
| ingress.hosts[0].host | string | `"chart-example.local"` |  |
| ingress.hosts[0].paths[0].path | string | `"/"` |  |
| ingress.hosts[0].paths[0].pathType | string | `"ImplementationSpecific"` |  |
| ingress.tls | list | `[]` |  |
| kubeadm.imageRepository | string | `"registry.k8s.io"` | OCI registry kubeadm uses for control-plane component images. |
| livenessProbe | string | `nil` |  |
| metrics.listenAddress | string | `"0.0.0.0:9090"` | listenAddress controls which interface the metrics server binds. Set to 0.0.0.0:<port> when a NetworkPolicy is in place to restrict access. |
| metrics.networkPolicy.additionalNamespaces | list | `[]` | additionalNamespaces allows extra namespaces to scrape the metrics port. |
| metrics.networkPolicy.enabled | bool | `true` | enabled controls whether a NetworkPolicy is created to restrict metrics port access to the Prometheus and Alloy scraper namespaces. |
| metrics.port | int | `9090` | Port for the dedicated Prometheus metrics server. Must match the port in listenAddress. |
| nameOverride | string | `"cloud-workflow-engine"` |  |
| namespaceOverride | string | `""` | Deploy into this namespace instead of the release namespace (umbrella use). |
| nkeOIDCAuth.caBundleSecret.key | string | `""` | Key in the Secret containing the OIDC CA bundle. |
| nkeOIDCAuth.caBundleSecret.name | string | `""` | Name of the Secret containing the OIDC CA bundle. |
| nkeOIDCAuth.caBundleSecret.namespace | string | `""` | Namespace of the Secret containing the OIDC CA bundle. |
| nkeOIDCAuth.discoveryURL | string | `""` | Optional discovery URL override used when issuerURL is not directly reachable by tenant kube-apiservers. |
| nkeOIDCAuth.enabled | bool | `false` | Enable OIDC authentication from NVKE management identities to tenant cluster kube-apiservers. |
| nkeOIDCAuth.issuerURL | string | `""` | OIDC issuer URL trusted by tenant kube-apiservers. |
| nkeOIDCAuth.tenantRbacName | string | `"nvke:cwe"` | Name of the ClusterRole/Role/Binding objects the tenant-access controller applies into every tenant cluster (nke-rbac.yaml). |
| nkeSystemNamespace.annotations | object | `{}` | Extra annotations merged onto nke-system. This is useful for adding additional annotations to the namespace to protect it from being deleted. Listing annotations that template already defaults here duplicates the key in the rendered Namespace. |
| nkeSystemNamespace.create | bool | `true` | Create the shared nke-system namespace. The object always carries helm.sh/resource-policy: keep so a Helm uninstall does not delete it, and argocd.argoproj.io/sync-options: Prune=false,Delete=false so an Argo prune or Application delete does not either. Both are set by the template for default delete protection. |
| nodeAgent.dsxImds.endpoint | string | `"http://169.254.169.254"` | Metadata endpoint used by the external dsx_imds node attestor. The default is the standard link-local metadata service address. |
| nodeAgent.dsxImds.operatorTenant | string | `""` | Operator tenant encoded in dsx_imds-attested SPIRE agent IDs. This must match the DSX Identity global.operatorTenant value. |
| nodeAgent.dsxImds.pluginChecksum | string | `""` | Optional SHA-256 checksum for the external dsx_imds plugin binary. Leave empty to expand DSX_IMDS_PLUGIN_CHECKSUM from the node image's /etc/spire-agent/spire-agent.env file. |
| nodeAgent.externalMTLS.enabled | bool | `false` | Pin an externally managed client certificate transport for newly created nodes instead of the controller SPIRE X509-SVID. Keep the Secret available while nodes using this transport remain. |
| nodeAgent.externalMTLS.secret.caBundleKey | string | `"ca.crt"` | Secret key containing the PEM-encoded issuing CA bundle. |
| nodeAgent.externalMTLS.secret.certificateKey | string | `"tls.crt"` | Secret key containing the PEM-encoded client certificate chain. |
| nodeAgent.externalMTLS.secret.name | string | `""` | Name of the externally managed Secret containing the client identity. |
| nodeAgent.externalMTLS.secret.privateKeyKey | string | `"tls.key"` | Secret key containing the PEM-encoded client private key. |
| nodeAgent.readiness.workers | int | `8` | Maximum simultaneous node-agent readiness reconciles. |
| nodeAgent.readiness.timeout | string | `"30s"` | Deadline for agent transport setup and version RPC. |
| nodeAgent.readiness.interval | string | `"2m"` | Delay after a reconcile before its next periodic check. |
| nodeAgent.readiness.jitter | float | `0.1` | Symmetric fractional scheduling variation; 0 disables jitter. |
| nodeAgent.spireAttestationMethod | string | `"tpm"` | SPIRE node attestation method pinned to newly created nodes. Existing nodes retain their method when this deployment default changes; keep their attestation plugins available. dsxImds requires SPIFFE and a compatible DSX node image. |
| nodeAgent.tlsJwt.algorithms | list | `["RS256"]` | Asymmetric JWT signing algorithms accepted from the issuer. |
| nodeAgent.tlsJwt.dsxIdentityExchangeURL | string | `""` | DSX Identity token-exchange endpoint injected into node-agent bootstrap arguments when TLS/JWT is enabled. |
| nodeAgent.tlsJwt.enabled | bool | `false` | Pin JWT node authentication to newly created nodes. Existing nodes retain their stored transport; keep issuer/JWKS settings available for JWT-enabled nodes. Requires either spiffe.enabled or nodeAgent.externalMTLS.enabled. |
| nodeAgent.tlsJwt.issuer | string | `""` | Identity issuer expected in node-agent JWTs. |
| nodeAgent.tlsJwt.jwksURI | string | `""` | HTTPS endpoint serving the issuer's JSON Web Key Set. |
| nodeAgent.tlsJwt.subjectService | string | `""` | Service segment in the canonical local SPIFFE ID exchanged by DSX Identity. |
| nodeAgent.tlsJwt.subjectTenant | string | `""` | Tenant segment in the canonical local SPIFFE ID exchanged by DSX Identity. |
| nodeAgent.tlsJwt.subjectTrustDomain | string | `""` | Exact SPIFFE trust domain accepted in node-agent JWT subjects. |
| nodeSelector | object | `{}` |  |
| oidc.providers | list | `[]` | Fleet-wide OIDC providers that clusters trust alongside nkeOIDCAuth; a cluster keeps the list it was created with, and one created on a Kubernetes version earlier than 1.34 gets none. At most five, each with name, issuerURL, audiences, usernameClaim, usernamePrefix and caBundle (inline PEM), and optionally groupsClaim, groupsPrefix and requiredClaims (claim name to required value). |
| podAnnotations | object | `{}` |  |
| podLabels.app | string | `"cloud-workflow-engine"` |  |
| podLabels.project | string | `"nvke"` |  |
| podLabels.team | string | `"nvke"` |  |
| podSecurityContext.runAsNonRoot | bool | `true` |  |
| profiles.image.digest | string | `""` | Digest pin for the profiles content image (preferred; e.g. `sha256:…`) |
| profiles.image.enabled | bool | `false` | Copy `/profiles` from a content image into profiles.path |
| profiles.image.repository | string | `"nvcr.io/j7sbcjl3qgta/cwe-profiles"` | Repository, including registry, for the fleet profiles content image (not the nke-controller image above). Override to pull it from a different registry (e.g. a private mirror); pair any change with a matching `imagePullSecrets` entry. |
| profiles.image.tag | string | `""` | Tag for the profiles content image (used when digest is empty) |
| profiles.path | string | `""` | Directory mounted and read as profilesDir |
| profiles.runbooksPath | string | `""` | Directory read as runbooksDir for config-profile hook scripts. Empty uses the runbooks/ directory under path. |
| readinessProbe | string | `nil` |  |
| releaseCRDs.install | bool | `true` | Install the MachineImage, ClusterRelease and NodePoolRelease CRDs with the chart. |
| replicaCount | int | `1` |  |
| resources | object | `{}` |  |
| scheduledMaintenance.certRenewal.enabled | bool | `true` | Enable controller-owned periodic Kubernetes control-plane certificate renewal. |
| scheduledMaintenance.certRenewal.interval | string | `"12h"` | Interval between certificate renewal checks for each cluster. |
| scheduledMaintenance.certRenewal.thresholdDays | int | `30` | Days before expiry that the runbook renews certificates. |
| secrets.cnpgDbPasswordsSecretName | string | `""` | Name of the Kubernetes Secret (created out-of-band by Vault Secrets Operator) providing CNPG database passwords. Expected keys: CNPG_SUPERUSER_PASSWORD, CNPG_CLOUD_PASSWORD, CNPG_TEMPORAL_PASSWORD. When empty, CNPG_* env vars are not injected. When set, all three keys are required to exist in the named Secret. |
| secrets.createDbSecret | bool | `false` | Create the DB connection Secret from appDb values. Disabled by default; set true only when not using an external secret manager (e.g. Vault Secrets Operator). Requires appDb.postgresUser and appDb.postgresPassword to be explicitly configured. |
| secrets.dbSecretKey | string | `""` | secretKey with db connectionString. If not specified, defaults to "database-connection-string" |
| secrets.dbSecretName | string | `""` | If specified, secret is created with the name provided. Otherwise defaults to name based on fullname template. |
| secrets.dgxcTenantCredSecretName | string | `""` | Name of the Kubernetes Secret (created out-of-band by Vault Secrets Operator) providing DGXC tenant credentials. Expected keys: user, password. When empty, DGXC_TENANT_USER and DGXC_TENANT_PASSWORD are not injected. |
| securityContext.readOnlyRootFilesystem | bool | `true` |  |
| service.annotations | object | `{}` | Additional annotations for the service |
| service.labels | object | `{}` | Additional labels for the service |
| service.port | int | `8080` | apiserver port |
| service.nodePort | string | `""` | NodePort for a second Service beside the one above, reachable without a cluster route. Empty renders none; the Service above stays ClusterIP. |
| service.type | string | `"ClusterIP"` |  |
| serviceAccount.annotations | object | `{}` |  |
| serviceAccount.automount | bool | `true` |  |
| serviceAccount.create | bool | `true` |  |
| serviceAccount.name | string | `"cloud-workflow-engine"` | The name of the service account to use. If not set and create is true, a name is generated using the fullname template |
| serviceMonitor.enabled | bool | `true` | Enable/disable ServiceMonitor creation. ANDed with the umbrella kill-switch `global.monitoring.enabled`, which is treated as true when unset so standalone installs are unaffected. |
| serviceMonitor.labels | object | `{}` | Additional labels for the ServiceMonitor |
| serviceMonitor.matchLabels | object | `{}` | Selector labels to match the service. Values here win over the chart-derived selector labels, so leave unset to let the derived labels through |
| serviceMonitor.name | string | `"cloud-workflow-engine-metrics"` | Name of the ServiceMonitor |
| spiffe.capdManagerSubject | string | `""` | Subject the nvke:capi-cluster-admin RBAC binding names for the CAPD manager. Defaults to its local SPIFFE ID; set it to the rewritten subject when CAPD uses an exchanged tenant kubeconfig. |
| spiffe.capiKubeadmBootstrapManagerSubject | string | `""` | Subject the nvke:capi-cluster-admin RBAC binding names for the kubeadm bootstrap manager. Defaults to its local SPIFFE ID; set it to the rewritten subject when that controller uses an exchanged tenant kubeconfig. |
| spiffe.capiKubeadmControlPlaneManagerSubject | string | `""` | Subject the nvke:capi-cluster-admin RBAC binding names for the kubeadm control-plane manager. Defaults to its local SPIFFE ID; set it to the rewritten subject when that controller uses an exchanged tenant kubeconfig. |
| spiffe.capiManagerSubject | string | `""` | Subject the nvke:capi-cluster-admin RBAC binding names for capi-manager. Defaults to spiffe://<trustDomain>/nke-service/capi-manager, which is what a tenant sees without token exchange. Set it whenever jwtExchangeEndpoint is set because the exchange may rewrite the token's sub claim. |
| spiffe.controllerSubject | string | `""` | Subject the nkeOIDCAuth.tenantRbacName RBAC bindings name in tenant clusters. Defaults to this controller's own SPIFFE ID, spiffe://<trustDomain>/nke-service/cloud-workflow-engine, which is what a tenant sees when no exchange is in play. Set it whenever jwtExchangeEndpoint is set: the exchange re-issues the token under its own trust domain and rewrites the path, so give the exchanged subject in full, e.g. spiffe://nke.<site>.cloud-workflow-engine.nvid.id/bm/cwe. A mismatch authenticates and then fails every authorization check. |
| spiffe.cweSubject | string | `""` | Older name for controllerSubject, still read. When both are set, controllerSubject wins. |
| spiffe.enabled | bool | `false` | Enable SPIFFE identity for mTLS to tenant clusters |
| spiffe.endpointSocket | string | `"unix:///spiffe-workload-api/spire-agent.sock"` |  |
| spiffe.jwtExchangeEndpoint | string | `""` | RFC 8693 token-exchange endpoint the exec credential plugin trades its JWT-SVID at before calling a tenant API server. Leave empty to present the JWT-SVID directly, which is what every site does today. Set it only where the tenant clusters trust the exchange's issuer, otherwise the controller's calls to them will start failing authentication. |
| spiffe.spire.className | string | `"spire-mgmt-spire"` |  |
| spiffe.spire.joinTokenTTL | string | `"1h"` | TTL used when creating SPIRE join tokens through the SPIRE server API. |
| spiffe.spire.serverAddress | string | `""` | SPIRE server address. When unset, serverAddressFrom is used. |
| spiffe.spire.serverAddressFrom | object | `{"fieldRef":{"fieldPath":"status.hostIP"}}` | Kubernetes valueFrom source for SPIRE server address when serverAddress is unset. |
| spiffe.spire.serverNamespace | string | `"spire-server"` |  |
| spiffe.spire.serverPort | string | `"30440"` |  |
| spiffe.spire.tpmPubhashesConfigMapName | string | `"tpm-pubhashes"` |  |
| spiffe.trustDomain | string | `""` | SPIRE trust domain. When set, the chart renders ConfigMap nke-rbac. Required when nkeOIDCAuth.enabled is true: builds the nkeOIDCAuth.tenantRbacName SPIFFE subject ID in that ConfigMap. Must equal the SPIRE server's trust domain (the spire chart's global.spire.trustDomain) so the RBAC principals match issued identities. |
| supportedInfraProviders | string | `"nico"` | Comma-separated API provider blocks accepted by mutating cluster and node-pool requests. The chart defaults the deployed controller to Nico only; set to "nico,infra" only for deployments that should accept generic CAPI-backed provider payloads. |
| teleport.enabled | bool | `false` | Enable Teleport identity for the controller to manage Teleport resources (tokens, nodes) |
| teleport.environment | string | `"non-prod"` | Teleport environment label: "prod" or "non-prod" (default: "non-prod") |
| teleport.identityFile | string | `"/var/run/teleport/identity"` | Path where the tbot identity file is mounted |
| teleport.identitySecretName | string | `"cloud-workflow-engine-teleport"` | Name of the Kubernetes secret containing the tbot identity (written by tbot output) |
| teleport.managedByLabelValue | string | `"nvke-controller"` | Value of the Teleport "managed-by" label this controller stamps on tokens it creates, and the value ownership checks compare against. An existing site pins its old value ("nke-cloud-workflow-engine"): ownership compares the value, so a site that does not stops recognizing its tokens. |
| teleport.proxyAddress | string | `""` | Teleport proxy address (site-specific, must be set per environment) |
| teleport.resourceGroupLabelValue | string | `"nvke"` | Value of the Teleport "resource-group" label stamped on the tokens and node userdata this controller creates. An existing site pins "nke". |
| teleport.tokenNamePrefix | string | `"nvke-controller-"` | Prefix for the deterministic Teleport TPM join token name this controller mints for a node. An existing site pins "nke-cwe-". |
| temporal.cloudWorkflow.namespace | string | `"cloud-workflow-engine"` |  |
| temporal.cloudWorkflow.queue | string | `"cloud-workflow-engine-tasks"` |  |
| temporal.host | string | `"temporal-frontend.temporal.svc.cluster.local"` |  |
| temporal.mtls.caCertFile | string | `"ca.crt"` |  |
| temporal.mtls.clientCertFile | string | `"tls.crt"` |  |
| temporal.mtls.clientKeyFile | string | `"tls.key"` |  |
| temporal.mtls.enabled | bool | `false` | Enable mTLS when connecting to Temporal. |
| temporal.mtls.mountPath | string | `"/var/secrets/temporal/certs/client"` | Mount path for the Temporal client certificate secret. |
| temporal.mtls.secretName | string | `""` | Secret containing ca.crt, tls.crt, and tls.key for the Temporal client. |
| temporal.mtls.serverName | string | `""` |  |
| temporal.port | int | `7233` |  |
| temporal.publicTemporalHostPort | string | `""` |  |
| tenantAccess.enabled | bool | `true` | Publish each cluster's kubeconfig as a Secret in the nke-system namespace, and remove those Secrets during cluster teardown. Gates both the controller and its cleanup activity, and the Secret grant they depend on. |
| tolerations | list | `[]` |  |
| tracing.enabled | bool | `false` | Enable OpenTelemetry tracing in the controller binary (TracerProvider + transport hooks). Keep false in chart defaults; enable per environment against an OTLP collector. |
| tracing.endpoint | string | `""` | Optional OTLP gRPC endpoint override (host:port). Leave empty when operatorInject.enabled is true so the operator-injected localhost sidecar endpoint is used. |
| tracing.insecure | bool | `true` | Use insecure (plaintext) OTLP gRPC. Required for localhost sidecar / most in-cluster collectors. |
| tracing.operatorInject.enabled | bool | `false` | When true (with tracing.enabled), add OpenTelemetry Operator pod annotations. |
| tracing.operatorInject.injectSdk | string | `""` | Value for instrumentation.opentelemetry.io/inject-sdk (namespace/name). |
| tracing.operatorInject.injectSidecar | string | `""` | Value for sidecar.opentelemetry.io/inject (namespace/name). |
| tracing.serviceName | string | `"cloud-workflow-engine"` | Resource attribute service.name written when tracing.endpoint is set. With operatorInject, prefer operator-injected OTEL_SERVICE_NAME instead. |
| volumeMounts | list | `[]` |  |
| volumes | list | `[]` |  |
