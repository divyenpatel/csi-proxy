package volume

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

// API exposes the internal volume operations available in the server
type API interface {
	// ListVolumesOnDisk lists volumes on a disk identified by a `diskNumber` and optionally a partition identified by `partitionNumber`.
	ListVolumesOnDisk(diskNumber uint32, partitionNumber uint32) (volumeIDs []string, err error)
	// MountVolume mounts the volume at the requested global staging target path.
	MountVolume(volumeID, targetPath string) error
	// UnmountVolume gracefully dismounts a volume.
	UnmountVolume(volumeID, targetPath string) error
	// IsVolumeFormatted checks if a volume is formatted with NTFS.
	IsVolumeFormatted(volumeID string) (bool, error)
	// FormatVolume formats a volume with the NTFS format.
	FormatVolume(volumeID string) error
	// ResizeVolume performs resizing of the partition and file system for a block based volume.
	ResizeVolume(volumeID string, sizeBytes int64) error
	// GetVolumeStats gets the volume information.
	GetVolumeStats(volumeID string) (int64, int64, error)
	// GetDiskNumberFromVolumeID returns the disk number for a given volumeID.
	GetDiskNumberFromVolumeID(volumeID string) (uint32, error)
	// GetVolumeIDFromTargetPath returns the volume id of a given target path.
	GetVolumeIDFromTargetPath(targetPath string) (string, error)
	// WriteVolumeCache writes the volume `volumeID`'s cache to disk.
	WriteVolumeCache(volumeID string) error
	// GetVolumeIDFromTargetPath returns the volume id of a given target path.
	GetClosestVolumeIDFromTargetPath(targetPath string) (string, error)
}

// VolumeAPI implements the internal Volume APIs
type VolumeAPI struct{}

// verifies that the API is implemented
var _ API = &VolumeAPI{}

var (
	// VolumeRegexp matches a Windows Volume
	// example: Volume{452e318a-5cde-421e-9831-b9853c521012}
	//
	// The field UniqueId has an additional prefix which is NOT included in the regex
	// however the regex can match UniqueId too
	// PS C:\disks> (Get-Disk -Number 1 | Get-Partition | Get-Volume).UniqueId
	// \\?\Volume{452e318a-5cde-421e-9831-b9853c521012}\
	VolumeRegexp = regexp.MustCompile(`Volume\{[\w-]*\}`)
)

// New - Construct a new Volume API Implementation.
func New() VolumeAPI {
	return VolumeAPI{}
}

func runExec(command string) ([]byte, error) {
	cmd := exec.Command("powershell", "/c", command)
	klog.V(4).Infof("Executing command: %q", cmd.String())
	out, err := cmd.CombinedOutput()
	return out, err
}

func getVolumeSize(volumeID string) (int64, error) {
	cmd := fmt.Sprintf("(Get-Volume -UniqueId \"%s\" | Get-partition).Size", volumeID)
	out, err := runExec(cmd)

	if err != nil || len(out) == 0 {
		return -1, fmt.Errorf("error getting size of the partition from mount. cmd %s, output: %s, error: %v", cmd, string(out), err)
	}

	outString := strings.TrimSpace(string(out))
	volumeSize, err := strconv.ParseInt(outString, 10, 64)
	if err != nil {
		return -1, fmt.Errorf("error parsing size of volume %s received %v trimmed to %v err %v", volumeID, out, outString, err)
	}

	return volumeSize, nil
}

// ListVolumesOnDisk - returns back list of volumes(volumeIDs) in a disk and a partition.
func (VolumeAPI) ListVolumesOnDisk(diskNumber uint32, partitionNumber uint32) (volumeIDs []string, err error) {
	var cmd string
	if partitionNumber == 0 {
		// 0 means that the partitionNumber wasn't set so we list all the partitions
		cmd = fmt.Sprintf("(Get-Disk -Number %d | Get-Partition | Get-Volume).UniqueId", diskNumber)
	} else {
		cmd = fmt.Sprintf("(Get-Disk -Number %d | Get-Partition -PartitionNumber %d | Get-Volume).UniqueId", diskNumber, partitionNumber)
	}
	out, err := runExec(cmd)
	if err != nil {
		return []string{}, fmt.Errorf("error list volumes on disk. cmd: %s, output: %s, error: %v", cmd, string(out), err)
	}

	volumeIds := strings.Split(strings.TrimSpace(string(out)), "\r\n")
	return volumeIds, nil
}

// FormatVolume - Formats a volume with the NTFS format.
func (VolumeAPI) FormatVolume(volumeID string) (err error) {
	cmd := fmt.Sprintf("Get-Volume -UniqueId \"%s\" | Format-Volume -FileSystem ntfs -Confirm:$false", volumeID)
	out, err := runExec(cmd)
	if err != nil {
		return fmt.Errorf("error formatting volume. cmd: %s, output: %s, error: %v", cmd, string(out), err)
	}
	// TODO: Do we need to handle anything for len(out) == 0
	return nil
}

// WriteVolumeCache - Writes the file system cache to disk with the given volume id
func (VolumeAPI) WriteVolumeCache(volumeID string) (err error) {
	return writeCache(volumeID)
}

// IsVolumeFormatted - Check if the volume is formatted with the pre specified filesystem(typically ntfs).
func (VolumeAPI) IsVolumeFormatted(volumeID string) (bool, error) {
	cmd := fmt.Sprintf("(Get-Volume -UniqueId \"%s\" -ErrorAction Stop).FileSystemType", volumeID)
	out, err := runExec(cmd)
	if err != nil {
		return false, fmt.Errorf("error checking if volume is formatted. cmd: %s, output: %s, error: %v", cmd, string(out), err)
	}
	stringOut := strings.TrimSpace(string(out))
	if len(stringOut) == 0 || strings.EqualFold(stringOut, "Unknown") {
		return false, nil
	}
	return true, nil
}

// MountVolume - mounts a volume to a path. This is done using the Add-PartitionAccessPath for presenting the volume via a path.
func (VolumeAPI) MountVolume(volumeID, path string) error {
	cmd := fmt.Sprintf("Get-Volume -UniqueId \"%s\" | Get-Partition | Add-PartitionAccessPath -AccessPath %s", volumeID, path)
	out, err := runExec(cmd)
	if err != nil {
		return fmt.Errorf("error mount volume to path. cmd: %s, output: %s, error: %v", cmd, string(out), err)
	}
	return nil
}

// UnmountVolume - unmounts the volume path by removing the partition access path
func (VolumeAPI) UnmountVolume(volumeID, path string) error {
	if err := writeCache(volumeID); err != nil {
		return err
	}
	cmd := fmt.Sprintf("Get-Volume -UniqueId \"%s\" | Get-Partition | Remove-PartitionAccessPath -AccessPath %s", volumeID, path)
	out, err := runExec(cmd)
	if err != nil {
		return fmt.Errorf("error getting driver letter to mount volume. cmd: %s, output: %s,error: %v", cmd, string(out), err)
	}
	return nil
}

// ResizeVolume - resizes a volume with the given size, if size == 0 then max supported size is used
func (VolumeAPI) ResizeVolume(volumeID string, size int64) error {
	// If size is 0 then we will resize to the maximum size possible, otherwise just resize to size
	var cmd string
	var out []byte
	var err error
	var finalSize int64
	var outString string
	if size == 0 {
		cmd = fmt.Sprintf("Get-Volume -UniqueId \"%s\" | Get-partition | Get-PartitionSupportedSize | Select SizeMax | ConvertTo-Json", volumeID)
		out, err = runExec(cmd)

		if err != nil || len(out) == 0 {
			return fmt.Errorf("error getting sizemin,sizemax from mount. cmd: %s, output: %s, error: %v", cmd, string(out), err)
		}

		var getVolumeSizing map[string]int64
		outString = string(out)
		err = json.Unmarshal([]byte(outString), &getVolumeSizing)
		if err != nil {
			return fmt.Errorf("out %v outstring %v err %v", out, outString, err)
		}

		sizeMax := getVolumeSizing["SizeMax"]

		finalSize = sizeMax
	} else {
		finalSize = size
	}

	currentSize, err := getVolumeSize(volumeID)
	if err != nil {
		return fmt.Errorf("error getting the current size of volume (%s) with error (%v)", volumeID, err)
	}

	//if the partition's size is already the size we want this is a noop, just return
	if currentSize >= finalSize {
		klog.V(2).Infof("Attempted to resize volume %s to a lower size, from currentBytes=%d wantedBytes=%d", volumeID, currentSize, finalSize)
		return nil
	}

	cmd = fmt.Sprintf("Get-Volume -UniqueId \"%s\" | Get-Partition | Resize-Partition -Size %d", volumeID, finalSize)
	out, err = runExec(cmd)
	if err != nil {
		return fmt.Errorf("error resizing volume. cmd: %s, output: %s size:%v, finalSize %v, error: %v", cmd, string(out), size, finalSize, err)
	}
	return nil
}

// GetVolumeStats - retrieves the volume stats for a given volume
func (VolumeAPI) GetVolumeStats(volumeID string) (int64, int64, error) {
	// get the size and sizeRemaining for the volume
	cmd := fmt.Sprintf("(Get-Volume -UniqueId \"%s\" | Select SizeRemaining,Size) | ConvertTo-Json", volumeID)
	out, err := runExec(cmd)

	if err != nil {
		return -1, -1, fmt.Errorf("error getting capacity and used size of volume. cmd: %s, output: %s, error: %v", cmd, string(out), err)
	}

	var getVolume map[string]int64
	outString := string(out)
	err = json.Unmarshal([]byte(outString), &getVolume)
	if err != nil {
		return -1, -1, fmt.Errorf("out %v outstring %v err %v", out, outString, err)
	}
	var volumeSizeRemaining int64
	var volumeSize int64

	volumeSize = getVolume["Size"]
	volumeSizeRemaining = getVolume["SizeRemaining"]

	volumeUsedSize := volumeSize - volumeSizeRemaining
	return volumeSize, volumeUsedSize, nil
}

// GetDiskNumberFromVolumeID - gets the disk number where the volume is.
func (VolumeAPI) GetDiskNumberFromVolumeID(volumeID string) (uint32, error) {
	// get the size and sizeRemaining for the volume
	cmd := fmt.Sprintf("(Get-Volume -UniqueId \"%s\" | Get-Partition).DiskNumber", volumeID)
	out, err := runExec(cmd)

	if err != nil || len(out) == 0 {
		return 0, fmt.Errorf("error getting disk number. cmd: %s, output: %s, error: %v", cmd, string(out), err)
	}

	reg, err := regexp.Compile("[^0-9]+")
	if err != nil {
		return 0, fmt.Errorf("error compiling regex. err: %v", err)
	}
	diskNumberOutput := reg.ReplaceAllString(string(out), "")

	diskNumber, err := strconv.ParseUint(diskNumberOutput, 10, 32)

	if err != nil {
		return 0, fmt.Errorf("error parsing disk number. cmd: %s, output: %s, error: %v", cmd, diskNumberOutput, err)
	}

	return uint32(diskNumber), nil
}

// GetVolumeIDFromTargetPath - gets the volume ID given a mount point, the function is recursive until it find a volume or errors out
func (VolumeAPI) GetVolumeIDFromTargetPath(mount string) (string, error) {
	volumeString, err := getTarget(mount)

	if err != nil {
		return "", fmt.Errorf("error getting the volume for the mount %s, internal error %v", mount, err)
	}

	return volumeString, nil
}

func getTarget(mount string) (string, error) {
	cmd := fmt.Sprintf("(Get-Item -Path %s).Target", mount)
	out, err := runExec(cmd)
	if err != nil || len(out) == 0 {
		return "", fmt.Errorf("error getting volume from mount. cmd: %s, output: %s, error: %v", cmd, string(out), err)
	}
	volumeString := strings.TrimSpace(string(out))
	if !strings.HasPrefix(volumeString, "Volume") {
		return getTarget(volumeString)
	}

	return ensureVolumePrefix(volumeString), nil
}

// GetVolumeIDFromTargetPath returns the volume id of a given target path.
func (VolumeAPI) GetClosestVolumeIDFromTargetPath(targetPath string) (string, error) {
	volumeString, err := findClosestVolume(targetPath)

	if err != nil {
		return "", fmt.Errorf("error getting the closest volume for the path=%s, err=%v", targetPath, err)
	}

	return volumeString, nil
}

// findClosestVolume finds the closest volume id for a given target path
// by following symlinks and moving up in the filesystem, if after moving up in the filesystem
// we get to a DriveLetter then the volume corresponding to this drive letter is returned instead.
func findClosestVolume(path string) (string, error) {
	candidatePath := path

	// Run in a bounded loop to avoid doing an infinite loop
	// while trying to follow symlinks
	//
	// The maximum path length in Windows is 260, it could be possible to end
	// up in a sceneario where we do more than 256 iterations (e.g. by following symlinks from
	// a place high in the hierarchy to a nested sibling location many times)
	// https://docs.microsoft.com/en-us/windows/win32/fileio/naming-a-file#:~:text=In%20editions%20of%20Windows%20before,required%20to%20remove%20the%20limit.
	//
	// The number of iterations is 256, which is similar to the number of iterations in filepath-securejoin
	// https://github.com/cyphar/filepath-securejoin/blob/64536a8a66ae59588c981e2199f1dcf410508e07/join.go#L51
	for i := 0; i < 256; i += 1 {
		fi, err := os.Lstat(candidatePath)
		if err != nil {
			return "", err
		}
		isSymlink := fi.Mode()&os.ModeSymlink != 0

		if isSymlink {
			target, err := dereferenceSymlink(candidatePath)
			if err != nil {
				return "", err
			}
			// if it has the form Volume{volumeid} then it's a volume
			if VolumeRegexp.Match([]byte(target)) {
				// symlinks that are pointing to Volumes don't have this prefix
				return ensureVolumePrefix(target), nil
			}
			// otherwise follow the symlink
			candidatePath = target
		} else {
			// if it's not a symlink move one level up
			previousPath := candidatePath
			candidatePath = filepath.Dir(candidatePath)

			// if the new path is the same as the previous path then we reached the root path
			if previousPath == candidatePath {
				// find the volume for the root path (assuming that it's a DriveLetter)
				target, err := getVolumeForDriveLetter(candidatePath[0:1])
				if err != nil {
					return "", err
				}
				return target, nil
			}
		}

	}

	return "", fmt.Errorf("Failed to find the closest volume for path=%s", path)
}

// ensureVolumePrefix makes sure that the volume has the Volume prefix
func ensureVolumePrefix(volume string) string {
	prefix := "\\\\?\\"
	if !strings.HasPrefix(volume, prefix) {
		volume = prefix + volume
	}
	return volume
}

// dereferenceSymlink dereferences the symlink `path` and returns the stdout.
func dereferenceSymlink(path string) (string, error) {
	cmd := exec.Command("powershell", "/c", fmt.Sprintf(`(Get-Item -Path %s).Target`, path))
	klog.V(8).Infof("About to execute: %q", cmd.String())
	var outbuf, errbuf bytes.Buffer
	cmd.Stderr = &errbuf
	cmd.Stdout = &outbuf
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if len(errbuf.String()) != 0 {
		return "", fmt.Errorf("Unexpected stderr output in command=%v stdeerr=%v", cmd.String(), errbuf.String())
	}
	output := strings.TrimSpace(outbuf.String())
	klog.V(8).Infof("Stdout: %s", output)
	return output, nil
}

// getVolumeForDriveLetter gets a volume from a drive letter (e.g. C:/).
func getVolumeForDriveLetter(path string) (string, error) {
	if len(path) != 1 {
		return "", fmt.Errorf("The path=%s is not a valid DriverLetter", path)
	}

	cmd := exec.Command("powershell", "/c", fmt.Sprintf(`(Get-Partition -DriveLetter %s | Get-Volume).UniqueId`, path))
	klog.V(8).Infof("About to execute: %q", cmd.String())
	targetb, err := cmd.Output()
	if err != nil {
		return "", err
	}
	output := strings.TrimSpace(string(targetb))
	klog.V(8).Infof("Stdout: %s", output)
	return output, nil
}

func writeCache(volumeID string) error {
	cmd := fmt.Sprintf("Get-Volume -UniqueId \"%s\" | Write-Volumecache", volumeID)
	out, err := runExec(cmd)
	if err != nil {
		return fmt.Errorf("error writing volume cache. cmd: %s, output: %s, error: %v", cmd, string(out), err)
	}
	return nil
}
