//go:build !windows

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/docker/docker/daemon/container"
	"github.com/docker/docker/daemon/internal/idtools"
	"github.com/docker/docker/daemon/pkg/oci"
	volumemounts "github.com/docker/docker/daemon/volume/mounts"
	volumeopts "github.com/docker/docker/daemon/volume/service/opts"
	containertypes "github.com/moby/moby/api/types/container"
	mounttypes "github.com/moby/moby/api/types/mount"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
)

// createContainerOSSpecificSettings performs host-OS specific container create functionality
func (daemon *Daemon) createContainerOSSpecificSettings(ctx context.Context, container *container.Container, config *containertypes.Config, hostConfig *containertypes.HostConfig) error {
	if err := daemon.Mount(container); err != nil {
		return err
	}
	defer daemon.Unmount(container)

	if err := container.SetupWorkingDirectory(daemon.idMapping.RootPair()); err != nil {
		return err
	}

	// Set the default masked and readonly paths with regard to the host config options if they are not set.
	if hostConfig.MaskedPaths == nil && !hostConfig.Privileged {
		hostConfig.MaskedPaths = oci.DefaultSpec().Linux.MaskedPaths // Set it to the default if nil
		container.HostConfig.MaskedPaths = hostConfig.MaskedPaths
	}
	if hostConfig.ReadonlyPaths == nil && !hostConfig.Privileged {
		hostConfig.ReadonlyPaths = oci.DefaultSpec().Linux.ReadonlyPaths // Set it to the default if nil
		container.HostConfig.ReadonlyPaths = hostConfig.ReadonlyPaths
	}

	for spec := range config.Volumes {
		destination := filepath.Clean(spec)

		// Skip volumes for which we already have something mounted on that
		// destination because of a --volume-from.
		if container.HasMountFor(destination) {
			log.G(ctx).WithField("container", container.ID).WithField("destination", spec).Debug("mountpoint already exists, skipping anonymous volume")
			// Not an error, this could easily have come from the image config.
			continue
		}
		path, err := container.GetResourcePath(destination)
		if err != nil {
			return err
		}

		stat, err := os.Stat(path)
		if err == nil && !stat.IsDir() {
			return fmt.Errorf("cannot mount volume over existing file, file exists %s", path)
		}

		v, err := daemon.volumes.Create(context.TODO(), "", hostConfig.VolumeDriver, volumeopts.WithCreateReference(container.ID))
		if err != nil {
			return err
		}

		if err := label.Relabel(v.Mountpoint, container.MountLabel, true); err != nil {
			return err
		}

		container.AddMountPointWithVolume(destination, &volumeWrapper{v: v, s: daemon.volumes}, true)
	}
	return daemon.populateVolumes(ctx, container)
}

// populateVolumes copies data from the container's rootfs into the volume for non-binds.
// this is only called when the container is created.
func (daemon *Daemon) populateVolumes(ctx context.Context, c *container.Container) error {
	for _, mnt := range c.MountPoints {
		if mnt.Volume == nil {
			continue
		}

		if mnt.Type != mounttypes.TypeVolume || !mnt.CopyData {
			continue
		}

		if err := daemon.populateVolume(ctx, c, mnt); err != nil {
			return err
		}
	}
	return nil
}

func (daemon *Daemon) populateVolume(ctx context.Context, c *container.Container, mnt *volumemounts.MountPoint) error {
	ctrDestPath, err := c.GetResourcePath(mnt.Destination)
	if err != nil {
		return err
	}

	if _, err := os.Stat(ctrDestPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	uid, gid := daemon.idMapping.RootPair()
	volumePath, cleanup, err := mnt.Setup(ctx, c.MountLabel, idtools.Identity{UID: uid, GID: gid}, nil)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		log.G(ctx).WithError(err).Debugf("can't copy data from %s:%s, to %s", c.ID, mnt.Destination, volumePath)
		return errors.Wrapf(err, "failed to populate volume")
	}
	defer func() {
		ctx := context.WithoutCancel(ctx)
		_ = cleanup(ctx)
		_ = mnt.Cleanup(ctx)
	}()

	log.G(ctx).Debugf("copying image data from %s:%s, to %s", c.ID, mnt.Destination, volumePath)
	if err := c.CopyImagePathContent(volumePath, ctrDestPath); err != nil {
		return err
	}

	return nil
}
