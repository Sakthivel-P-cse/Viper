# Kubernetes production template

`viper.yaml` is a hardened single-region starting template for a three-node
cluster. Before applying it:

- Replace the image with a signed, digest-pinned release image.
- Create the `viper-tls` Secret with the exact peer, client-server, client,
  administrator, and CA filenames referenced by the manifest.
- Ensure each peer certificate contains the matching
  `urn:viper:node:node-<n>` URI SAN.
- Label the client and monitoring namespaces with
  `viper-client-access=true` and `viper-metrics-access=true` as appropriate.
- Set a storage class with encrypted persistent volumes and verify its fsync
  and failure semantics.
- Keep `updateStrategy: OnDelete` until mixed-version compatibility is tested;
  the current project supports maintenance upgrades only.
- Replace the TCP readiness probe with an authenticated readiness check in the
  deployment environment. A listening port alone does not prove quorum.

Validate the manifest without contacting a cluster:

```text
kubectl apply --dry-run=client -f deploy/kubernetes/viper.yaml
```

This template does not create a CA or production secret. Use the organization's
certificate authority and secret manager for those materials.
