/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metadatas

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"k8s.io/utils/exec"

	"github.com/huaweicloud/huaweicloud-csi-driver/pkg/utils/mounts"
)

const (
	// metadataURLTemplate allows building an OpenStack Metadata service URL.
	// It's a hardcoded IPv4 link-local address as documented in "User Documentation section "Metadata service
	defaultMetadataVersion = "latest"
	metadataURLTemplate    = "http://169.254.169.254/openstack/%s/meta_data.json"

	// MetadataID is used as an identifier on the metadata search order configuration.
	MetadataID = "metadataService"

	// Config drive is defined as an iso9660 or vfat (deprecated) drive with the "config-2" label.
	configDriveLabel        = "config-2"
	configDrivePathTemplate = "openstack/%s/meta_data.json"

	// ConfigDriveID is used as an identifier on the metadata search order configuration.
	ConfigDriveID = "configDrive"
)

// ErrBadMetadata is used to indicate a problem parsing data from metadata server
var ErrBadMetadata = errors.New("invalid OpenStack metadata, got empty uuid")

// MetadataService instance of IMetadata
var MetadataService IMetadata

// Metadata is fixed for the current host, so cache the value process-wide
var metadataCache *Metadata

// MyDuration is the encoding.TextUnmarshaler interface for time.Duration
type MyDuration struct {
	time.Duration
}

// UnmarshalText is used to convert from text to Duration
func (d *MyDuration) UnmarshalText(text []byte) error {
	res, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = res
	return nil
}

// MetadataOpts is used for configuring how to talk to metadata service or config drive
type MetadataOpts struct {
	SearchOrder    string     `gcfg:"search-order"`
	RequestTimeout MyDuration `gcfg:"request-timeout"`
}

// DeviceMetadata is a single/simplified data structure for all kinds of device metadata types.
type DeviceMetadata struct {
	Type    string `json:"type"`
	Bus     string `json:"bus,omitempty"`
	Serial  string `json:"serial,omitempty"`
	Address string `json:"address,omitempty"`
}

// Metadata has the information fetched from OpenStack metadata service or
// config drives. Assumes the "latest" meta_data.json format.
type Metadata struct {
	UUID             string           `json:"uuid"`
	Name             string           `json:"name"`
	AvailabilityZone string           `json:"availability_zone"`
	Devices          []DeviceMetadata `json:"devices,omitempty"`
}

type metadataService struct {
	searchOrder string
}

// IMetadata implements GetInstanceID & GetAvailabilityZone
type IMetadata interface {
	GetInstanceID() (string, error)
	GetAvailabilityZone() (string, error)
}

// GetMetadataProvider retrieves instance of IMetadata
func GetMetadataProvider(order string) IMetadata {

	if MetadataService == nil {
		MetadataService = &metadataService{searchOrder: order}
	}
	return MetadataService
}

// Set sets the value of metadata cache
func Set(value *Metadata) {
	metadataCache = value
}

// Clear clears the metadata cache
func Clear() {
	metadataCache = nil
}

// parseMetadata reads JSON from OpenStack metadata server and parses
// instance ID out of it.
func parseMetadata(r io.Reader) (*Metadata, error) {
	var metadata Metadata
	json := json.NewDecoder(r)
	if err := json.Decode(&metadata); err != nil {
		return nil, err
	}

	if metadata.UUID == "" {
		return nil, ErrBadMetadata
	}

	return &metadata, nil
}

func getMetadataURL(metadataVersion string) string {
	return fmt.Sprintf(metadataURLTemplate, metadataVersion)
}

func getConfigDrivePath(metadataVersion string) string {
	return fmt.Sprintf(configDrivePathTemplate, metadataVersion)
}

func getFromConfigDrive(metadataVersion string) (*Metadata, error) {
	// Try to read instance UUID from config drive.
	dev := "/dev/disk/by-label/" + configDriveLabel
	if _, err := os.Stat(dev); os.IsNotExist(err) {
		out, err := exec.New().Command(
			"blkid", "-l",
			"-t", "LABEL="+configDriveLabel,
			"-o", "device",
		).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("unable to run blkid: %v", err)
		}
		dev = strings.TrimSpace(string(out))
	}

	mntDir, err := ioutil.TempDir("", "configdrive")
	if err != nil {
		return nil, err
	}
	defer os.Remove(mntDir)

	klog.Infof("Attempting to mount configdrive %s on %s", dev, mntDir)

	mounter := mounts.GetMountProvider().Mounter()
	err = mounter.Mount(dev, mntDir, "iso9660", []string{"ro"})
	if err != nil {
		err = mounter.Mount(dev, mntDir, "vfat", []string{"ro"})
	}
	if err != nil {
		return nil, fmt.Errorf("error mounting configdrive %s: %v", dev, err)
	}
	defer mounter.Unmount(mntDir) //nolint:errcheck

	klog.Infof("Configdrive mounted on %s", mntDir)

	configDrivePath := getConfigDrivePath(metadataVersion)
	f, err := os.Open(
		filepath.Join(mntDir, configDrivePath))
	if err != nil {
		return nil, fmt.Errorf("error reading %s on config drive: %v", configDrivePath, err)
	}
	defer f.Close()

	return parseMetadata(f)
}

func getFromMetadataService(metadataVersion string) (*Metadata, error) {
	// Try to get JSON from metadata server.
	metadataURL := getMetadataURL(metadataVersion)
	klog.Infof("Attempting to fetch metadata from %s", metadataURL)
	resp, err := http.Get(metadataURL)
	if err != nil {
		return nil, fmt.Errorf("error fetching %s: %v", metadataURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("unexpected status code when reading metadata from %s: %s", metadataURL, resp.Status)
		return nil, err
	}

	return parseMetadata(resp.Body)
}

func GetDevicePath(volumeID string) string {
	// Nova Hyper-V hosts cannot override disk SCSI IDs. In order to locate
	// volumes, we're querying the metadata service. Note that the Hyper-V
	// driver will include device metadata for untagged volumes as well.
	//
	// We're avoiding using cached metadata (or the configdrive),
	// relying on the metadata service.
	instanceMetadata, err := getFromMetadataService(defaultMetadataVersion)

	if err != nil {
		klog.Errorf("Could not retrieve instance metadata. Error: %v", err)
		return ""
	}

	for _, device := range instanceMetadata.Devices {
		if device.Type == "disk" && device.Serial == volumeID {
			klog.Infof("Found disk metadata for volumeID %q. Bus: %q, Address: %q",
				volumeID, device.Bus, device.Address)

			diskPattern := fmt.Sprintf("/dev/disk/by-path/*-%s-%s", device.Bus, device.Address)
			diskPaths, err := filepath.Glob(diskPattern)
			if err != nil {
				klog.Errorf("could not retrieve disk path for volumeID: %q. Error filepath.Glob(%q): %v",
					volumeID, diskPattern, err)
				return ""
			}

			if len(diskPaths) == 1 {
				return diskPaths[0]
			}
			klog.Infof("expecting to find one disk path for volumeID %q, found %d: %v",
				volumeID, len(diskPaths), diskPaths)
		}
	}

	klog.Errorf("Could not retrieve device metadata for volumeID: %q", volumeID)
	return ""
}

// Get retrieves metadata from either config drive or metadata service.
// Search order depends on the order set in config file.
func Get(order string) (*Metadata, error) {
	if metadataCache == nil {
		var md *Metadata
		var err error

		elements := strings.Split(order, ",")
		for _, id := range elements {
			id = strings.TrimSpace(id)
			switch id {
			case ConfigDriveID:
				md, err = getFromConfigDrive(defaultMetadataVersion)
			case MetadataID:
				md, err = getFromMetadataService(defaultMetadataVersion)
			default:
				err = fmt.Errorf("%s is not a valid metadata search order option. "+
					"Supported options are %s and %s", id, ConfigDriveID, MetadataID)
			}

			if err == nil {
				break
			}
		}

		if err != nil {
			return nil, err
		}
		metadataCache = md
	}
	return metadataCache, nil
}

// GetInstanceID return instance ID of the node
func (m *metadataService) GetInstanceID() (string, error) {
	md, err := Get(m.searchOrder)
	if err != nil {
		return "", err
	}
	return md.UUID, nil
}

// GetAvailabilityZone returns AZ of the node
func (m *metadataService) GetAvailabilityZone() (string, error) {
	md, err := Get(m.searchOrder)
	if err != nil {
		return "", err
	}
	return md.AvailabilityZone, nil
}

func CheckMetadataSearchOrder(order string) error {
	if order == "" {
		return errors.New("invalid value in section [Metadata] with key `search-order`. Value cannot be empty")
	}

	elements := strings.Split(order, ",")
	if len(elements) > 2 {
		return errors.New("invalid value in section [Metadata] with key `search-order`. " +
			"Value cannot contain more than 2 elements")
	}

	for _, id := range elements {
		id = strings.TrimSpace(id)
		switch id {
		case ConfigDriveID:
		case MetadataID:
		default:
			return fmt.Errorf("invalid element %q found in section [Metadata] with key `search-order`."+
				"Supported elements include %q and %q", id, ConfigDriveID, MetadataID)
		}
	}

	return nil
}
