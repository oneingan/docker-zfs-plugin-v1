package zfsdriver

import (
	"fmt"
	"strings"
	"time"

	"github.com/clinta/go-zfs"
	"github.com/docker/go-plugins-helpers/volume"
	"go.uber.org/zap"
	linuxproc "github.com/c9s/goprocinfo/linux"
)

// ZfsDriver implements the plugin helpers volume.Driver interface for zfs
type ZfsDriver struct {
	volume.Driver
	rds []*zfs.Dataset // root dataset
}

// NewZfsDriver returns the plugin driver object
func NewZfsDriver(dss ...string) (*ZfsDriver, error) {
	zap.L().Debug("creating new ZFSDriver")

	zd := &ZfsDriver{}
	if len(dss) < 1 {
		return nil, fmt.Errorf("No datasets specified")
	}
	for _, ds := range dss {
		if !zfs.DatasetExists(ds) {
			_, err := zfs.CreateDatasetRecursive(ds, make(map[string]string))
			if err != nil {
				zap.L().Error("failed to create root dataset", zap.String("ds", ds), zap.Error(err))
				return nil, err
			}
		}
		rds, err := zfs.GetDataset(ds)
		if err != nil {
			zap.L().Error("failed to get root dataset", zap.String("ds", ds), zap.Error(err))
			return nil, err
		}
		zd.rds = append(zd.rds, rds)
	}

	return zd, nil
}

// isRootDatasetDefined checks if name is a child of any defined root dataset in this driver
// instance.
func (zd *ZfsDriver) isRootDatasetDefined(name string) (isValid bool) {
	isValid = false
	for _, rds := range zd.rds {
		if strings.HasPrefix(name, rds.Name + "/") {
			isValid = true
			return
		}
	}
	return
}

// Create creates a new zfs dataset for a volume
func (zd *ZfsDriver) Create(req *volume.CreateRequest) error {
	zap.L().Debug("Create", zap.String("Name", req.Name), zap.Reflect("Options", req.Options))

	// Check root dataset
	if !zd.isRootDatasetDefined(req.Name) {
		return fmt.Errorf("invalid parent dataset")
	}

	if zfs.DatasetExists(req.Name) {
		return fmt.Errorf("volume already exists")
	}

	_, err := zfs.CreateDatasetRecursive(req.Name, req.Options)
	return err
}

// List returns a list of zfs volumes on this host
func (zd *ZfsDriver) List() (*volume.ListResponse, error) {
	zap.L().Debug("List")
	var vols []*volume.Volume

	for _, rds := range zd.rds {
		dsl, err := rds.DatasetList()
		if err != nil {
			return nil, err
		}
		for _, ds := range dsl {
			//TODO: rewrite this to utilize zd.getVolume() when
			//upstream go-zfs is rewritten to cache properties
			var mp string
			mp, err = ds.GetMountpoint()
			if err != nil {
				zap.L().Error("failed to get mountpoint from dataset", zap.String("Name", ds.Name))
				continue
			}
			vols = append(vols, &volume.Volume{Name: ds.Name, Mountpoint: mp})
		}
	}

	return &volume.ListResponse{Volumes: vols}, nil
}

// Get returns the volume.Volume{} object for the requested volume
// nolint: dupl
func (zd *ZfsDriver) Get(req *volume.GetRequest) (*volume.GetResponse, error) {
	zap.L().Debug("Get", zap.String("Name", req.Name))

	v, err := zd.getVolume(req.Name)
	if err != nil {
		return nil, err
	}

	return &volume.GetResponse{Volume: v}, nil
}

func (zd *ZfsDriver) getVolume(name string) (*volume.Volume, error) {
	ds, err := zfs.GetDataset(name)
	if err != nil {
		return nil, err
	}

	mp, err := ds.GetMountpoint()
	if err != nil {
		return nil, err
	}

	ts, err := ds.GetCreation()
	if err != nil {
		zap.L().Error("failed to get creation property from zfs dataset", zap.Error(err))
		return &volume.Volume{Name: name, Mountpoint: mp}, nil
	}

	return &volume.Volume{Name: name, Mountpoint: mp, CreatedAt: ts.Format(time.RFC3339)}, nil
}

func (zd *ZfsDriver) getMP(name string) (string, error) {
	ds, err := zfs.GetDataset(name)
	if err != nil {
		return "", err
	}

	mp, err := ds.GetMountpoint()
	if err != nil {
		return "", fmt.Errorf("Unable to get dataset '%s' mount point: %w", name, err)
	}

	if mp == "none" {
		// Parent dataset must be mounted somewhere before being able to bind them
		// to containers first
		zap.L().Debug("dataset has no mount point", zap.String("Name", name))
		return "", fmt.Errorf("Dataset '%s' (or its parent) does not have mount point", name)
	}
	if mp == "legacy" {
		// Must look up dataset mount point manually
		// TODO: cache?
		// TODO: auto mount?
		zap.L().Warn("dataset has legacy mount point and requires lookup", zap.String("Name", name))
		mounts, err := linuxproc.ReadMounts("/proc/mounts")
		if err != nil {
			return "", fmt.Errorf("Unable to read /proc/mounts to figure out dataset '%s' mount point: %w", name, err)
		}
		for _, mnt := range mounts.Mounts {
			if mnt.FSType == "zfs" && mnt.Device == name {
				return mnt.MountPoint, nil
			}
		}
		return "", fmt.Errorf("Dataset '%s' has legacy mountpoint, but it is not mounted", name)
	}

	return mp, nil
}

// Remove destroys a zfs dataset for a volume
func (zd *ZfsDriver) Remove(req *volume.RemoveRequest) error {
	zap.L().Debug("Remove", zap.String("Name", req.Name))

	// Check root dataset
	if !zd.isRootDatasetDefined(req.Name) {
		return fmt.Errorf("invalid parent dataset")
	}

	ds, err := zfs.GetDataset(req.Name)
	if err != nil {
		return err
	}

	return ds.Destroy()
}

// Path returns the mountpoint of a volume
func (zd *ZfsDriver) Path(req *volume.PathRequest) (*volume.PathResponse, error) {
	zap.L().Debug("Path", zap.String("Name", req.Name))

	mp, err := zd.getMP(req.Name)
	if err != nil {
		return nil, err
	}

	return &volume.PathResponse{Mountpoint: mp}, nil
}

// Mount returns the mountpoint of the zfs volume
func (zd *ZfsDriver) Mount(req *volume.MountRequest) (*volume.MountResponse, error) {
	zap.L().Debug("Mount", zap.String("ID", req.ID), zap.String("Name", req.Name))
	mp, err := zd.getMP(req.Name)
	if err != nil {
		zap.L().Error("failed to get dataset mount point", zap.String("Name", req.Name), zap.Error(err))
		return nil, err
	}

	zap.L().Debug("dataset mountpoint", zap.String("Name", req.Name), zap.String("mountpoint", mp))

	return &volume.MountResponse{Mountpoint: mp}, nil
}

// Unmount does nothing because a zfs dataset need not be unmounted
func (zd *ZfsDriver) Unmount(req *volume.UnmountRequest) error {
	zap.L().Debug("Unmount", zap.String("ID", req.ID), zap.String("Name", req.Name))
	return nil
}

//Capabilities sets the scope to local as this is a local only driver
func (zd *ZfsDriver) Capabilities() *volume.CapabilitiesResponse {
	zap.L().Debug("Capabilities")
	return &volume.CapabilitiesResponse{Capabilities: volume.Capability{Scope: "local"}}
}
