/*
Copyright 2016 The Rook Authors. All rights reserved.

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
package osd

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	testceph "github.com/rook/rook/pkg/cephmgr/client/test"
	"github.com/rook/rook/pkg/cephmgr/mon"
	"github.com/rook/rook/pkg/cephmgr/osd/partition"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/clusterd/inventory"
	"github.com/rook/rook/pkg/util"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/rook/rook/pkg/util/proc"
	"github.com/stretchr/testify/assert"
)

func TestOSDAgentWithDevices(t *testing.T) {
	// set up a temporary config directory that will be cleaned up after test
	configDir, err := ioutil.TempDir("", "TestOSDAgentWithDevices")
	if err != nil {
		t.Fatalf("failed to create temp config dir: %+v", err)
	}
	defer os.RemoveAll(configDir)

	clusterName := "mycluster"
	nodeID := "abc"
	etcdClient, agent, _ := createTestAgent(t, nodeID, "sdx,sdy", configDir)

	startCount := 0
	executor := &exectest.MockExecutor{}
	executor.MockStartExecuteCommand = func(name string, command string, args ...string) (*exec.Cmd, error) {
		logger.Infof("START %d for %s. %s %+v", startCount, name, command, args)
		cmd := &exec.Cmd{Args: append([]string{command}, args...)}

		switch {
		case startCount < 2:
			assert.Equal(t, "--type=osd", args[1])
		default:
			assert.Fail(t, fmt.Sprintf("unexpected case %d", startCount))
		}
		startCount++
		return cmd, nil
	}

	execCount := 0
	executor.MockExecuteCommand = func(name string, command string, args ...string) error {
		logger.Infof("RUN %d for %s. %s %+v", execCount, name, command, args)
		parts := strings.Split(name, " ")
		nameSuffix := parts[0]
		if len(parts) > 1 {
			nameSuffix = parts[1]
		}
		switch {
		case execCount == 0: // first exec is the mkfs for sdy
			assert.Equal(t, "--mkfs", args[3])
			createTestKeyring(t, configDir, args)
		case execCount == 1: // all remaining execs are for partitioning sdx then mkfs sdx
			assert.Equal(t, "sgdisk", command)
			assert.Equal(t, "--zap-all", args[0])
			assert.Equal(t, "/dev/"+nameSuffix, args[1])
		case execCount == 2:
			assert.Equal(t, "sgdisk", command)
			assert.Equal(t, "--clear", args[0])
			assert.Equal(t, "/dev/"+nameSuffix, args[2])
		case execCount == 3:
			assert.Equal(t, "/dev/"+nameSuffix, args[10])
		case execCount == 4:
			assert.Equal(t, "--mkfs", args[3])
			createTestKeyring(t, configDir, args)
		default:
			assert.Fail(t, fmt.Sprintf("unexpected case %d", execCount))
		}
		execCount++
		return nil
	}
	outputExecCount := 0
	executor.MockExecuteCommandWithOutput = func(name string, command string, args ...string) (string, error) {
		logger.Infof("OUTPUT %d for %s. %s %+v", outputExecCount, name, command, args)
		outputExecCount++
		return "", nil
	}

	context := &clusterd.Context{
		EtcdClient: etcdClient,
		Executor:   executor,
		NodeID:     nodeID,
		ConfigDir:  configDir,
		ProcMan:    proc.New(executor),
		Inventory:  createInventory(),
	}

	// Set sdx as already having an assigned osd id, a UUID and saved to the partition scheme.
	// The other device (sdy) will go through id selection, which is mocked in the createTestAgent method to return an id of 3.
	_, sdxUUID := mockPartitionSchemeEntry(t, 23, "sdx", configDir)

	// sdx should already have desired state set to have its data and metadata collocated on the sdx device
	etcdClient.SetValue(fmt.Sprintf("/rook/services/ceph/osd/desired/%s/device/%s/osd-id-data", nodeID, sdxUUID), "23")
	etcdClient.SetValue(fmt.Sprintf("/rook/services/ceph/osd/desired/%s/device/%s/osd-id-metadata", nodeID, sdxUUID), "23")

	// note only sdx already has a UUID (it's been through partitioning)
	context.Inventory.Local.Disks = []*inventory.LocalDisk{
		&inventory.LocalDisk{Name: "sdx", Size: 1234567890, UUID: sdxUUID},
		&inventory.LocalDisk{Name: "sdy", Size: 2234567890},
	}

	// prep the OSD agent and related orcehstration data
	prepAgentOrchestrationData(t, agent, etcdClient, context, clusterName)

	err = agent.ConfigureLocalService(context)
	assert.Nil(t, err)

	// wait for the async osds to complete
	<-agent.osdsCompleted

	assert.Equal(t, 0, agent.configCounter)
	assert.Equal(t, 5, execCount) // 1 mkfs for sdy, 3 partition steps for sdx, 1 mkfs for sdx
	assert.Equal(t, 2, outputExecCount)
	assert.Equal(t, 2, startCount) // 2 OSD procs should be started
	assert.Equal(t, 2, len(agent.osdProc), fmt.Sprintf("procs=%+v", agent.osdProc))

	err = agent.DestroyLocalService(context)
	assert.Nil(t, err)
	assert.Equal(t, 0, len(agent.osdProc))
}

func TestOSDAgentNoDevices(t *testing.T) {
	// set up a temporary config directory that will be cleaned up after test
	configDir, err := ioutil.TempDir("", "TestOSDAgentNoDevices")
	if err != nil {
		t.Fatalf("failed to create temp config dir: %+v", err)
	}
	defer os.RemoveAll(configDir)

	clusterName := "mycluster"
	os.MkdirAll(filepath.Join(configDir, "osd3"), 0744)

	// create a test OSD agent with no devices specified
	nodeID := "abc"
	etcdClient, agent, _ := createTestAgent(t, nodeID, "", configDir)

	startCount := 0
	executor := &exectest.MockExecutor{}
	executor.MockStartExecuteCommand = func(name string, command string, args ...string) (*exec.Cmd, error) {
		startCount++
		cmd := &exec.Cmd{Args: append([]string{command}, args...)}
		return cmd, nil
	}

	// should be no executeCommand calls
	runCount := 0
	executor.MockExecuteCommand = func(name string, command string, args ...string) error {
		runCount++
		createTestKeyring(t, configDir, args)
		return nil
	}

	// should be no executeCommandWithOutput calls
	outputExecCount := 0
	executor.MockExecuteCommandWithOutput = func(name string, command string, args ...string) (string, error) {
		assert.Fail(t, "executeCommandWithOutput is not expected for OSD local device")
		outputExecCount++
		return "", nil
	}

	// set up expected ProcManager commands
	context := &clusterd.Context{
		EtcdClient: etcdClient,
		Executor:   executor,
		NodeID:     nodeID,
		ProcMan:    proc.New(executor),
		ConfigDir:  configDir,
		Inventory:  createInventory(),
	}

	// prep the OSD agent and related orcehstration data
	prepAgentOrchestrationData(t, agent, etcdClient, context, clusterName)

	// configure the OSD and verify the results
	err = agent.Initialize(context)
	assert.Nil(t, err)
	etcdClient.SetValue(fmt.Sprintf("/rook/services/ceph/osd/desired/abc/dir/%s/osd-id-data", getPseudoDir(configDir)), "3")

	err = agent.ConfigureLocalService(context)
	assert.Nil(t, err)
	assert.Equal(t, 1, runCount)
	assert.Equal(t, 1, startCount)
	assert.Equal(t, 0, outputExecCount)
	assert.Equal(t, 1, len(agent.osdProc))

	// the local device should be marked as an applied OSD now
	osds, err := GetAppliedOSDs(context.NodeID, etcdClient)
	assert.Nil(t, err)
	assert.Equal(t, 1, len(osds))

	// destroy the OSD and verify the results
	err = agent.DestroyLocalService(context)
	assert.Nil(t, err)
	assert.Equal(t, 0, len(agent.osdProc))
}

func TestAppliedDevices(t *testing.T) {
	nodeID := "abc"
	etcdClient := util.NewMockEtcdClient()

	// no applied osds
	osds, err := GetAppliedOSDs(nodeID, etcdClient)
	assert.Nil(t, err)
	assert.Equal(t, 0, len(osds))

	// two applied osds
	appliedOSDKey := "/rook/services/ceph/osd/applied/abc"
	etcdClient.SetValue(path.Join(appliedOSDKey, "1", dataDiskUUIDKey), "1234")
	etcdClient.SetValue(path.Join(appliedOSDKey, "2", dataDiskUUIDKey), "2345")

	osds, err = GetAppliedOSDs(nodeID, etcdClient)
	assert.Nil(t, err)
	assert.Equal(t, 2, len(osds))
	assert.Equal(t, "1234", osds[1])
	assert.Equal(t, "2345", osds[2])
}

func TestRemoveDevice(t *testing.T) {
	// set up a temporary config directory that will be cleaned up after test
	configDir, err := ioutil.TempDir("", "TestRemoveDevice")
	if err != nil {
		t.Fatalf("failed to create temp config dir: %+v", err)
	}
	defer os.RemoveAll(configDir)

	nodeID := "a"
	etcdClient, agent, conn := createTestAgent(t, nodeID, "", configDir)
	executor := &exectest.MockExecutor{}

	context := &clusterd.Context{EtcdClient: etcdClient, NodeID: nodeID, Executor: executor, ProcMan: proc.New(executor), Inventory: createInventory()}
	context.Inventory.Local.Disks = []*inventory.LocalDisk{&inventory.LocalDisk{Name: "sda", Size: 1234567890, UUID: "5435435333"}}
	etcdClient.SetValue("/rook/services/ceph/osd/desired/a/device/5435435333/osd-id-data", "23")
	etcdClient.SetValue("/rook/services/ceph/osd/desired/a/device/5435435333/osd-id-metadata", "23")

	// create two applied osds, one of which is desired
	appliedRoot := "/rook/services/ceph/osd/applied/" + nodeID
	etcdClient.SetValue(path.Join(appliedRoot, "23", dataDiskUUIDKey), "5435435333")
	etcdClient.SetValue(path.Join(appliedRoot, "56", dataDiskUUIDKey), "2342342343")

	// removing the device will fail without the id
	err = agent.stopUndesiredDevices(context, conn)
	assert.Nil(t, err)

	applied := etcdClient.GetChildDirs(appliedRoot)
	assert.True(t, applied.Equals(util.CreateSet([]string{"23"})), fmt.Sprintf("applied=%+v", applied))
}

func createTestAgent(t *testing.T, nodeID, devices, configDir string) (*util.MockEtcdClient, *osdAgent, *testceph.MockConnection) {
	location := "root=here"
	forceFormat := false
	etcdClient := util.NewMockEtcdClient()
	factory := &testceph.MockConnectionFactory{}
	agent := NewAgent(factory, devices, "", forceFormat, location, partition.BluestoreConfig{})
	agent.cluster = &mon.ClusterInfo{Name: "myclust"}
	agent.Initialize(&clusterd.Context{EtcdClient: etcdClient, NodeID: nodeID, ConfigDir: configDir})
	if devices == "" {
		assert.Equal(t, configDir, etcdClient.GetValue(fmt.Sprintf(
			"/rook/services/ceph/osd/desired/%s/dir/%s/path", nodeID, getPseudoDir(configDir))))
	}

	conn, _ := factory.NewConnWithClusterAndUser("default", "user")
	mockConn := conn.(*testceph.MockConnection)
	mockConn.MockMonCommand = func(buf []byte) (buffer []byte, info string, err error) {
		response := "{\"key\":\"mysecurekey\", \"osdid\":3.0}"
		return []byte(response), "", nil
	}

	return etcdClient, agent, mockConn
}

func prepAgentOrchestrationData(t *testing.T, agent *osdAgent, etcdClient *util.MockEtcdClient, context *clusterd.Context, clusterName string) {
	key := path.Join(mon.CephKey, osdAgentName, clusterd.DesiredKey, context.NodeID)
	etcdClient.CreateDir(key)

	err := agent.Initialize(context)
	etcdClient.SetValue(path.Join(mon.CephKey, osdAgentName, clusterd.DesiredKey, context.NodeID, "ready"), "1")
	assert.Nil(t, err)

	// prep the etcd keys as if the leader initiated the orchestration
	etcdClient.SetValue(path.Join(mon.CephKey, "fsid"), "id")
	etcdClient.SetValue(path.Join(mon.CephKey, "name"), clusterName)
	etcdClient.SetValue(path.Join(mon.CephKey, "_secrets", "monitor"), "monsecret")
	etcdClient.SetValue(path.Join(mon.CephKey, "_secrets", "admin"), "adminsecret")

	monKey := path.Join(mon.CephKey, "monitor", clusterd.DesiredKey, context.NodeID)
	etcdClient.SetValue(path.Join(monKey, "id"), "1")
	etcdClient.SetValue(path.Join(monKey, "ipaddress"), "10.6.5.4")
	etcdClient.SetValue(path.Join(monKey, "port"), "8743")
}

func createTestKeyring(t *testing.T, configRoot string, args []string) {
	var configDir string
	if len(args) > 5 && strings.HasPrefix(args[5], "--id") {
		configDir = filepath.Join(configRoot, "osd") + args[5][5:]
		err := os.MkdirAll(configDir, 0744)
		assert.Nil(t, err)
		err = ioutil.WriteFile(path.Join(configDir, "keyring"), []byte("mykeyring"), 0644)
		assert.Nil(t, err)
	}
}

func TestDesiredDeviceState(t *testing.T) {
	nodeID := "a"
	etcdClient := util.NewMockEtcdClient()

	// add a device
	err := AddDesiredDevice(etcdClient, nodeID, "myuuid", 23)
	assert.Nil(t, err)
	devices := etcdClient.GetChildDirs("/rook/services/ceph/osd/desired/a/device")
	assert.Equal(t, 1, devices.Count())
	assert.True(t, devices.Contains("myuuid"))

	// remove the device
	err = RemoveDesiredDevice(etcdClient, nodeID, "myuuid")
	assert.Nil(t, err)
	devices = etcdClient.GetChildDirs("/rook/services/ceph/osd/desired/a/device")
	assert.Equal(t, 0, devices.Count())

	// removing a non-existent device is a no-op
	err = RemoveDesiredDevice(etcdClient, nodeID, "foo")
	assert.Nil(t, err)
}

func TestLoadDesiredDevices(t *testing.T) {
	etcdClient := util.NewMockEtcdClient()
	a := &osdAgent{desiredDevices: []string{}}

	// no devices are desired
	context := &clusterd.Context{EtcdClient: etcdClient, Inventory: createInventory(), NodeID: "a"}
	desired, err := a.loadDesiredDevices(context)
	assert.Nil(t, err)
	assert.Equal(t, 0, len(desired.Entries))

	// two devices and one metadata device are desired and it is a new config
	context.Inventory.Local.Disks = []*inventory.LocalDisk{
		&inventory.LocalDisk{Name: "sda", Size: 1234567890, UUID: "12345"},
		&inventory.LocalDisk{Name: "sdb", Size: 2234567890, UUID: "54321"},
		&inventory.LocalDisk{Name: "sdc", Size: 3234567890, UUID: "99999"},
	}
	a.desiredDevices = []string{"sda", "sdb"}
	a.metadataDevice = "sdc"
	desired, err = a.loadDesiredDevices(context)
	assert.Nil(t, err)
	assert.Equal(t, 3, len(desired.Entries))
	assert.Equal(t, DeviceOsdIDEntry{Data: -1, Metadata: nil}, *desired.Entries["sda"])
	assert.Equal(t, DeviceOsdIDEntry{Data: -1, Metadata: nil}, *desired.Entries["sdb"])
	assert.Equal(t, DeviceOsdIDEntry{Data: -1, Metadata: []int{}}, *desired.Entries["sdc"])

	// 3 devices are desired and they have previously been configured (with data on 2 devices and metadata for both on the 3rd)
	etcdClient.SetValue("/rook/services/ceph/osd/desired/a/device/12345/osd-id-data", "23")
	etcdClient.SetValue("/rook/services/ceph/osd/desired/a/device/54321/osd-id-data", "24")
	etcdClient.SetValue("/rook/services/ceph/osd/desired/a/device/99999/osd-id-metadata", "23,24")
	desired, err = a.loadDesiredDevices(context)
	assert.Nil(t, err)
	assert.Equal(t, 3, len(desired.Entries))
	assert.Equal(t, DeviceOsdIDEntry{Data: 23, Metadata: []int{}}, *desired.Entries["sda"])
	assert.Equal(t, DeviceOsdIDEntry{Data: 24, Metadata: []int{}}, *desired.Entries["sdb"])
	assert.Equal(t, DeviceOsdIDEntry{Data: -1, Metadata: []int{23, 24}}, *desired.Entries["sdc"])

	// no devices are desired but they have previously been configured, so they should be returned
	a.desiredDevices = []string{}
	desired, err = a.loadDesiredDevices(context)
	assert.Nil(t, err)
	assert.Equal(t, 3, len(desired.Entries))
	assert.Equal(t, DeviceOsdIDEntry{Data: 23, Metadata: []int{}}, *desired.Entries["sda"])
	assert.Equal(t, DeviceOsdIDEntry{Data: 24, Metadata: []int{}}, *desired.Entries["sdb"])
	assert.Equal(t, DeviceOsdIDEntry{Data: -1, Metadata: []int{23, 24}}, *desired.Entries["sdc"])
}

func TestDesiredDirsState(t *testing.T) {
	etcdClient := util.NewMockEtcdClient()

	// add a dir
	err := AddDesiredDir(etcdClient, "/my/dir", "a")
	assert.Nil(t, err)
	dirs := etcdClient.GetChildDirs("/rook/services/ceph/osd/desired/a/dir")
	assert.Equal(t, 1, dirs.Count())
	assert.True(t, dirs.Contains("my_dir"))
	assert.Equal(t, "/my/dir", etcdClient.GetValue("/rook/services/ceph/osd/desired/a/dir/my_dir/path"))

	loadedDirs, err := loadDesiredDirs(etcdClient, "a")
	assert.Nil(t, err)

	assert.Equal(t, 1, len(loadedDirs))
	assert.Equal(t, unassignedOSDID, loadedDirs["/my/dir"])
}

func TestGetPartitionPerfScheme(t *testing.T) {
	etcdClient := util.NewMockEtcdClient()
	context := &clusterd.Context{EtcdClient: etcdClient, Inventory: createInventory(), NodeID: "a"}

	// 3 disks: 2 for data and 1 for the metadata of both disks (2 WALs and 2 DBs)
	context.Inventory.Local.Disks = []*inventory.LocalDisk{
		&inventory.LocalDisk{Name: "sda", Size: 107374182400}, // 100 GB
		&inventory.LocalDisk{Name: "sdb", Size: 107374182400}, // 100 GB
		&inventory.LocalDisk{Name: "sdc", Size: 44158681088},  // 1 MB (starting offset) + 2 * (576 MB + 20 GB) = 41.125 GB
	}
	a := &osdAgent{desiredDevices: []string{"sda", "sdb"}, metadataDevice: "sdc"}

	devices, err := a.loadDesiredDevices(context)
	assert.Nil(t, err)
	assert.Equal(t, 3, len(devices.Entries))

	factory := &testceph.MockConnectionFactory{}
	conn, _ := factory.NewConnWithClusterAndUser("default", "user")
	mockConn := conn.(*testceph.MockConnection)

	// mock monitor command to return an osd ID when the client registers/creates an osd
	currOsdID := 10
	mockConn.MockMonCommand = func(args []byte) (buffer []byte, info string, err error) {
		switch {
		case strings.Index(string(args), "osd create") != -1:
			currOsdID++
			return []byte(fmt.Sprintf(`{"osdid": %d}`, currOsdID)), "info", nil
		}
		return nil, "", fmt.Errorf("unexpected mon_command '%s'", string(args))
	}

	scheme, err := getPartitionPerfScheme(context, conn, devices, partition.BluestoreConfig{})
	assert.Nil(t, err)
	assert.Equal(t, 2, len(scheme.Entries))

	// verify the metadata entries, they should be on sdc and there should be 4 of them (2 per OSD)
	assert.NotNil(t, scheme.Metadata)
	assert.Equal(t, "sdc", scheme.Metadata.Device)
	assert.Equal(t, 4, len(scheme.Metadata.Partitions))

	// verify the first entry in the performance partition scheme.  note that the block device will either be sda or
	// sdb because ordering of map traversal in golang isn't guaranteed.  Ensure that the first is either sda or sdb
	// and that the second is the other one.
	entry := scheme.Entries[0]
	assert.Equal(t, 11, entry.ID)
	firstBlockDevice := entry.Partitions[partition.BlockPartitionName].Device
	assert.True(t, firstBlockDevice == "sda" || firstBlockDevice == "sdb", firstBlockDevice)
	verifyPartitionEntry(t, entry.Partitions[partition.BlockPartitionName], firstBlockDevice, -1, 1)
	verifyPartitionEntry(t, entry.Partitions[partition.WalPartitionName], "sdc", partition.WalDefaultSizeMB, 1)
	verifyPartitionEntry(t, entry.Partitions[partition.DatabasePartitionName], "sdc", partition.DBDefaultSizeMB, 577)

	// verify the second entry in the scheme.  Note the comment above about sda vs. sdb ordering.
	entry = scheme.Entries[1]
	assert.Equal(t, 12, entry.ID)
	var secondBlockDevice string
	if firstBlockDevice == "sda" {
		secondBlockDevice = "sdb"
	} else {
		secondBlockDevice = "sda"
	}
	verifyPartitionEntry(t, entry.Partitions[partition.BlockPartitionName], secondBlockDevice, -1, 1)
	verifyPartitionEntry(t, entry.Partitions[partition.WalPartitionName], "sdc", partition.WalDefaultSizeMB, 21057)
	verifyPartitionEntry(t, entry.Partitions[partition.DatabasePartitionName], "sdc", partition.DBDefaultSizeMB, 21633)
}

func TestGetPartitionPerfSchemeDiskInUse(t *testing.T) {
	configDir, err := ioutil.TempDir("", "TestGetPartitionPerfSchemeDiskInUse")
	if err != nil {
		t.Fatalf("failed to create temp config dir: %+v", err)
	}
	defer os.RemoveAll(configDir)

	etcdClient := util.NewMockEtcdClient()
	context := &clusterd.Context{EtcdClient: etcdClient, Inventory: createInventory(), NodeID: "a", ConfigDir: configDir}

	// mock device sda having been already partitioned
	_, sdaUUID := mockPartitionSchemeEntry(t, 1, "sda", configDir)

	context.Inventory.Local.Disks = []*inventory.LocalDisk{
		&inventory.LocalDisk{Name: "sda", Size: 107374182400, UUID: sdaUUID}, // 100 GB
	}
	a := &osdAgent{desiredDevices: []string{"sda"}}

	// mock device sda already being saved to desired state
	etcdClient.SetValue(fmt.Sprintf("/rook/services/ceph/osd/desired/a/device/%s/osd-id-data", sdaUUID), "1")
	etcdClient.SetValue(fmt.Sprintf("/rook/services/ceph/osd/desired/a/device/%s/osd-id-metadata", sdaUUID), "1")

	// load desired devices, this should return that sda is desired to have osd 1
	devices, err := a.loadDesiredDevices(context)
	assert.Nil(t, err)
	assert.Equal(t, 1, len(devices.Entries))
	assert.Equal(t, 1, devices.Entries["sda"].Data)
	assert.Equal(t, []int{1}, devices.Entries["sda"].Metadata)

	// get the partition scheme based on the desired devices.  Since sda is already in use, the partition
	// scheme returned should reflect that.
	scheme, err := getPartitionPerfScheme(context, nil, devices, partition.BluestoreConfig{})
	assert.Nil(t, err)

	// the partition scheme should have a single entry for osd 1 on sda and it should have collocated data and metadata
	assert.NotNil(t, scheme)
	assert.Equal(t, 1, len(scheme.Entries))
	assert.Equal(t, 1, scheme.Entries[0].ID)
	assert.Equal(t, 3, len(scheme.Entries[0].Partitions))
	for _, p := range scheme.Entries[0].Partitions {
		assert.Equal(t, "sda", p.Device)
		assert.Equal(t, sdaUUID, p.DiskUUID)
	}

	// there should be no dedicated metadata partitioning because sda has osd 1 collocated on it
	assert.Nil(t, scheme.Metadata)
}

func TestGetPartitionPerfSchemeDiskNameChanged(t *testing.T) {
	configDir, err := ioutil.TempDir("", "TestGetPartitionPerfSchemeDiskNameChanged")
	if err != nil {
		t.Fatalf("failed to create temp config dir: %+v", err)
	}
	defer os.RemoveAll(configDir)

	etcdClient := util.NewMockEtcdClient()
	context := &clusterd.Context{EtcdClient: etcdClient, Inventory: createInventory(), NodeID: "a", ConfigDir: configDir}

	// setup an existing partition schme with metadata on nvme01 and data on sda
	_, metadataUUID, sdaUUID := mockDistributedPartitionScheme(t, 1, "nvme01", "sda", configDir)

	// mock the currently discovered hardware, note the device names have changed (e.g., across reboots) but their UUIDs are always static
	context.Inventory.Local.Disks = []*inventory.LocalDisk{
		&inventory.LocalDisk{Name: "nvme01-changed", Size: 107374182400, UUID: metadataUUID},
		&inventory.LocalDisk{Name: "sda-changed", Size: 107374182400, UUID: sdaUUID},
	}
	a := &osdAgent{desiredDevices: []string{"sda-changed"}}

	// mock the 2 devices as being committed to desired state already then load desired devices
	etcdClient.SetValue(fmt.Sprintf("/rook/services/ceph/osd/desired/a/device/%s/osd-id-data", sdaUUID), "1")
	etcdClient.SetValue(fmt.Sprintf("/rook/services/ceph/osd/desired/a/device/%s/osd-id-metadata", metadataUUID), "1")
	devices, err := a.loadDesiredDevices(context)
	assert.Nil(t, err)

	// get the current partition scheme.  This should notice that the device names changed and update the
	// partition scheme to have the latest device names
	scheme, err := getPartitionPerfScheme(context, nil, devices, partition.BluestoreConfig{})
	assert.Nil(t, err)
	assert.NotNil(t, scheme)
	assert.Equal(t, "nvme01-changed", scheme.Metadata.Device)
	assert.Equal(t, "sda-changed", scheme.Entries[0].Partitions[partition.BlockPartitionName].Device)
	assert.Equal(t, "nvme01-changed", scheme.Entries[0].Partitions[partition.WalPartitionName].Device)
	assert.Equal(t, "nvme01-changed", scheme.Entries[0].Partitions[partition.DatabasePartitionName].Device)
}

func createInventory() *inventory.Config {
	return &inventory.Config{Local: &inventory.Hardware{Disks: []*inventory.LocalDisk{}}}
}

func verifyPartitionEntry(t *testing.T, actual *partition.PerfSchemePartitionDetails, expectedDevice string,
	expectedSize int64, expectedOffset int64) {

	assert.Equal(t, expectedDevice, actual.Device)
	assert.Equal(t, expectedSize, actual.SizeMB)
	assert.Equal(t, expectedOffset, actual.OffsetMB)
}

func mockPartitionSchemeEntry(t *testing.T, osdID int, device, configDir string) (entry *partition.PerfSchemeEntry, diskUUID string) {
	entry = partition.NewPerfSchemeEntry()
	entry.ID = osdID
	entry.OsdUUID = uuid.Must(uuid.NewRandom())
	partition.PopulateCollocatedPerfSchemeEntry(entry, device, partition.BluestoreConfig{})
	scheme := partition.NewPerfScheme()
	scheme.Entries = append(scheme.Entries, entry)
	err := scheme.Save(configDir)
	assert.Nil(t, err)

	// figure out what random UUID got assigned to the device
	for _, p := range entry.Partitions {
		diskUUID = p.DiskUUID
		break
	}
	assert.NotEqual(t, "", diskUUID)

	return entry, diskUUID
}

func mockDistributedPartitionScheme(t *testing.T, osdID int, metadataDevice, device, configDir string) (*partition.PerfScheme, string, string) {
	scheme := partition.NewPerfScheme()
	scheme.Metadata = partition.NewMetadataDeviceInfo(metadataDevice)

	entry := partition.NewPerfSchemeEntry()
	entry.ID = osdID
	entry.OsdUUID = uuid.Must(uuid.NewRandom())

	partition.PopulateDistributedPerfSchemeEntry(entry, device, scheme.Metadata, partition.BluestoreConfig{})
	scheme.Entries = append(scheme.Entries, entry)
	err := scheme.Save(configDir)
	assert.Nil(t, err)

	// return the full partition scheme, the metadata device UUID and the data device UUID
	return scheme, scheme.Metadata.DiskUUID, entry.Partitions[partition.BlockPartitionName].DiskUUID
}