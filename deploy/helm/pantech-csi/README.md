# Pantech CSI Helm Chart

Install the Pantech CSI driver on Kubernetes 1.28+.

```sh
helm install pantech-csi ./deploy/helm/pantech-csi \
  --namespace kube-system \
  --set pantech.region="$PANTECH_REGION" \
  --set pantech.availabilityZone="$PANTECH_AVAILABILITY_ZONE" \
  --set pantech.diskOffering="$PANTECH_DISK_OFFERING" \
  --set pantech.apiKey="$PANTECH_API_KEY"
```

Use an existing Secret instead of putting credentials in Helm values:

```sh
kubectl -n kube-system create secret generic pantech-csi \
  --from-literal=PANTECH_API_URL=https://api.pantechdynamics.com/public/v1 \
  --from-literal=PANTECH_API_KEY="$PANTECH_API_KEY" \
  --from-literal=PANTECH_REGION="$PANTECH_REGION" \
  --from-literal=PANTECH_AVAILABILITY_ZONE="$PANTECH_AVAILABILITY_ZONE" \
  --from-literal=PANTECH_DISK_OFFERING="$PANTECH_DISK_OFFERING"

helm install pantech-csi ./deploy/helm/pantech-csi \
  --namespace kube-system \
  --set pantech.existingSecret=pantech-csi \
  --set pantech.region="$PANTECH_REGION" \
  --set pantech.availabilityZone="$PANTECH_AVAILABILITY_ZONE" \
  --set pantech.diskOffering="$PANTECH_DISK_OFFERING"
```
