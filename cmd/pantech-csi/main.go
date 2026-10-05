package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/brimble/pantech-csi/internal/cloud"
	"github.com/brimble/pantech-csi/internal/driver"
	"github.com/brimble/pantech-csi/internal/mount"
)

func main() {
	config := driver.ConfigFromEnv()
	flag.StringVar(&config.Endpoint, "endpoint", config.Endpoint, "CSI endpoint, unix:// only")
	flag.Func("mode", "controller, node, or all", func(value string) error {
		config.Mode = driver.Mode(value)
		return nil
	})
	flag.Parse()

	if err := run(config); err != nil {
		slog.Error("driver stopped", "error", err)
		os.Exit(1)
	}
}

func run(config driver.Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if err := requireLinuxNode(config); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config.Logger.Info("starting pantech csi driver",
		"driver", config.DriverName,
		"version", config.Version,
		"mode", config.Mode,
		"endpoint", config.Endpoint,
		"api_url_configured", config.APIURL != "",
		"api_key_configured", config.APIKey != "",
		"fake_provider_enabled", config.AllowFake,
		"region_configured", config.Region != "",
		"availability_zone_configured", config.AvailabilityZone != "",
		"disk_offering_configured", config.DiskOffering != "",
	)

	provider, err := providerFromConfig(config)
	if err != nil {
		return err
	}
	server, err := driver.NewServer(config, provider, mount.New())
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	return server.Serve(ctx)
}

func requireLinuxNode(config driver.Config) error {
	if config.Mode == driver.ModeController {
		return nil
	}
	if runtime.GOOS == "linux" || config.AllowFake {
		return nil
	}
	return fmt.Errorf("node plugin requires linux unless PANTECH_ALLOW_FAKE=true")
}

func providerFromConfig(config driver.Config) (cloud.Cloud, error) {
	if config.AllowFake {
		return cloud.NewFake(cloud.InstanceMetadata{
			InstanceID:        "pantech-test-instance",
			AvailabilityZone:  config.AvailabilityZone,
			Region:            config.Region,
			MaxVolumesPerNode: 32,
		}), nil
	}
	return cloud.NewClient(cloud.ClientConfig{
		BaseURL:          config.APIURL,
		APIKey:           config.APIKey,
		Region:           config.Region,
		AvailabilityZone: config.AvailabilityZone,
	})
}
