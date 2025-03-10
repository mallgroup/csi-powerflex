package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/dell/gofsutil"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Variables set only for unit testing.
var unitTestEmulateBlockDevice bool

// Variables populdated from the environment
var mountAllowRWOMultiPodAccess bool

// Device is a struct for holding details about a block device
type Device struct {
	FullPath string
	Name     string
	RealDev  string
}

// GetDevice returns a Device struct with info about the given device, or
// an error if it doesn't exist or is not a block device
func GetDevice(path string) (*Device, error) {

	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}

	// eval any symlinks and make sure it points to a device
	d, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}

	// TODO does EvalSymlinks throw error if link is to non-
	// existent file? assuming so by masking error below
	ds, _ := os.Stat(d)
	dm := ds.Mode()
	if unitTestEmulateBlockDevice {
		// For unit testing only, emulate a block device on windows
		dm = dm | os.ModeDevice
	}
	if dm&os.ModeDevice == 0 {
		return nil, fmt.Errorf(
			"%s is not a block device", path)
	}

	return &Device{
		Name:     fi.Name(),
		FullPath: replaceBackslashWithSlash(path),
		RealDev:  replaceBackslashWithSlash(d),
	}, nil
}

// publishVolume uses the parameters in req to bindmount the underlying block
// device to the requested target path. A private mount is performed first
// within the given privDir directory.
//
// publishVolume handles both Mount and Block access types
func publishVolume(
	req *csi.NodePublishVolumeRequest,
	privDir, device string, reqID string) error {

	id := req.GetVolumeId()

	target := req.GetTargetPath()
	if target == "" {
		return status.Error(codes.InvalidArgument,
			"target path required")
	}

	ro := req.GetReadonly()

	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return status.Error(codes.InvalidArgument,
			"volume capability required")
	}

	// make sure device is valid
	sysDevice, err := GetDevice(device)
	if err != nil {
		return status.Errorf(codes.Internal,
			"error getting block device for volume: %s, err: %s",
			id, err.Error())
	}

	isBlock, mntVol, accMode, multiAccessFlag, err := validateVolumeCapability(volCap, ro)
	if err != nil {
		return err
	}

	// Make sure target is created. The spec says the driver is responsible
	// for creating the target, but Kubernetes generallly creates the target.
	privTgt := getPrivateMountPoint(privDir, id)
	err = createTarget(target, isBlock)
	if err != nil {
		// Unmount and remove the private directory for the retry so clean start next time.
		// K8S probably removed part of the path.
		cleanupPrivateTarget(reqID, privTgt)
		return status.Error(codes.FailedPrecondition, fmt.Sprintf("Could not create %s: %s", target, err.Error()))
	}

	// make sure privDir exists and is a directory
	if _, err := mkdir(privDir); err != nil {
		return err
	}

	// Handle block as a short cut
	if isBlock {
		// BLOCK only
		mntFlags := mntVol.GetMountFlags()
		err = mountBlock(sysDevice, target, mntFlags, singleAccessMode(accMode))
		return err
	}

	// check that target is right type for vol type
	// Path to mount device to

	f := log.Fields{
		"id":           id,
		"volumePath":   sysDevice.FullPath,
		"device":       sysDevice.RealDev,
		"CSIRequestID": reqID,
		"target":       target,
		"privateMount": privTgt,
	}
	log.WithFields(f).Debugf("fields")

	ctx := context.WithValue(context.Background(), gofsutil.ContextKey("RequestID"), reqID)

	// Check if device is already mounted
	devMnts, err := getDevMounts(sysDevice)
	if err != nil {
		return status.Errorf(codes.Internal,
			"could not reliably determine existing mount status: %s",
			err.Error())
	}

	if len(devMnts) == 0 {
		// Device isn't mounted anywhere, do the private mount
		log.WithFields(f).Printf("attempting mount to private area")

		// Make sure private mount point exists
		created, err := mkdir(privTgt)
		if err != nil {
			return status.Errorf(codes.Internal,
				"Unable to create private mount point: %s",
				err.Error())
		}
		alreadyMounted := false
		if !created {
			log.WithFields(f).Printf("private mount target already exists")

			// The place where our device is supposed to be mounted
			// already exists, but we also know that our device is not mounted anywhere
			// Either something didn't clean up correctly, or something else is mounted
			// If the private mount is not in use, it's okay to re-use it. But make sure
			// it's not in use first

			mnts, err := gofsutil.GetMounts(ctx)
			if err != nil {
				return status.Errorf(codes.Internal,
					"could not reliably determine existing mount status: %s",
					err.Error())
			}
			if len(mnts) == 0 {
				return status.Errorf(codes.Unavailable, "volume %s not published to node", id)
			}
			for _, m := range mnts {
				if m.Path == privTgt {
					log.Debug(fmt.Sprintf("MOUNT: %#v", m))
					resolvedMountDevice := evalSymlinks(m.Device)
					if resolvedMountDevice != sysDevice.RealDev {
						log.WithFields(f).WithField("mountedDevice", m.Device).Error(
							"mount point already in use by device")
						return status.Error(codes.Internal,
							"Mount point already in use by device")
					}
					alreadyMounted = true
				}
			}
		}

		if !alreadyMounted {
			fs := mntVol.GetFsType()
			mntFlags := mntVol.GetMountFlags()
			if fs == "xfs" {
				mntFlags = append(mntFlags, "nouuid")
			}
			if err := handlePrivFSMount(
				ctx, accMode, sysDevice, mntFlags, fs, privTgt); err != nil {
				// K8S may have removed the desired mount point. Clean up the private target.
				cleanupPrivateTarget(reqID, privTgt)
				return err
			}
		}

	} else {
		// Device is already mounted. Need to ensure that it is already
		// mounted to the expected private mount, with correct rw/ro perms
		mounted := false
		for _, m := range devMnts {
			if m.Path == target {
				log.Printf("mount %#v already mounted to requested target %s", m, target)
			} else if m.Path == privTgt {
				log.WithFields(f).Printf("mount Path %s Source %s Device %s Opts %v", m.Path, m.Source, m.Device, m.Opts)
				mounted = true
				rwo := multiAccessFlag
				if ro {
					rwo = "ro"
				}
				if rwo == "" || contains(m.Opts, rwo) {
					log.WithFields(f).Printf("private mount already in place")
				} else {
					log.WithFields(f).Printf("mount %#v rwo %s", m, rwo)
					return status.Error(codes.InvalidArgument,
						"Access mode conflicts with existing mounts")
				}
			} else if singleAccessMode(accMode) {
				return status.Error(codes.FailedPrecondition,
					fmt.Sprintf("Access mode conflicts with existing mounts for privTgt %s", privTgt))
			}
		}
		if !mounted {
			return status.Error(codes.Internal,
				fmt.Sprintf("Device already in use and mounted elsewhere for privTgt %s", privTgt))
		}
	}

	// Private mount in place, now bind mount to target path
	targetMnts, err := getPathMounts(target)
	if err != nil {
		return status.Errorf(codes.Internal,
			"could not reliably determine existing mount status: %s",
			err.Error())
	}

	// If mounts already existed for this device, check if mount to
	// target path was already there
	if len(targetMnts) > 0 {
		for _, m := range targetMnts {
			if m.Path == target {
				// volume already published to target
				// if mount options look good, do nothing
				rwo := multiAccessFlag
				if ro {
					rwo = "ro"
				}
				if rwo != "" && !contains(m.Opts, rwo) {
					log.WithFields(f).Printf("mount %#v rwo %s\n", m, rwo)
					return status.Error(codes.Internal,
						"volume previously published with different options")
				}
				// Existing mount satisfies request
				log.WithFields(f).Debug("volume already published to target")
				return nil
			}
		}
	}

	// Recheck that target is created. k8s has this awful habit of deleting the target if it times out the request.
	// This will narrow the window.
	err = createTarget(target, isBlock)
	if err != nil {
		// Unmount and remove the private directory for the retry so clean start next time.
		// K8S probably removed part of the path.
		cleanupPrivateTarget(reqID, privTgt)
		return status.Error(codes.FailedPrecondition, fmt.Sprintf("Could not create %s: %s", target, err.Error()))
	}

	var mntFlags []string
	mntFlags = mntVol.GetMountFlags()
	if mntVol.FsType == "xfs" {
		mntFlags = append(mntFlags, "nouuid")
	}
	if ro || accMode.GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY {
		mntFlags = append(mntFlags, "ro")
	}

	if err := gofsutil.BindMount(ctx, privTgt, target, mntFlags...); err != nil {
		// Unmount and remove the private directory for the retry so clean start next time.
		// K8S probably removed part of the path.
		cleanupPrivateTarget(reqID, privTgt)
		return status.Errorf(codes.Internal,
			"error publish volume to target path: %s",
			err.Error())
	}

	return nil
}

func handlePrivFSMount(
	ctx context.Context,
	accMode *csi.VolumeCapability_AccessMode,
	sysDevice *Device,
	mntFlags []string,
	fs, privTgt string) error {

	// Invoke the formats with a No Discard option to reduce formatting time
	formatCtx := context.WithValue(ctx, gofsutil.ContextKey(gofsutil.NoDiscard), gofsutil.NoDiscard)

	// If read-only access mode, we don't allow formatting
	if accMode.GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY || accMode.GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
		mntFlags = append(mntFlags, "ro")
		if err := gofsutil.Mount(ctx, sysDevice.FullPath, privTgt, fs, mntFlags...); err != nil {
			return status.Errorf(codes.Internal,
				"error performing private mount: %s",
				err.Error())
		}
		return nil
	} else if accMode.GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
		if err := gofsutil.FormatAndMount(formatCtx, sysDevice.FullPath, privTgt, fs, mntFlags...); err != nil {
			return status.Errorf(codes.Internal,
				"error performing private mount: %s",
				err.Error())
		}
		return nil
	}
	return status.Error(codes.Internal, "Invalid access mode")
}

func getPrivateMountPoint(privDir string, name string) string {
	return filepath.Join(privDir, name)
}

func contains(list []string, item string) bool {
	for _, x := range list {
		if x == item {
			return true
		}
	}
	return false
}

// mkfile creates a file specified by the path if needed.
// return pair is a bool flag of whether file was created, and an error
func mkfile(path string) (bool, error) {
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		/* #nosec G302 G304 */
		file, err := os.OpenFile(path, os.O_CREATE, 0755)
		if err != nil {
			log.WithField("dir", path).WithError(
				err).Error("Unable to create dir")
			return false, err
		}
		err = file.Close()
		if err != nil {
			// Log the error but keep going
			log.WithField("file", path).WithError(
				err).Error("Unable to close file")
		}
		log.WithField("path", path).Debug("created file")
		return true, nil
	}
	if st.IsDir() {
		return false, fmt.Errorf("existing path is a directory")
	}
	return false, nil
}

// mkdir creates the directory specified by path if needed.
// return pair is a bool flag of whether dir was created, and an error
func mkdir(path string) (bool, error) {
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		/* #nosec G301 */
		if err := os.Mkdir(path, 0755); err != nil {
			log.WithField("dir", path).WithError(
				err).Error("Unable to create dir")
			return false, err
		}
		log.WithField("path", path).Debug("created directory")
		return true, nil
	}
	if !st.IsDir() {
		return false, fmt.Errorf("existing path is not a directory")
	}
	return false, nil
}

// unpublishVolume removes the bind mount to the target path, and also removes
// the mount to the private mount directory if the volume is no longer in use.
// It determines this by checking to see if the volume is mounted anywhere else
// other than the private mount.
func unpublishVolume(
	req *csi.NodeUnpublishVolumeRequest,
	privDir, device string, reqID string) error {

	ctx := context.Background()
	id := req.GetVolumeId()

	target := req.GetTargetPath()
	if target == "" {
		return status.Error(codes.InvalidArgument,
			"target path required")
	}

	// make sure device is valid
	sysDevice, err := GetDevice(device)
	if err != nil {
		return status.Errorf(codes.Internal,
			"error getting block device for volume: %s, err: %s",
			id, err.Error())
	}

	// Path to mount device to
	privTgt := getPrivateMountPoint(privDir, id)

	f := log.Fields{
		"device":       sysDevice.RealDev,
		"privTgt":      privTgt,
		"CSIRequestID": reqID,
		"target":       target,
	}

	mnts, err := gofsutil.GetMounts(ctx)
	if err != nil {
		return status.Errorf(codes.Internal,
			"could not reliably determine existing mount status: %s",
			err.Error())
	}

	tgtMnt := false
	privMnt := false
	for _, m := range mnts {
		if m.Source == sysDevice.RealDev || m.Device == sysDevice.RealDev || m.Device == sysDevice.FullPath {
			if m.Path == privTgt {
				privMnt = true
			} else if m.Path == target {
				tgtMnt = true
			}
			if !privMnt {
				log.Printf("found some other device matching private mount %s , %#v do manual cleanup if needed \n", privTgt, m)
			}
		}
	}

	log.Printf("Cleanup flags tgtMnt=%t  privMnt=%t\n", tgtMnt, privMnt)

	if tgtMnt {
		log.WithFields(f).Debug(fmt.Sprintf("Unmounting %s", target))
		if err := gofsutil.Unmount(ctx, target); err != nil {
			return status.Errorf(codes.Internal,
				"Error unmounting target: %s", err.Error())
		}
		if err := removeWithRetry(target); err != nil {
			return status.Errorf(codes.Internal,
				"Error remove target folder: %s", err.Error())
		}
	}

	if privMnt {
		log.WithFields(f).Debug(fmt.Sprintf("Unmounting %s", privTgt))
		if err := unmountPrivMount(ctx, sysDevice, privTgt); err != nil {
			return status.Errorf(codes.Internal,
				"Error unmounting private mount: %s", err.Error())
		}
	}

	return nil
}

func unmountPrivMount(
	ctx context.Context,
	dev *Device,
	target string) error {

	mnts, err := getDevMounts(dev)
	if err != nil {
		return err
	}

	// remove private mount if we can
	if len(mnts) == 1 && mnts[0].Path == target {
		if err := gofsutil.Unmount(ctx, target); err != nil {
			return err
		}
		log.WithField("directory", target).Debug(
			"removing directory")
		if err := os.Remove(target); err != nil {
			log.Errorf("Unable to remove directory: %v", err)
		}
	}
	return nil
}

func getDevMounts(
	sysDevice *Device) ([]gofsutil.Info, error) {

	ctx := context.Background()
	devMnts := make([]gofsutil.Info, 0)

	mnts, err := gofsutil.GetMounts(ctx)
	if err != nil {
		return devMnts, err
	}
	for _, m := range mnts {
		if m.Device == sysDevice.RealDev || (m.Device == "devtmpfs" && m.Source == sysDevice.RealDev) {
			devMnts = append(devMnts, m)
		}
	}
	return devMnts, nil
}

// For Windows testing, replace any paths with \\ to have /
func replaceBackslashWithSlash(input string) string {
	return strings.Replace(input, "\\", "/", -1)
}

// getPathMounts finds all the mounts for a given path.
func getPathMounts(path string) ([]gofsutil.Info, error) {
	ctx := context.Background()
	devMnts := make([]gofsutil.Info, 0)

	mnts, err := gofsutil.GetMounts(ctx)
	if err != nil {
		return devMnts, err
	}
	for _, m := range mnts {
		if m.Path == path {
			devMnts = append(devMnts, m)
		}
	}
	return devMnts, nil
}

func removeWithRetry(target string) error {
	var err error
	for i := 0; i < 3; i++ {
		err = os.Remove(target)
		if err != nil && !os.IsNotExist(err) {
			log.Error("error removing private mount target: " + err.Error())
			err = os.RemoveAll(target)
			if err != nil {
				log.Errorf("Error removing directory: %v", err.Error())
			}
			time.Sleep(3 * time.Second)
		} else {
			err = nil
			break
		}
	}
	return err
}

// Evaulate symlinks to a resolution. In case of an error,
// logs the error but returns the original path.
func evalSymlinks(path string) string {
	// eval any symlinks and make sure it points to a device
	d, err := filepath.EvalSymlinks(path)
	if err != nil {
		log.Error("Could not evaluate symlinks for path: " + path)
		return path
	}
	return d
}

// Given a volume capability, validates it and returns:
// boolean isBlock -- the capability is for a block device
// csi.VolumeCapability_MountVolume - contains FsType and MountFlags
// csi.VolumeCapability_AccessMode accMode gives the access mode
// string multiAccessFlag - "rw" or "ro" or "" as appropriate
// error
func validateVolumeCapability(volCap *csi.VolumeCapability, readOnly bool) (bool, *csi.VolumeCapability_MountVolume, *csi.VolumeCapability_AccessMode, string, error) {
	var mntVol *csi.VolumeCapability_MountVolume
	isBlock := false
	isMount := false
	multiAccessFlag := ""
	accMode := volCap.GetAccessMode()
	if accMode == nil {
		return false, mntVol, nil, "", status.Error(codes.InvalidArgument, "Volume Access Mode is required")
	}
	if blockVol := volCap.GetBlock(); blockVol != nil {
		isBlock = true
		switch accMode.GetMode() {
		case csi.VolumeCapability_AccessMode_UNKNOWN:
			return true, mntVol, accMode, "", status.Error(codes.InvalidArgument, "Unknown Access Mode")
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
		case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
			multiAccessFlag = "ro"
		case csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:
		case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			multiAccessFlag = "rw"
		}
		if readOnly {
			return true, mntVol, accMode, "", status.Error(codes.InvalidArgument, "read only not supported for Block Volume")
		}
	}
	mntVol = volCap.GetMount()
	if mntVol != nil {
		isMount = true
		switch accMode.GetMode() {
		case csi.VolumeCapability_AccessMode_UNKNOWN:
			return false, mntVol, accMode, "", status.Error(codes.InvalidArgument, "Unknown Access Mode")
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
			if mountAllowRWOMultiPodAccess {
				multiAccessFlag = "rw"
			}
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
			if mountAllowRWOMultiPodAccess {
				multiAccessFlag = "ro"
			}
		case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
			multiAccessFlag = "ro"
		case csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:
		case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			return false, mntVol, accMode, "", status.Error(codes.AlreadyExists, "Mount volumes do not support AccessMode MULTI_NODE_MULTI_WRITER")
		}
	}

	if !isBlock && !isMount {
		return false, mntVol, accMode, "", status.Error(codes.InvalidArgument, "Volume Access Type is required")
	}
	return isBlock, mntVol, accMode, multiAccessFlag, nil
}

// singleAccessMode returns true if only a single access is allowed SINGLE_NODE_WRITER or SINGLE_NODE_READER_ONLY
func singleAccessMode(accMode *csi.VolumeCapability_AccessMode) bool {
	if mountAllowRWOMultiPodAccess {
		// User specifically asks for multi-pod access on same nodes
		return false
	}
	switch accMode.GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
		return true
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:
		return true
	}
	return false
}

func createTarget(target string, isBlock bool) error {
	var err error
	// Make sure target is created. The spec says the driver is responsible
	// for creating the target, but Kubernetes generallly creates the target.
	if isBlock {
		_, err = mkfile(target)
		if err != nil {
			return status.Error(codes.FailedPrecondition, fmt.Sprintf("Could not create %s: %s", target, err.Error()))
		}
	} else {
		_, err = mkdir(target)
		if err != nil {
			return status.Error(codes.FailedPrecondition, fmt.Sprintf("Could not create %s: %s", target, err.Error()))
		}
	}
	return nil
}

// cleanupPrivateTarget unmounts and removes the private directory for the retry so clean start next time.
func cleanupPrivateTarget(reqID, privTgt string) {
	log.WithField("CSIRequestID", reqID).WithField("privTgt", privTgt).Info("Cleaning up private target")
	if privErr := gofsutil.Unmount(context.Background(), privTgt); privErr != nil {
		log.WithField("CSIRequestID", reqID).Printf("Error unmounting privTgt %s: %s", privTgt, privErr)
	}
	if privErr := removeWithRetry(privTgt); privErr != nil {
		log.WithField("CSIRequestID", reqID).Printf("Error removing privTgt %s: %s", privTgt, privErr)
	}
}

// mountBlock bind mounts the device to the required target
func mountBlock(device *Device, target string, mntFlags []string, singleAccess bool) error {
	log.Printf("mountBlock called device %#v target %s mntFlags %#v", device, target, mntFlags)
	// Check to see if already mounted
	mnts, err := getDevMounts(device)
	if err != nil {
		return status.Errorf(codes.Internal, "Could not getDevMounts for: %s", device.RealDev)
	}
	for _, mnt := range mnts {
		if mnt.Path == target {
			log.Info("Block volume target is already mounted")
			return nil
		} else if singleAccess {
			return status.Error(codes.InvalidArgument, "Access mode conflicts with existing mounts")
		}
	}
	err = createTarget(target, true)
	if err != nil {
		return status.Error(codes.FailedPrecondition, fmt.Sprintf("Could not create %s: %s", target, err.Error()))
	}
	err = gofsutil.BindMount(context.Background(), device.RealDev, target, mntFlags...)
	if err != nil {
		return status.Errorf(codes.Internal, "error bind mounting to target path: %s", target)
	}
	return nil
}
