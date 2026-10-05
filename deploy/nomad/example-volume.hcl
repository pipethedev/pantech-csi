type      = "csi"
id        = "example-pantech-volume"
name      = "example-pantech-volume"
plugin_id = "csi.pantechdynamics.com"

capacity_min = "10GiB"
capacity_max = "10GiB"

capability {
  access_mode     = "single-node-writer"
  attachment_mode = "file-system"
}

mount_options {
  fs_type = "ext4"
}

parameters {
  availability_zone   = "replace-me"
  disk_offering_slug  = "replace-me"
  region              = "replace-me"
}
