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
