/*
Copyright 2023 NVIDIA CORPORATION & AFFILIATES

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

package sriovnet

import (
	"net"
	"path/filepath"
	"strconv"
)

const (
	ibSriovCfgDir               = "sriov"
	ibSriovNodeFile             = "node"
	ibSriovPortFile             = "port"
	ibSriovPortAdminFile        = "policy"
	ibSriovPortAdminStateFollow = "Follow"
)

func ibGetPortAdminState(pfNetdevName string, vfIndex int) (string, error) {
	path := filepath.Join(
		NetSysDir, pfNetdevName, pcidevPrefix, ibSriovCfgDir, strconv.Itoa(vfIndex), ibSriovPortAdminFile)
	adminStateFile := fileObject{
		Path: path,
	}

	state, err := adminStateFile.Read()
	if err != nil {
		return "", err
	}
	return state, nil
}

func ibSetPortAdminState(pfNetdevName string, vfIndex int, newState string) error {
	path := filepath.Join(
		NetSysDir, pfNetdevName, pcidevPrefix, ibSriovCfgDir, strconv.Itoa(vfIndex), ibSriovPortAdminFile)
	adminStateFile := fileObject{
		Path: path,
	}

	return adminStateFile.Write(newState)
}

func ibSetNodeGUID(pfNetdevName string, vfIndex int, guid net.HardwareAddr) error {
	path := filepath.Join(NetSysDir, pfNetdevName, pcidevPrefix, ibSriovCfgDir, strconv.Itoa(vfIndex), ibSriovNodeFile)
	nodeGUIDFile := fileObject{
		Path: path,
	}
	kernelGUIDFormat := guid.String()
	return nodeGUIDFile.Write(kernelGUIDFormat)
}

func ibSetPortGUID(pfNetdevName string, vfIndex int, guid net.HardwareAddr) error {
	path := filepath.Join(NetSysDir, pfNetdevName, pcidevPrefix, ibSriovCfgDir, strconv.Itoa(vfIndex), ibSriovPortFile)
	portGUIDFile := fileObject{
		Path: path,
	}
	kernelGUIDFormat := guid.String()
	return portGUIDFile.Write(kernelGUIDFormat)
}
