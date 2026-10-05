# Pantech CSI Driver

`pantech-csi` is a Container Storage Interface driver for Pantech Cloud block storage. It is built as one binary that can run as a Nomad controller, Nomad node plugin, Nomad monolith plugin, or Kubernetes CSI driver.

## Pantech API Reference

The provider integration is based on the [Pantech Dynamics public API](https://docs.pantechdynamics.com/api). The short contract is the [agent brief](https://docs.pantechdynamics.com/agent-brief.md), and the OpenAPI document is [public-api.yaml](https://docs.pantechdynamics.com/openapi/public-api.yaml).

Authentication uses a pre-issued API key (`PAN_…`) sent as `Authorization: Bearer <api-key>`. The driver does not create API keys or buy credit.

## Build

```sh
go build ./...
go test -race ./...
golangci-lint run
```

Build the Linux binary for containers:

```sh
GOOS=linux GOARCH=amd64 go build -o pantech-csi ./cmd/pantech-csi
```

## CI/CD

GitHub Actions runs build, vet, race tests, and lint on pull requests and pushes. Pushes to `main` publish these images to GitHub Container Registry using the built-in `GITHUB_TOKEN`:

```text
ghcr.io/pipethedev/pantech-csi:latest-controller
ghcr.io/pipethedev/pantech-csi:latest-node
```

Both images are published for `linux/amd64` and `linux/arm64`.

To also deploy the Nomad CSI jobs on every `main` push, configure these repository settings:

| Type | Name | Description |
|---|---|---|
| Secret | `NOMAD_ADDR` | Nomad API address reachable from GitHub Actions. |
| Secret | `NOMAD_TOKEN` | Nomad token allowed to run the CSI jobs. |
| Variable | `NOMAD_DEPLOY` | Set to `true` to enable deploys. |
| Variable | `PANTECH_REGION` | Pantech region rendered into the jobspecs. |
| Variable | `PANTECH_AVAILABILITY_ZONE` | Pantech zone rendered into the jobspecs. |
| Variable | `PANTECH_DISK_OFFERING` | Disk offering slug rendered into the jobspecs. |

The Pantech API key lives in Nomad itself via `nomad var put pantech/csi api_key=...`; it is not stored in GitHub Actions.

## Runtime Config

| Name | Required | Default | Description |
|---|---:|---|---|
| `CSI_ENDPOINT` | no | `unix:///csi/csi.sock` | CSI unix socket endpoint. `tcp://` is rejected. |
| `PANTECH_API_URL` | yes for real provider | none | Pantech API base URL, for example `https://api.pantechdynamics.com/public/v1`. |
| `PANTECH_API_KEY` | yes for real provider | none | Pre-issued Pantech API key sent as a bearer credential. |
| `PANTECH_REGION` | no | none | Region sent when creating a volume. The platform default is used when this is empty. |
| `PANTECH_AVAILABILITY_ZONE` | yes | none | Zone used for topology and for name reconciliation. |
| `PANTECH_DISK_OFFERING` | yes for real creates | none | Default `disk_offering_slug` from `GET /disk-offerings`. |
| `PANTECH_ALLOW_FAKE` | no | `false` | Enables the in-memory fake provider and allows node/all mode on non-Linux hosts for local tests only. Without this flag, node/all mode refuses to start on non-Linux so a real provider cannot be paired with a fake mounter. Do not set this in Nomad or Kubernetes. |
| `PANTECH_DRIVER_NAME` | no | `csi.pantechdynamics.com` | CSI driver name. |
| `PANTECH_DRIVER_VERSION` | no | `0.1.0` | Version shown by Nomad and Kubernetes health tooling. |

## Nomad

Nomad is the primary target. Use the Docker task driver for node plugins because the driver needs host block-device access.

```sh
nomad job run deploy/nomad/controller.nomad.hcl
nomad job run deploy/nomad/node.nomad.hcl
nomad plugin status csi.pantechdynamics.com
nomad volume create deploy/nomad/example-volume.hcl
nomad volume register deploy/nomad/register-volume.hcl
```

Replace the `replace-me` placeholders, including `PANTECH_API_KEY`. `nomad var put pantech/csi api_key="$PANTECH_API_KEY"` overrides the jobspec value. Use `stage_publish_base_dir` with a non-default path in tests. For single-writer volumes, avoid rolling update settings that force a new allocation to claim the same volume before the old allocation releases it.

### Restart Safety

CSI plugins are not in the data path after a volume is mounted. A Nomad client-agent restart must not detach a still-running workload. Keep the Nomad client `data_dir` on persistent host storage so volume claims and mount metadata survive the restart.

The controller and node plugin jobs already keep their allocations during a client disconnect. Every stateful workload group that uses this driver should do the same:

```hcl
disconnect {
  lost_after = "24h"
  replace    = false
  reconcile  = "keep_original"
}
```

Drain a node with `nomad node drain` before an intentional shutdown so plugins unmount and unpublish after the workload stops. If a node is permanently lost, fence it and migrate the volume by hand. Nomad will not automatically move a single-writer volume that may still be mounted.

## Kubernetes

The Kubernetes manifests are under `deploy/kubernetes`. They use CSI sidecars, but the driver itself does not import Kubernetes clients or depend on Kubernetes-only volume context.

```sh
kubectl apply -k deploy/kubernetes
```

Create the `pantech-csi` Secret with `PANTECH_API_KEY`.

The Helm chart is under `deploy/helm/pantech-csi`:

```sh
kubectl -n kube-system create secret generic pantech-csi \
  --from-literal=PANTECH_API_URL=https://api.pantechdynamics.com/public/v1 \
  --from-literal=PANTECH_API_KEY="$PANTECH_API_KEY" \
  --from-literal=PANTECH_REGION="$PANTECH_REGION" \
  --from-literal=PANTECH_AVAILABILITY_ZONE="$PANTECH_AVAILABILITY_ZONE" \
  --from-literal=PANTECH_DISK_OFFERING="$PANTECH_DISK_OFFERING"

helm install pantech-csi deploy/helm/pantech-csi \
  --namespace kube-system \
  --set pantech.existingSecret=pantech-csi \
  --set pantech.region="$PANTECH_REGION" \
  --set pantech.availabilityZone="$PANTECH_AVAILABILITY_ZONE" \
  --set pantech.diskOffering="$PANTECH_DISK_OFFERING"
```

## Supported Parameters

| Key | Type | Default | Mutable | Description |
|---|---|---|---:|---|
| `availability_zone` | string | `PANTECH_AVAILABILITY_ZONE` | no | Pantech zone used for topology. |
| `disk_offering_slug` | string | `PANTECH_DISK_OFFERING` | no | Disk offering from `GET /disk-offerings`. Required to create a volume. |
| `region` | string | `PANTECH_REGION` | no | Region sent on create. Optional. |

Unknown parameters are rejected with `INVALID_ARGUMENT`.

## Provider behavior

Writes return `202` with an `operation_id`. The client sends a new `Idempotency-Key` per change and polls `GET /operations/{operation_id}` until the operation succeeds or fails. A retry of the same call reuses that key.

`GET /volumes/{volume_id}` reads one volume. List calls follow `next_cursor`, with at most 100 items per page. CSI list pages pass that cursor through. Name reconciliation walks every page, because `q` is a substring match, then keeps the exact name in the requested zone. `GET /snapshots` also returns instance root-disk snapshots; this driver keeps only snapshots that have a `volume_id`.

Creating a volume calls `POST /volumes`. Creating one from a snapshot calls `POST /snapshots/{snapshot_id}/restore`. Snapshots of a volume call `POST /volumes/{volume_id}/snapshots`. Snapshot delete is `DELETE /snapshots/{snapshot_id}` and does not need a zone. When a zone is known, CSI snapshot IDs are still `zone/snapshot-id` so a later delete can recover the provider id.

Pantech volumes attach to one instance and do not return a guest device path. Publish context uses `/dev/disk/by-id/virtio-<volume id>`. The node plugin also tries the QEMU SCSI by-id name.

The public API does not offer conditional create by name. After create, the driver re-lists the name, waits until the lowest compatible id stays stable, and deletes visible unattached duplicates when that is safe. This is best-effort reconciliation.

## Node identity

The public API has no “current instance” endpoint. The node plugin reads cloud-init instance data from `/run/cloud-init/instance-data.json` or `/var/lib/cloud/instance/instance-data.json`. If neither file has an instance ID, `NodeGetInfo` fails.
