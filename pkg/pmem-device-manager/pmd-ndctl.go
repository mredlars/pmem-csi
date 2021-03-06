package pmdmanager

import (
	"fmt"
	"sync"

	"github.com/intel/pmem-csi/pkg/ndctl"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/util/mount"
)

const (
	// 1 GB align in ndctl creation request has proven to be reliable.
	// Newer kernels may allow smaller alignment but we do not want to introduce kernel depenency.
	ndctlAlign uint64 = 1024 * 1024 * 1024
)

type pmemNdctl struct {
	ctx *ndctl.Context
}

var _ PmemDeviceManager = &pmemNdctl{}

// mutex to synchronize all ndctl calls
// https://github.com/pmem/ndctl/issues/96
// Once ndctl supports concurrent calls we need to revisit
// our locking strategy.
var ndctlMutex = &sync.Mutex{}

//NewPmemDeviceManagerNdctl Instantiates a new ndctl based pmem device manager
func NewPmemDeviceManagerNdctl() (PmemDeviceManager, error) {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()

	ctx, err := ndctl.NewContext()
	if err != nil {
		return nil, fmt.Errorf("Failed to initialize pmem context: %s", err.Error())
	}
	// Check is /sys writable. If not then there is no point starting
	mounter := mount.New("")
	mounts, err := mounter.List()
	for i := range mounts {
		klog.V(5).Infof("NewPmemDeviceManagerNdctl: Check mounts: device=%s path=%s opts=%s",
			mounts[i].Device, mounts[i].Path, mounts[i].Opts)
		if mounts[i].Device == "sysfs" && mounts[i].Path == "/sys" {
			for _, opt := range mounts[i].Opts {
				if opt == "rw" {
					klog.V(4).Infof("NewPmemDeviceManagerNdctl: /sys mounted read-write, good")
				} else if opt == "ro" {
					return nil, fmt.Errorf("FATAL: /sys mounted read-only, can not operate\n")
				}
			}
			break
		}
	}

	return &pmemNdctl{
		ctx: ctx,
	}, nil
}

func (pmem *pmemNdctl) GetCapacity() (map[string]uint64, error) {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()

	Capacity := map[string]uint64{}
	nsmodes := []ndctl.NamespaceMode{ndctl.FsdaxMode, ndctl.SectorMode}
	var capacity uint64
	for _, bus := range pmem.ctx.GetBuses() {
		for _, r := range bus.ActiveRegions() {
			realalign := ndctlAlign * r.InterleaveWays()
			available := r.MaxAvailableExtent()
			// align down, avoid claiming more than what we really can serve
			klog.V(4).Infof("GetCapacity: available before realalign: %d", available)
			available /= realalign
			available *= realalign
			klog.V(4).Infof("GetCapacity: available after realalign: %d", available)
			if available > capacity {
				capacity = available
			}
		}
	}
	// we set same capacity for all namespace modes
	// TODO: we should maintain all modes capacity when adding or subtracting
	// from upper layer, not done right now!!
	for _, nsmod := range nsmodes {
		Capacity[string(nsmod)] = capacity
	}
	return Capacity, nil
}

func (pmem *pmemNdctl) CreateDevice(volumeId string, size uint64, nsmode string) error {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()
	// Check that such volume does not exist. In certain error states, for example when
	// namespace creation works but device zeroing fails (missing /dev/pmemX.Y in container),
	// this function is asked to create new devices repeatedly, forcing running out of space.
	// Avoid device filling with garbage entries by returning error.
	// Overall, no point having more than one namespace with same name.
	_, err := pmem.getDevice(volumeId)
	if err == nil {
		klog.V(4).Infof("Device with name: %s already exists, refuse to create another", volumeId)
		return fmt.Errorf("CreateDevice: Failed: namespace with that name exists")
	}
	// libndctl needs to store meta data and will use some of the allocated
	// space for that (https://github.com/pmem/ndctl/issues/79).
	// We don't know exactly how much space that is, just
	// that it should be a small amount. But because libndctl
	// rounds up to the alignment, in practice that means we need
	// to request `align` additional bytes.
	size += ndctlAlign
	klog.V(4).Infof("Compensate for libndctl creating one alignment step smaller: increase size to %d", size)
	ns, err := pmem.ctx.CreateNamespace(ndctl.CreateNamespaceOpts{
		Name:  volumeId,
		Size:  size,
		Align: ndctlAlign,
		Mode:  ndctl.NamespaceMode(nsmode),
	})
	if err != nil {
		return err
	}
	data, _ := ns.MarshalJSON() //nolint: gosec
	klog.V(3).Infof("Namespace created: %s", data)
	// clear start of device to avoid old data being recognized as file system
	device, err := pmem.getDevice(volumeId)
	if err != nil {
		return err
	}
	err = ClearDevice(device, false)
	if err != nil {
		return err
	}

	return nil
}

func (pmem *pmemNdctl) DeleteDevice(volumeId string, flush bool) error {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()

	device, err := pmem.getDevice(volumeId)
	if err != nil {
		return err
	}
	err = ClearDevice(device, flush)
	if err != nil {
		return err
	}
	return pmem.ctx.DestroyNamespaceByName(volumeId)
}

func (pmem *pmemNdctl) FlushDeviceData(volumeId string) error {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()

	device, err := pmem.getDevice(volumeId)
	if err != nil {
		return err
	}
	return ClearDevice(device, true)
}

func (pmem *pmemNdctl) GetDevice(volumeId string) (PmemDeviceInfo, error) {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()

	return pmem.getDevice(volumeId)
}

func (pmem *pmemNdctl) ListDevices() ([]PmemDeviceInfo, error) {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()

	devices := []PmemDeviceInfo{}
	for _, ns := range pmem.ctx.GetActiveNamespaces() {
		devices = append(devices, namespaceToPmemInfo(ns))
	}
	return devices, nil
}

func (pmem *pmemNdctl) getDevice(volumeId string) (PmemDeviceInfo, error) {
	ns, err := pmem.ctx.GetNamespaceByName(volumeId)
	if err != nil {
		return PmemDeviceInfo{}, err
	}

	return namespaceToPmemInfo(ns), nil
}

func namespaceToPmemInfo(ns *ndctl.Namespace) PmemDeviceInfo {
	return PmemDeviceInfo{
		VolumeId: ns.Name(),
		Path:     "/dev/" + ns.BlockDeviceName(),
		Size:     ns.Size(),
	}
}
