# How to create a Catalog

This page explains how to create a custom catalog for your multi cluster platform with the `kubara catalog` command group.

If you want to understand the concepts behind catalogs and how they work in detail, please check the [Catalog concepts page](../1_getting_started/catalogs.md).

## When you need a custom catalog at all

You usually need a custom catalog when you want to:

- add a service in need on multiple clusters which kubara does not ship
- replace a built-in chart with an internal one
- change service defaults across all generated clusters
- ship reusable platform templates outside the kubara source tree

If you only want to customize values for a generated cluster, editing files in `customer-service-catalog/` is usually enough. A custom catalog is the right tool when you want to change the **service model** or the **templates kubara renders**.


## Create the catalog

Start by scaffolding the catalog root:

```bash
kubara catalog create my-catalog
```

This creates a new directory named after the catalog. The name must follow RFC 1123 naming rules: lowercase letters, digits, and `-`, starting with a letter and ending with a letter or digit.

The generated layout is:

```text
my-catalog/
├── Catalog.yaml
├── services/
├── managed-service-catalog/
│   ├── helm/
│   └── terraform/
└── customer-service-catalog/
    ├── helm/
    │   └── example/
    └── terraform/
        └── example/
```

The created `Catalog.yaml` looks like this:

```yaml
apiVersion: kubara.io/v1alpha1
kind: Catalog
metadata:
  name: my-catalog
spec:
  version: 0.1.0
```

`spec.version` is the OCI distribution version and must use exact `x.y.z` format without a leading `v`.

## Add a service

Change into the catalog root and add a service definition:

```bash
cd my-catalog
kubara catalog add widget-dashboard
```

This command requires `Catalog.yaml` to be present in the current directory and creates `services/widget-dashboard.yaml`.

The generated service definition looks like this:

```yaml
apiVersion: kubara.io/v1alpha1
kind: ServiceDefinition
metadata:
  name: widget-dashboard
spec:
  chartPath: widget-dashboard
  status: disabled
  clusterTypes:
    - hub
    - spoke
```

## Continue with kubara commands

After creating the catalog and its services, use the catalog by passing the catalog root to kubara commands:

```bash
kubara schema --catalog ./my-catalog
kubara init --catalog ./my-catalog
kubara generate --catalog ./my-catalog
```

You can also package and distribute the same catalog through OCI:

```bash
cd my-catalog
kubara catalog package
kubara catalog package oci://ghcr.io/example/catalogs/
kubara catalog push --insecure oci://ghcr.io/example/my-catalog:0.1.0
kubara catalog push --insecure --from oci://localhost/my-catalog:0.1.0 oci://ghcr.io/example/catalogs/my-catalog:0.1.0
kubara schema --catalog oci://ghcr.io/example/my-catalog:0.1.0
kubara generate --catalog oci://ghcr.io/example/my-catalog:0.1.0
kubara catalog list
kubara catalog unpackage oci://localhost/my-catalog:0.1.0
```

For private registries, add `--registry-config /path/to/config.json`. kubara uses `~/.docker/config.json` by default and reuses the local cache under `~/.kubara/catalogs` for OCI-backed catalogs. If your registry has certificate issues that you still want to ignore, add `--insecure` to `kubara catalog pull` and `kubara catalog push`. To refresh a cached tagged catalog, run:

```bash
kubara catalog pull --force --insecure oci://ghcr.io/example/my-catalog:0.1.0
```

To materialize a cached OCI catalog into an editable directory, run:

```bash
kubara catalog unpackage oci://localhost/my-catalog:0.1.0
```

If you omit the package base, kubara stores the packaged catalog under `oci://localhost/<catalog-name>:<spec.version>`. If you want a different local reference, pass a base such as `oci://ghcr.io/example/catalogs/` to `kubara catalog package`.

If you omit the output directory, kubara creates one named after the catalog in your current working directory.

If you want to promote or rename an already cached catalog without repackaging from a working directory, use:

```bash
kubara catalog push --insecure --from oci://localhost/my-catalog:0.1.0 oci://ghcr.io/example/catalogs/my-catalog:0.1.0
```

When the source ref is not cached yet, kubara pulls it first and then pushes it to the target reference.

## Extending the catalog

For a **new** service that does not exist in the built-in catalog, you normally still need both:

- a `ServiceDefinition`
- matching template content under `managed-service-catalog/` and/or `customer-service-catalog/`

If you only create the `ServiceDefinition`, kubara can understand the service metadata, but it still needs actual templates to render useful output for that service.

## Overriding a built-in service

You can also override built-in services by reusing the same `metadata.name`.

Typical reasons:

- change the default `status`
- change the `chartPath`
- provide a different `configSchema`
- replace the built-in chart/templates with your own

Without `--catalog-overwrite`, kubara rejects the collision. With `--catalog-overwrite`, your external definition replaces the built-in one for that service name.

## Practical guidance

- Point `--catalog` at the **catalog root**.
- Use `oci://...` values with `--catalog` when you want kubara to resolve the catalog from the OCI cache or pull it automatically.
- Use `kubara catalog create` and `kubara catalog add` as the entrypoint for catalog work.
- Keep `metadata.name` stable and canonical.
- Keep `spec.version` aligned with the OCI tag you push.
- Keep `chartPath` aligned with the chart directory name under `managed-service-catalog/helm/`.
- Use `configSchema` for defaults and validation instead of documenting required values only in prose.
- Treat generated files in your repo as output; treat the external catalog as the maintainable source.
