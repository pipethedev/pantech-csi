package driver

import (
	"github.com/brimble/pantech-csi/internal/cloud"
	"github.com/brimble/pantech-csi/internal/mount"
	"github.com/container-storage-interface/spec/lib/go/csi"
)

type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	config Config
	cloud  cloud.Cloud
	mount  mount.Mounter
	locks  *VolumeLocks
	names  *VolumeLocks
}

func New(config Config, provider cloud.Cloud, mounter mount.Mounter) *Driver {
	return &Driver{
		config: config,
		cloud:  provider,
		mount:  mounter,
		locks:  NewVolumeLocks(),
		names:  NewVolumeLocks(),
	}
}
