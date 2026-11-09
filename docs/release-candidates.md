# Deploying Release Candidates

The shared publisher sets the chart version, appVersion, source revision, and bundled auth-callout dependency version. It does not rewrite image repositories or tags in chart values.

Select a published RC and download its chart packages from the NGC team `0837451325059433/components-dev`. Use the same version for charts and Exchange images, without the Git tag's `v` prefix.

Apply the following overrides after your site values. Replace `X.Y.Z-rc.N` with the published version. These overrides do not replace the site configuration or provision credentials.

## DSX Event Bus

For the `nats-event-bus` chart, save these overrides as `event-bus-rc.yaml`:

```yaml
auth-callout:
  image:
    repository: nvcr.io/0837451325059433/components-dev/auth-callout
    tag: "X.Y.Z-rc.N"
  imagePullSecrets:
    - name: ngc-dsx-registry-creds
```

For the standalone `auth-callout` chart, use `image` and `imagePullSecrets` at the root, without the `auth-callout` wrapper.

## DSX Agent Gateway

For the `dsx-agent-gateway` chart, save these overrides as `agent-gateway-rc.yaml`:

```yaml
bridge:
  image:
    repository: nvcr.io/0837451325059433/components-dev/dsx-agentgateway-bridge
    tag: "X.Y.Z-rc.N"
  imagePullSecrets:
    - name: ngc-dsx-registry-creds
```

Keep `bridge.enabled` and its connection settings in your site configuration. The bridge is disabled by default. These overrides do not change upstream Agentgateway, NATS, or other dependency images.

## Verify Before Applying

Provision `ngc-dsx-registry-creds` in each release namespace with read-only access to the RC images, or use your existing pull secret name. Do not use the CI publishing key on cluster nodes.

Render each downloaded chart with its site values and RC overrides. For example, after replacing the version:

```bash
helm template event-bus ./nats-event-bus-X.Y.Z-rc.N.tgz \
  --namespace dsx-event-bus \
  -f site-event-bus.yaml -f event-bus-rc.yaml

helm template agent-gateway ./dsx-agent-gateway-X.Y.Z-rc.N.tgz \
  --namespace dsx-agent-gateway \
  -f site-agent-gateway.yaml -f agent-gateway-rc.yaml
```

Check that the auth-callout and enabled bridge Deployments use the NGC repositories and RC tag shown above. Keep the same values when installing or upgrading.

## Promote and Deploy an RC

Keep promotion disabled until the [shared promotion setup](https://github.com/dsx-ai-factory/dsx-github-actions/blob/main/docs/promote-rc.md) is complete, including the protected environment, reviewers, and credentials. Then follow these steps:

1. Add the dispatcher and policy to `main`. Update each existing `release/X.Y.Z` branch's `.github/workflows/release-rc.yml` to use the policy's `source_publisher_sha` and `promotion-manifest: true`.
1. Publish a new RC through that release branch and have QA validate it. Older RCs without a promotion manifest cannot be selected.
1. After activation, open **Actions > Promote RC > Run workflow** on `main`. Enter the QA-approved `X.Y.Z-rc.N` and obtain release approval.
1. Download the promoted `X.Y.Z` charts. Keep your site settings and the repository and pull-secret overrides above, changing image tags to `X.Y.Z`. Render and check image references before installation.

Promotion reuses image digests without rebuilding; embedded image versions remain RC versions. The stable `nats-event-bus` chart retains its vendored RC `auth-callout` chart. Set its image tag explicitly to select the stable image; otherwise retain access to the RC image.
