package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

var ErrorAttachDefNotOvnManaged = errors.New("net-attach-def not managed by OVN")
var ErrorChainingNotSupported = errors.New("CNI plugin chaining is not supported")

// WriteCNIConfig writes a CNI JSON config file to directory given by global config
// if the file doesn't already exist, or is different than the content that would
// be written.
func WriteCNIConfig() error {
	netConf := &ovncnitypes.NetConf{
		NetConf: types.NetConf{
			CNIVersion: "0.4.0",
			Name:       "ovn-kubernetes",
			Type:       CNI.Plugin,
		},
		LogFile:           Logging.CNIFile,
		LogLevel:          fmt.Sprintf("%d", Logging.Level),
		LogFileMaxSize:    Logging.LogFileMaxSize,
		LogFileMaxBackups: Logging.LogFileMaxBackups,
		LogFileMaxAge:     Logging.LogFileMaxAge,
	}

	newBytes, err := json.Marshal(netConf)
	if err != nil {
		return fmt.Errorf("failed to marshal CNI config JSON: %v", err)
	}

	confFile := filepath.Join(CNI.ConfDir, CNIConfFileName)
	if existingBytes, err := os.ReadFile(confFile); err == nil {
		if bytes.Equal(newBytes, existingBytes) {
			// No changes; do nothing
			return nil
		}
	}

	// Install the CNI config file after all initialization is done
	// MkdirAll() returns no error if the path already exists
	if err := os.MkdirAll(CNI.ConfDir, os.ModeDir); err != nil {
		return err
	}

	var f *os.File
	f, err = os.CreateTemp(CNI.ConfDir, "ovnkube-")
	if err != nil {
		return err
	}

	if _, err := f.Write(newBytes); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	return os.Rename(f.Name(), confFile)
}

// ParseNetConf parses config in NAD spec
func ParseNetConf(bytes []byte) (*ovncnitypes.NetConf, error) {
	var netconf *ovncnitypes.NetConf
	confList, err := libcni.ConfListFromBytes(bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to get netconf list from NAD spec: %w", err)
	}
	netconf, err = parseNetConfList(confList)
	if err != nil {
		return nil, fmt.Errorf("failed to get netconf from netconf list %q: %w", confList.Name, err)
	}

	if netconf.Topology == "" {
		// NAD of default network
		netconf.Name = ovntypes.DefaultNetworkName
	}

	return netconf, nil
}

// parseNetConfList accepts a NetworkConfigList with at most one plugin specified.
// If a single plugin exists, this NetConf is returned.
// If it doesn't exist, return the NetConfig from the NetworkConfigList
// FIXME: we dont respect LoadOnlyInlinedPlugins field and instead if a single plugin is specified, we use that.
func parseNetConfList(confList *libcni.NetworkConfigList) (*ovncnitypes.NetConf, error) {
	var bytes []byte
	if len(confList.Plugins) > 0 {
		bytes = confList.Plugins[0].Bytes
	} else {
		bytes = confList.Bytes
	}
	netconf := &ovncnitypes.NetConf{MTU: Default.MTU}
	if err := json.Unmarshal(bytes, netconf); err != nil {
		return nil, err
	}
	// skip non-OVN NAD
	if netconf.Type != "ovn-k8s-cni-overlay" {
		return nil, ErrorAttachDefNotOvnManaged
	}

	if len(confList.Plugins) > 1 {
		return nil, ErrorChainingNotSupported
	}

	netconf.Name = confList.Name
	netconf.CNIVersion = confList.CNIVersion

	if err := ValidateNetConfNameFields(netconf); err != nil {
		return nil, err
	}

	return netconf, nil
}

func ValidateNetConfNameFields(netconf *ovncnitypes.NetConf) error {
	if netconf.Topology != "" {
		if netconf.NADName == "" {
			return fmt.Errorf("missing NADName in secondary network netconf %s", netconf.Name)
		}
		// "ovn-kubernetes" network name is reserved for later
		if netconf.Name == "" || netconf.Name == ovntypes.DefaultNetworkName || netconf.Name == "ovn-kubernetes" {
			return fmt.Errorf("invalid name in in secondary network netconf (%s)", netconf.Name)
		}
	}

	return nil
}

// ReadCNIConfig unmarshals a CNI JSON config into an NetConf structure
func ReadCNIConfig(bytes []byte) (*ovncnitypes.NetConf, error) {
	conf, err := ParseNetConf(bytes)
	if err != nil {
		return nil, err
	}
	if conf.RawPrevResult != nil {
		if err := version.ParsePrevResult(&conf.NetConf); err != nil {
			return nil, err
		}
	}
	return conf, nil
}
