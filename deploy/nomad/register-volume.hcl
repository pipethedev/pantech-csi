type        = "csi"
id          = "existing-pantech-volume"
name        = "existing-pantech-volume"
external_id = "replace-with-pantech-volume-id"
plugin_id   = "csi.pantechdynamics.com"

capacity_min = "10GiB"
capacity_max = "10GiB"

capability {
  access_mode     = "single-node-writer"
  attachment_mode = "file-system"
}

mount_options {
  fs_type = "ext4"
}

context {
  availability_zone  = "replace-me"
  disk_offering_slug = "replace-me"
}
