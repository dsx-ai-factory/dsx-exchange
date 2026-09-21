# DSX Exchange

DSX Exchange is a monorepo for the DSX Event Bus and Agentgateway, including
schemas, services, Helm charts, and local evaluation tooling.

Documentation for DSX Exchange is available at [https://docs.nvidia.com/dsx-exchange](https://docs.nvidia.com/dsx-exchange).

## Overview

DSX Exchange includes:

- `schemas`: AsyncAPI contracts for DSX Exchange MQTT topics and payloads.
- `auth-callout`: NATS auth callout service for OAuth2, mTLS, NKey, and no-auth profiles.
- `dsx-agentgateway-bridge`: MCP discovery and request routing over NATS.
- `deploy`: Helm charts for the NATS event bus and Agentgateway.
- `local`: Kind-based local evaluation environment, Skaffold deployment, MQTT and MCP tests, and benchmark tooling.

The event bus itself is schema agnostic. Schemas document externally visible contracts; NATS and the auth callout enforce routing, federation, and authorization behavior.

Upstream Agentgateway routes authenticated MCP requests to local MCP servers.
The `dsx-agentgateway-bridge` adds remote discovery and request routing over the
DSX Event Bus.

## Requirements

- OS: Linux or macOS with Docker support.
- Tools: `mise`, `make`, and Docker. Mise installs the remaining tools from the
  locked root toolchain.
- Kubernetes: a local Kind cluster for e2e testing.
- Runtime: Go modules use the Go version pinned in `mise.toml`.

GPU drivers are not required.

## Getting Started

Clone the repository, install the local e2e prerequisites, and run the local
validation checks:

```bash
git clone https://github.com/dsx-ai-factory/dsx-exchange.git
cd dsx-exchange
mise install --locked
make test
```

If you already have a DSX Event Bus and need to build or test an MQTT
integration application, start with the
[Integrator Quickstart](https://docs.nvidia.com/dsx-exchange/integrator-quickstart).

To deploy or integrate with DSX Agent Gateway, start with the
[DSX Agent Gateway overview](https://docs.nvidia.com/dsx-exchange/agent-gateway).

Publish looping dummy BMS data into the local CSC MQTT broker:

```bash
make dummy-bms
```

## Usage

Use the top-level Makefile for repository workflows:

```bash
make help
make check
make test
```

After the local Kind environment is deployed, run the dummy BMS demo with
`make dummy-bms`.

## Performance

`make test` includes smoke-sized Event Bus and Agent Gateway performance
validation suitable for Kind.

Run the Event Bus benchmark directly:

```bash
go -C local/mqttbs run ./cmd/mqttbs run basic-suite \
  --broker tcp://172.18.200.1:1883
```

See [local/mqttbs/README.md](local/mqttbs/README.md) for smoke-sized options.

## Releases & Roadmap

- Release notes: [CHANGELOG.md](CHANGELOG.md)
- Third-party license inventory: [THIRD_PARTY_LICENSES.csv](THIRD_PARTY_LICENSES.csv) and [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md)

### Versioning

DSX Exchange follows [Semantic Versioning](https://semver.org/) (`vX.Y.Z`), automated via semantic-release. A new version is published automatically when a semantic-release compliant commit is merged to `main`.

| Commit prefix | Version bump | When to use |
|---------------|--------------|-------------|
| `fix:` | Patch (Z) | Bug fixes, CVE remediation |
| `feat:` | Minor (Y) | New features, backward-compatible changes |
| `feat!:` or `BREAKING CHANGE:` | Major (X) | Breaking API, schema, or chart changes |

### Release Candidates

New release branches use the shared `release-candidate-publish.yml` workflow through [release-rc.yml](.github/workflows/release-rc.yml).
The caller pins a reviewed shared-workflow commit and supplies inline image and chart manifests.
Shared jobs own manifest validation, source tags, GitHub prereleases, NGC publishing, and immutable-artifact checks.
Exchange does not maintain RC publishing jobs or RC helper scripts.

1. Create a protected `release/X.Y.Z` branch from `main` with this workflow.
2. Choose the target that Conventional Commits imply. A different target fails before publishing; the branch name does not force a version bump.
3. Merge release changes through reviewed PRs. Qualifying commits produce
   `vX.Y.Z-rc.N`; commits without release changes do not create a new RC.

The shared workflow validates manifest JSON and product paths before creating an RC tag.
It generates RC configuration for the current release branch.
Do not add release branches to `.releaserc.json`; that file remains for stable releases from `main`.
The existing `release.yml` stable publisher is unchanged.

Each RC uses one version for these `components-dev` artifacts:

- Images: `auth-callout` and `dsx-agentgateway-bridge`, for fixed `linux/amd64` and `linux/arm64` platforms.
- Charts: `auth-callout`, `nats-event-bus`, and `dsx-agent-gateway`.

The caller sets the required `runner: linux-amd64-cpu4`; the wrapper has no runner default.
Image publishing requires Docker and a readable `/etc/buildkit/buildkitd.toml` on that runner.
Preflight checks these prerequisites before source tag creation.
The caller sets `environment: components-dev` and `ngc-path: 0837451325059433/components-dev`.
The `components-dev` environment must provide `NGC_DSX_COMPONENTS_PUSH_KEY` and restrict deployments to approved protected release branches.
Shared artifact jobs resolve this environment secret; the caller does not pass it through a workflow-level secret mapping.
The `nats-event-bus` manifest explicitly declares its local `auth-callout` dependency at `auth-callout/deploy`.
For a confirmed missing chart version, shared preparation aligns that dependency with the RC version before packaging.

Rerun the same workflow run after fixing an artifact failure, while its commit remains the release branch head.
Shared jobs verify existing artifacts against the RC source commit and publish only missing artifacts.
Chart jobs check a freshly fetched, authenticated NGC index before preparation and packaging.
Matching charts skip dependency resolution and packaging; lookup errors do not count as missing versions.
A mismatched artifact fails verification and is not overwritten.

Exchange passes its repository-scoped `RELEASE_DEPLOY_KEY` as `release-deploy-key` to the wrapper.
The wrapper forwards this optional secret only to the source publisher because Exchange tag rules authorize Deploy Keys.
The key handles Git pushes; `GITHUB_TOKEN` still handles the GitHub Release API.
No tag protection rule is relaxed.

This workflow applies to new release branches created from `main`.
Existing release branches retain their checked-in workflows.
Do not copy this workflow into an older branch without disabling that branch's previous RC publisher.
PR validation runs shared contract checks and product manifest validation through copy-pr-bot without publishing or using the publishing environment.
The shared `release-candidate.yml` remains available as a source-only workflow; it does not publish NGC artifacts.

Refer to the [RC onboarding guide](https://github.com/dsx-ai-factory/dsx-github-actions/blob/main/docs/release-candidate-publishing.md) for manifests, access policy, artifact verification, and first-RC acceptance.
Local checks do not establish that a real RC publication or rerun has passed.

### Roadmap

Upcoming work is tracked in [GitHub Issues](https://github.com/dsx-ai-factory/dsx-exchange/issues). See [CONTRIBUTING.md](CONTRIBUTING.md) for how to get involved.

## Contribution Guidelines

- Start here: [CONTRIBUTING.md](CONTRIBUTING.md)
- Code of Conduct: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)

Development quickstart:

```bash
git clone https://github.com/dsx-ai-factory/dsx-exchange.git
cd dsx-exchange
mise install --locked
make test
```

## Governance & Maintainers

- Governance: [GOVERNANCE.md](GOVERNANCE.md)
- Maintainers: [MAINTAINERS.md](MAINTAINERS.md)
- Triage policy: use GitHub issue labels and pull request review from repository maintainers.

## Security

- Vulnerability disclosure: [SECURITY.md](SECURITY.md)
- Do not file public issues for security reports.

## Support

- Support level: Maintained, with best-effort public issue triage.
- Help: file a GitHub issue with a focused reproduction or question.
- Response expectations: no guaranteed service-level agreement.

See [SUPPORT.md](SUPPORT.md) for details.

## Community

Use GitHub issues and pull requests for public project discussion, bug reports, feature requests, and contribution review.

## References

- [NATS](https://nats.io/)
- [NATS auth callout](https://docs.nats.io/running-a-nats-service/configuration/securing_nats/auth_callout)
- [agentgateway](https://agentgateway.dev/)
- [Model Context Protocol](https://modelcontextprotocol.io/)
- [AsyncAPI](https://www.asyncapi.com/)
- [CloudEvents MQTT Protocol Binding](https://github.com/cloudevents/spec/blob/main/cloudevents/bindings/mqtt-protocol-binding.md)

## License

This project is licensed under the Apache License 2.0. See [LICENSE](LICENSE) for details.
