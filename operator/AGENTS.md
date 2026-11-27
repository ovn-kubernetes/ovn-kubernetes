# AGENTS.md — OVN-Kubernetes operator

Installs and manages OVN-Kubernetes through one `OVNK8sOperator` custom
resource. A separate Go module scaffolded with Kubebuilder; see the root
[`AGENTS.md`](../AGENTS.md) for repository conventions and the OKEP for the
design rationale.

## Architecture

- **One CR, one controller.** The CR is a singleton named `default` in
  `ovnk8s-operator-system`. The controller reconciles only on CR spec changes
  (`GenerationChangedPredicate`, no watches on managed objects).
- **Templates are the desired state.** `templates/` holds Go `text/template`
  files converted from the upstream Helm chart (`helm/ovn-kubernetes`). The
  operator renders them from the CR plus its embedded config and applies the
  result.
- **Components in dependency order.** `internal/defaults/config.yaml` declares
  deployment modes (only `standard` has templates), components
  (`ovnkubeControlPlane`, `ovnkubeNode`, optional `ovnkubeIdentity`), their
  `dependsOn` edges and default images. `global` is always deployed first.

### Reconcile flow (`internal/controller`)

1. Handle deletion: remove every resource labeled as managed by this CR, then
   drop the `ovnk8s-operator.ovn.org/cleanup` finalizer.
2. Check the release (`version`, `minUpgradeVersion`) and classify the
   operation: installation, upgrade, recovery, steady or blocked.
3. If identity is enabled, ensure the webhook cert Secret
   (`internal/webhookcert`) and pass its CA to the renderer.
4. Render all components (`internal/renderer`), then apply
   `componentOverrides[].patches` to each component's workload.
5. Copy `global.imagePullSecrets` from the operator namespace.
6. For each component in DAG order: apply its manifests
   (`internal/deployer`, server-side apply as field manager `ovnk8s-operator`
   with forced ownership), then wait until its Deployments and DaemonSets are
   ready (`internal/validator`, polls up to `--validation-timeout`).
7. Run test hooks if `spec.testHooks.enabled` (`internal/testhook`).
8. Update status conditions: `UpgradeComplete`, `CNIHealthy`,
   `ImagePullSecretsSynced` (`internal/status`).

Readiness polling and test hooks run inside `Reconcile`, so a long rollout
delays the next reconcile.

### Template data model

- `.global`: `spec.global` fields by JSON name, plus `podNetworkCIDR` and
  `serviceNetworkCIDR` (comma-joined, pod entries as `cidr/hostPrefix`),
  `featureGates` (dotted names of the gates present, such as `egress.ip`),
  `featureGateConfig` (gate options by gate name), `components` (enabled
  component names) and `metrics`.
- `.<component>`: `.<container>.image.{repository,tag,sha256}`,
  `customVariables`, `nodePlacement`, `replicas`;
  `.ovnkubeIdentity.webhookCABundle` holds the PEM CA.
- Functions: `includeRaw`, `b64enc`, `b64dec`, `has`, `get` (empty string when
  missing), `toYaml`, `indent`, plus Go's built-ins. There is no sprig.
- A feature is on only when its gate is present in the CR. Settings with no
  API field are hard-coded to the chart's `values-single-node-zone.yaml`
  defaults.

## Layout

```text
api/v1alpha1/          # CRD types and markers
cmd/main.go            # entry point; `render` subcommand prints manifests for a CR
internal/
  operator/            # manager setup (run.go), render subcommand
  controller/          # reconciler, finalizer, RBAC markers (rbac.go)
  defaults/            # embedded config.yaml, image stamping via -ldflags
  dependency/          # component DAG
  renderer/            # template data, functions, patches
  deployer/            # server-side apply, labels, uninstall cleanup
  validator/           # workload readiness
  status/              # conditions
  webhookcert/         # identity webhook CA and serving cert
  testhook/            # post-deployment test Jobs
  kinds/               # Kubernetes kind names
templates/             # global/ (incl. crds/), ovnkubeControlPlane/, ovnkubeNode/, ovnkubeIdentity/
test-hooks/            # hook Job templates
config/                # kustomize: CRD, RBAC, manager Deployment, sample CR
hack/helm-parity/      # temporary Helm-vs-operator render comparison
hack/kind.sh           # kind cluster with OVN-Kubernetes installed by the operator
```

## Rules

- **Generated files, do not edit:** `config/crd/bases/*.yaml` and
  `config/rbac/role.yaml` (`make manifests`), `**/zz_generated.*.go`
  (`make generate`), `PROJECT`. Keep `// +kubebuilder:scaffold:*` markers.
- **CRDs come from the chart.** `templates/global/crds/` is a copy of
  `helm/ovn-kubernetes/crds`; refresh it with `make sync-crds`. CI runs
  `make verify-crds`.
- **RBAC must cover the templates.** Kubernetes only lets the operator create
  roles it already holds. When a template grants a new permission or renders a
  new kind, add the `+kubebuilder:rbac` marker in
  `internal/controller/rbac.go`, run `make manifests`, and add new kinds to
  `deployedResources` in `internal/renderer/rbac_test.go` and to `getGVK` in
  `internal/deployer/cleanup.go`.
- **Template changes track the chart.** Keep the rendered output equivalent to
  the Helm chart for the same settings and check with `make helm-parity`.
- **New CR inputs need a test.** Add the expected field to
  `internal/renderer/template_integration_test.go`.
- **Logging:** use `log.FromContext(ctx)`; the manager logs through klog
  (`--loglevel`, default 4).

## Commands

Run from `operator/`:

| Command | Purpose |
|---------|---------|
| `make manifests generate` | Regenerate the CRD, RBAC and DeepCopy code |
| `make test` | Unit and envtest tests (sets `KUBEBUILDER_ASSETS`) |
| `make lint` | golangci-lint |
| `make build`, `make docker-build IMG=...` | Build the manager binary or image |
| `make sync-crds`, `make verify-crds` | Refresh or check the bundled CRDs |
| `make helm-parity` | Compare the render with the Helm chart on kind |
| `make kind` | Create a kind cluster installed by the operator |
| `hack/kind.sh --test-uninstall` | Same, then delete the CR and check cleanup |
| `go run ./cmd render --cr <file> --templates ./templates` | Print the manifests for a CR |
