# API authentication in nvke-controller-config: design

> **Status:** phase 1 is implemented. A chart install and released `nvkectl` need no changes, but the `apiAuth` section is required, so a hand-written `nvke-controller-config` must now carry one (see [Startup behaviour](#startup-behaviour)). [Phase 2](#phase-2), which retires the old path, ships in two steps; the first is implemented.
>
> **Names:** the chart names the settings ConfigMap `nvke-controller-config` and the authentication ConfigMap `nvke-auth-config` by default; an install that predates the chart's move to nvke names pins `nke-controller-config` and `nke-auth-config`. Below, `nvke-controller-config` means the settings ConfigMap whatever its name. Where this document describes behaviour from before that move, `nke-auth-config` is the authentication ConfigMap, and the Tilt scaffolding it names is now `controller/deploy/k8s/nvke-auth-dev.yaml`.

## Problem

The controller reads every setting from `nvke-controller-config` at startup except one: the NVKE API authentication configuration. That is still its own file, `auth.yaml` in the `nke-auth-config` ConfigMap, found through the `AUTH_CONFIG_PATH` environment variable and loaded by `auth/authn`. So the controller has two configuration files, two loaders and two ways to be misconfigured.

The authentication configuration was left out of the move to `nvke-controller-config` deliberately, because folding it in raises two questions a rename does not answer:

- **How authentication is turned off.** Today an unset `AUTH_CONFIG_PATH` means off, a valid file means on, and a broken file stops the controller rather than letting it start unprotected. `nvke-controller-config` is always mounted, so it can never be absent. The off switch has to become explicit.
- **Who owns the schema.** `auth.yaml` is a separately designed feature with its own types, strict loader and `apiVersion`/`kind` header. Folding it in must not leave a second copy of its rules.

## How it is used today

The chart has two modes, both selected by `apiAuth.configMap.create`:

| Mode | Who uses it | What happens |
|---|---|---|
| `create: true` | Every fleet site and the operator guide | The chart renders `apiAuth.config` into `nke-auth-config`, mounts it at `/etc/nke/auth` and sets `AUTH_CONFIG_PATH=/etc/nke/auth/auth.yaml`. |
| `create: false` | Tilt (`controller/deploy/k8s/nvke-auth-dev.yaml`) and the CAPD and CAPNICo e2e deploy paths (`nvke-auth-dev` items) | The chart mounts a ConfigMap it does not own and sets `AUTH_CONFIG_PATH`. |

Every fleet site has the same shape: one issuer, `local-kubernetes`, the management cluster's own ServiceAccount issuer, with the projected ServiceAccount CA and token as its `caBundlePath` and `bearerTokenPath`, `serviceAccounts` admission and one audience. No site uses `apiAuth.configMap.extraFiles`.

Outside the controller, these read the configuration:

- `nvkectl` (`cli/internal/auth/source.go`) treats a missing `nke-auth-config` as "authentication off" and mints a ServiceAccount token for `audiences[0]` of its `auth.yaml` otherwise.
- The e2e suite's token helper (`controller/test/e2e/internal/authtoken`) reads the audiences from `nke-auth-config` the same way.
- The e2e suite's `detectAuthnEnforcement` (`controller/test/e2e/authn_test.go`) treats a missing `AUTH_CONFIG_PATH` on the controller Deployment as "authentication off", and reads `permissiveMode` from `nvke-controller-config` otherwise.
- `hack/nvke-token.sh` and `hack/nvke-auth-demo.sh` read `AUTH_CONFIG_PATH` from the Deployment the same way.

## Goals

- The controller reads the authentication configuration from `nvke-controller-config` when the chart renders it, through the same startup load as every other setting.
- `auth/authn` keeps the only definition of the schema and its rules.
- Turning authentication off is an explicit setting, and a configuration that is filled in but turned off is an error, not a silent "off".
- A configuration that is set but invalid still stops the controller at startup.
- No operator changes their chart values, and released `nvkectl` keeps working.

## Non-goals (phase 1)

- Retiring `AUTH_CONFIG_PATH` or the external-ConfigMap mode (`create: false`).
- Moving `nvkectl` off `nke-auth-config`.
- Moving `permissiveMode` under `apiAuth`. It governs the legacy file too, which is read while `apiAuth.enabled` is `false`, so it cannot live in a section that is turned off.
- An inline CA bundle. `caBundlePath` keeps working, and `extraFiles` keep landing in `/etc/nke/auth`.

## Design

### Schema

`nvke-controller-config` gains an `apiAuth` section. Its `audiences` and `issuers` are `auth/authn`'s existing `Config` fields, unchanged; `enabled` is new.

```yaml
permissiveMode: false        # unchanged, top level
apiAuth:
  enabled: true
  audiences: [nke-site-example]
  issuers:
    - name: local-kubernetes
      issuer: https://kubernetes.default.svc.cluster.local
      jwks:
        uri: https://kubernetes.default.svc/openid/v1/jwks
        caBundlePath: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
        bearerTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token
      algorithms: [RS256]
      admit:
        serviceAccounts:
          - namespace: nke-clients
```

The embedded section has no `apiVersion` or `kind`, like every other section of the file. The standalone `auth.yaml` keeps requiring both.

### Startup behaviour

| `apiAuth.enabled` | `AUTH_CONFIG_PATH` | The controller |
|---|---|---|
| `false`, `audiences` and `issuers` empty | unset | Runs with authentication off, as today. |
| `false`, `audiences` and `issuers` empty | set | Loads the file at `AUTH_CONFIG_PATH`, as today. This is the external-ConfigMap mode. |
| `false`, `audiences` or `issuers` present | either | Refuses to start: the configuration is filled in but turned off. |
| `true` | unset | Validates `apiAuth` with `auth/authn`'s rules and enables authentication. A validation failure stops the controller. |
| `true` | set | Refuses to start and names both sources, rather than choosing one. |

Permissive mode comes from `permissiveMode` in every case, as today. The startup log line that reports the mode also names its source: `nvke-controller-config` or the `AUTH_CONFIG_PATH` file.

The `apiAuth` section itself is required: a `config.yaml` without it, or with `apiAuth: null`, refuses to start, so a missing section never reads as "off" and turning authentication off takes an explicit `enabled: false`, which the chart always renders.

### auth/authn

- One exported validation entry point runs everything `Load` checks today apart from the file header: `Config.Validate` plus the "every issuer needs `admit`" check, which today lives only in `Load`. `Load` calls it too, so both paths enforce identical rules.
- `Setup` takes either a path (the legacy file) or an already-validated `*Config`. Everything after loading, from key fetching to the middleware, is unchanged.

### controllerconfig

- `Config` gains `APIAuth`, holding `Enabled`, `Audiences` and `Issuers`. `Issuers` is a list of `authn.Issuer`, so the issuer schema stays defined once. The strict loader parses the section with the rest of the file.
- `validate` applies the table above: with `enabled: false`, `audiences` and `issuers` must be empty; with `enabled: true`, it calls `auth/authn`'s validation and prefixes errors with `apiAuth:`.
- `LoadStartupConfig`, which sees both the file and the environment, refuses `apiAuth.enabled` together with `AUTH_CONFIG_PATH`.
- `cmd/worker` passes the section to `authn.Setup` when it is enabled, and the path otherwise.
- `hack/local-controller-config.yaml` gains `apiAuth: {enabled: false, audiences: [], issuers: []}`.

### Chart

The values are unchanged. What the chart renders depends on the mode:

| Values | Rendered | The controller reads |
|---|---|---|
| `apiAuth.enabled: false` | `apiAuth: {enabled: false, audiences: [], issuers: []}`; no `AUTH_CONFIG_PATH` | nothing: authentication off |
| `enabled: true`, `configMap.create: true` | `apiAuth` with `enabled: true` and `apiAuth.config`'s `audiences` and `issuers`; no `AUTH_CONFIG_PATH`. `nke-auth-config` is still rendered from the same values and mounted at `/etc/nke/auth`. | `apiAuth` |
| `enabled: true`, `configMap.create: false` | `apiAuth: {enabled: false, …}`; the external ConfigMap mounted and `AUTH_CONFIG_PATH` set, as today | the file at `AUTH_CONFIG_PATH` |

`nke-auth-config` keeps being rendered in `create: true` mode for two reasons. `nvkectl` and the e2e token helper still find it there, and `extraFiles` still land in `/etc/nke/auth` for `caBundlePath`. Both copies come from the same `apiAuth.config` value, so they cannot disagree. The Reloader annotation already lists both ConfigMaps, and the render-time checks on `apiAuth.config` and `configMap.create` are unchanged. `AUTH_CONFIG_PATH` stays off the chart's list of retired variables, because the external-ConfigMap mode still sets it.

### Clients and tooling

`detectAuthnEnforcement` and both scripts treat authentication as on when either `AUTH_CONFIG_PATH` is set on the Deployment or `apiAuth.enabled` is `true` in `nvke-controller-config`. Reading both keeps the e2e suite correct against controllers older than this change. `nvkectl` needs no change in phase 1.

## Compatibility and rollout

- **Fleet sites** keep their values. After the upgrade their controller reads `apiAuth` instead of `AUTH_CONFIG_PATH`, with the same issuers, audiences and mode.
- **Tilt and the CAPNICo e2e path** keep the external ConfigMap and the legacy path.
- **Rollback** is chart and image together, as for #1527. A controller older than this change rejects the unknown `apiAuth` key and does not start, so a mismatched pair fails closed rather than running unauthenticated.
- **A partial rollback that reverts `nvke-controller-config` while the new controller keeps running** also fails closed, because the older `config.yaml` has no `apiAuth` section and the controller refuses to start without one.

## Testing

- `auth/authn`: the shared validation entry point, including the `admit` check, and `Setup` from an in-memory `Config`.
- `controllerconfig`: a table over `enabled` and the presence of `audiences` and `issuers`, including an absent or null section, which is refused; the `AUTH_CONFIG_PATH` conflict in `LoadStartupConfig`; the drift tests extended to the new key.
- Chart: `chart-auth-assertions.sh` checks that `create: true` renders `apiAuth` and no `AUTH_CONFIG_PATH`, and that `create: false` is unchanged. `chart-controller-config-assertions.sh` checks that `apiAuth` renders in every mode.
- E2E: the CAPD e2e deploy path moves to `create: true`, so CI exercises the new path. The authentication document moves into its overlay values. Its `nvke-auth-dev` item keeps the ServiceAccounts and RBAC but drops its `nke-auth-config` ConfigMap, because the chart now renders that ConfigMap. Tilt and the CAPNICo e2e path exercise the legacy one. The pull request's CAPD check does not select the authentication specs, so a full CAPD run is dispatched on the branch before it is marked ready; the local entry point runs Tilt, which stays on the external ConfigMap.
- Docs: the operator guide (Secure the NVKE API), `controller/docs/configuring-api-authentication.md` and the `AUTH_CONFIG_PATH` row in `controller/AGENTS.md`.

## Phase 2

Phase 1 left the controller with two sources: `apiAuth` for a chart-rendered configuration and `AUTH_CONFIG_PATH` for a ConfigMap managed elsewhere. Phase 2 retires the second, so `apiAuth` in `nvke-controller-config` is the only place the controller, `nvkectl` and the tooling read authentication from.

### Constraints

- **Released `nvkectl` reads only the authentication ConfigMap.** Every release up to `cli/v0.3.0-rc.2` looks up `nvke-auth-config` or `nke-auth-config` and treats their absence as "authentication off". If the chart stopped rendering that ConfigMap first, those clients would send no token and get 401. So the chart can drop it only after an `nvkectl` that reads `nvke-controller-config` has been released.
- **No released controller has `apiAuth`.** Phase 1 is unreleased, so the shape of the section can still change without a deprecation window: any `config.yaml` that loads on a released controller has no `apiAuth` and is refused by this one regardless.

Phase 2 therefore ships as two pull requests.

### Step 1: everything reads `apiAuth` (not breaking)

**`nvkectl` and the tooling read `nvke-controller-config` first.** `nvkectl` (`cli/internal/auth/source.go`), the e2e token helper (`authtoken.RawConfig`) and `hack/nvke-token.sh` look for the controller's settings ConfigMap, `nvke-controller-config` then `nke-controller-config`, in the same namespaces as before. When its `apiAuth.enabled` is `true` they mint for `apiAuth.audiences[0]`. When the section is off or absent they fall back to the authentication ConfigMap, because in the external-ConfigMap mode `apiAuth.enabled` is `false` while the controller authenticates through `AUTH_CONFIG_PATH`. A denied read is handled as before: remembered, and returned only when nothing readable turns authentication on, so a missing grant never reads as "off". `nvkectl` users need `get` on the settings ConfigMap as well as the authentication one; a role scoped by `resourceNames` must add it.

**Tilt and the CAPNICo e2e path move onto values**, as CAPD did in phase 1. Tilt passes `hack/tilt-controller-api-auth-values.yaml`, with `configMap.create: true` and the configuration, whenever `NKE_AUTH_ENABLED` is true. `deploy/k8s/nvke-auth-dev.yaml` and the CAPNICo `nvke-auth-dev` copy keep the namespaces, ServiceAccounts and token-minter RBAC and drop their `nvke-auth-config` ConfigMap, which the chart now renders. After this nothing in the repository uses `configMap.create: false`. An existing Tilt cluster has a `nvke-auth-config` that Tilt applied, which Helm will not adopt: delete it once before the next `tilt up`.

**`permissiveMode` moves to `apiAuth.permissive`.** The Helm value has always been `apiAuth.permissive`; only the rendered `config.yaml` put it at the top level, as a leftover of the move from the `PERMISSIVE_MODE` variable. It now sits beside the rest of the section. It applies to whichever source configures authentication, so it may be `true` while `apiAuth.enabled` is `false` and `AUTH_CONFIG_PATH` is set; the rule that an off section must be empty covers `audiences` and `issuers` only. A `config.yaml` that still sets top-level `permissiveMode` is refused with an error naming `apiAuth.permissive`, rather than strict decoding's bare "unknown field". The move carries no compatibility window, for the reason under [Constraints](#constraints). The chart refuses `permissive` inside `apiAuth.config`, as it refuses `enabled`, and the retired `PERMISSIVE_MODE` variable now points at `apiAuth.permissive`.

**Issuers take an inline CA bundle.** `jwks.caBundle` is a PEM bundle carried in the configuration itself, an alternative to `caBundlePath`. At most one of the two may be set, and a bundle with no PEM certificates is refused at validation, so it fails at startup like every other configuration error. A CA certificate is public, so it needs no Secret, which matches `oidc.providers[].caBundle`. It is what lets step 2 drop `extraFiles`: today a private CA has to be a file in `/etc/nke/auth`, which exists only because `nvke-auth-config` is mounted there.

### Step 2: retire the old path (breaking)

Merged only after a release that includes step 1, and deployed as chart and image together, as for #1527.

- **`AUTH_CONFIG_PATH` is retired.** The chart refuses it in `env`. The controller refuses to start when it is set, so a hand-written Deployment fails closed instead of starting without the authentication it asked for. `authn.Load`, the file header check and `Options.ConfigPath` go, since the file was their only caller; `auth.yaml`'s `apiVersion` and `kind` go with them.
- **The external-ConfigMap mode is removed.** `apiAuth.configMap.create`, `apiAuth.configMap.name` and `apiAuth.configMap.extraFiles` are refused by name, as the chart refuses other removed values. `apiAuth` renders whenever `apiAuth.enabled` is true.
- **`nvke-auth-config` is no longer rendered,** and its volume, mount and Reloader entry go. `caBundlePath` stays: it is how an issuer names a file the pod already has, such as the projected ServiceAccount CA.
- **`apiAuth.permissive` requires `apiAuth.enabled`.** With one source, permissive with authentication off is a configuration that does nothing, so it is refused.
- **`nvkectl` and the tooling keep their fallback** to the authentication ConfigMap, for sites still running a controller from before step 2.

`nvkectl` older than step 1 sends no token to a controller from step 2 and gets 401. The release notes for step 2 say so; the fix is to upgrade `nvkectl`.

### Testing

- `auth/authn`: `caBundle` validation (both set, not PEM) and a JWKS fetch trusting an inline bundle.
- `controllerconfig`: `apiAuth.permissive` while off and on, and the refusal of top-level `permissiveMode`; the drift tests map `PERMISSIVE_MODE` to the new key.
- Chart: `chart-auth-assertions.sh` checks `apiAuth.permissive` in both modes, the absence of the top-level key, the refusal of `permissive` in `apiAuth.config`, and an inline bundle rendered without `extraFiles`.
- `nvkectl` and `authtoken`: the lookup order, the fallback when the section is off or absent, a pinned settings name, and denied reads.
- E2E: CAPD and CAPNICo both run on `create: true`; the authentication specs read `apiAuth.permissive`.

## Alternatives considered

- **`authn.Load` reads the section out of `config.yaml`.** One loader, but `auth/authn` would have to know the controller configuration's layout, and the file would be parsed twice.
- **The chart renders `auth.yaml` from `nvke-controller-config`'s values and the controller is left alone.** The controller would still read two files, which is the problem this solves.
- **Presence of issuers as the on switch.** Fewer keys, but an accidentally emptied list would turn authentication off instead of failing, against the fail-closed contract.
- **Move `permissiveMode` under `apiAuth` now.** Tidier, but the legacy file, read while `apiAuth.enabled` is `false`, would then take its mode from a section that is turned off. Moving it would also break the e2e suite's `SetPermissiveMode`, which writes the top-level key, on controllers older than this change. Phase 2 moves it anyway: allowing `permissive` in a section that is off answers the first point, and the e2e suite runs against the controller it ships with.
- **Keep `permissiveMode` at the top level in phase 2.** No migration, but `config.yaml` would keep a shape that differs from the values, and the move only gets more expensive once `apiAuth` is released.
