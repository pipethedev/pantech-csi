job "pantech-csi-controller" {
  datacenters = ["dc1"]
  type        = "service"

  group "controller" {
    count = 2

    constraint {
      operator = "distinct_hosts"
      value    = "true"
    }

    disconnect {
      lost_after = "24h"
      replace    = false
      reconcile  = "keep_original"
    }

    task "plugin" {
      driver = "docker"
      user   = "root"

      config {
        image      = "ghcr.io/pipethedev/pantech-csi:latest-controller"
        force_pull = true
        args = [
          "-endpoint=unix:///csi/csi.sock",
          "-mode=controller",
        ]
      }

      env {
        PANTECH_API_URL           = "https://api.pantechdynamics.com/public/v1"
        PANTECH_API_KEY           = "replace-me"
        PANTECH_REGION            = "replace-me"
        PANTECH_AVAILABILITY_ZONE = "replace-me"
        PANTECH_DISK_OFFERING     = "replace-me"
      }

      template {
        destination = "secrets/pantech.env"
        env         = true
        data        = <<EOH
{{- with nomadVar "pantech/csi" }}PANTECH_API_KEY={{ .api_key }}{{ end -}}
EOH
      }

      csi_plugin {
        id                     = "csi.pantechdynamics.com"
        type                   = "controller"
        mount_dir              = "/csi"
        stage_publish_base_dir = "/local/csi"
        health_timeout         = "30s"
      }
    }
  }
}
