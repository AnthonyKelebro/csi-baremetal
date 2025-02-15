/*
Copyright © 2020 Dell Inc. or its subsidiaries. All Rights Reserved.

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

package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	k8sError "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	api "github.com/dell/csi-baremetal/api/generated/v1"
	apiV1 "github.com/dell/csi-baremetal/api/v1"
	accrd "github.com/dell/csi-baremetal/api/v1/availablecapacitycrd"
	"github.com/dell/csi-baremetal/api/v1/drivecrd"
	"github.com/dell/csi-baremetal/api/v1/lvgcrd"
	vcrd "github.com/dell/csi-baremetal/api/v1/volumecrd"
	"github.com/dell/csi-baremetal/pkg/base"
	"github.com/dell/csi-baremetal/pkg/base/k8s"
	dataDiscover "github.com/dell/csi-baremetal/pkg/base/linuxutils/datadiscover/types"
	"github.com/dell/csi-baremetal/pkg/base/linuxutils/fs"
	"github.com/dell/csi-baremetal/pkg/base/linuxutils/lsblk"
	"github.com/dell/csi-baremetal/pkg/base/util"
	"github.com/dell/csi-baremetal/pkg/eventing"
	"github.com/dell/csi-baremetal/pkg/mocks"
	mocklu "github.com/dell/csi-baremetal/pkg/mocks/linuxutils"
	mockProv "github.com/dell/csi-baremetal/pkg/mocks/provisioners"
	p "github.com/dell/csi-baremetal/pkg/node/provisioners"
)

// TODO: refactor these UTs - https://github.com/dell/csi-baremetal/issues/90

var (
	testErr            = errors.New("error")
	lsblkAllDevicesCmd = fmt.Sprintf(lsblk.CmdTmpl, "")

	drive1UUID   = uuid.New().String()
	drive2UUID   = uuid.New().String()
	testPartUUID = uuid.New().String()

	drive1 = api.Drive{
		UUID:         drive1UUID,
		SerialNumber: "hdd1-serial",
		Size:         1024 * 1024 * 1024 * 500,
		NodeId:       nodeID,
		Type:         apiV1.DriveTypeHDD,
		Status:       apiV1.DriveStatusOnline,
		Health:       apiV1.HealthGood,
		Path:         "/dev/sda",
		IsSystem:     true,
	} // /dev/sda in LsblkTwoDevices

	drive2 = api.Drive{
		UUID:         drive2UUID,
		SerialNumber: "hdd2-serial",
		Size:         1024 * 1024 * 1024 * 200,
		NodeId:       nodeID,
		Type:         apiV1.DriveTypeHDD,
		Status:       apiV1.DriveStatusOnline,
		Health:       apiV1.HealthGood,
		Path:         "/dev/sdb",
		IsSystem:     true,
	} // /dev/sdb in LsblkTwoDevices

	// block device that corresponds to the drive1
	bdev1 = lsblk.BlockDevice{
		Name:     drive1.Path,
		Type:     drive1.Type,
		Size:     lsblk.CustomInt64{Int64: drive1.Size},
		Serial:   drive1.SerialNumber,
		Children: nil,
	}

	// block device that corresponds to the drive2
	bdev2 = lsblk.BlockDevice{
		Name:     drive2.Path,
		Type:     drive2.Type,
		Size:     lsblk.CustomInt64{Int64: drive1.Size},
		Serial:   drive2.SerialNumber,
		Children: nil,
	}

	// todo don't hardcode device name
	lsblkSingleDeviceCmd = fmt.Sprintf(lsblk.CmdTmpl, "/dev/sda")

	testDriveCR = drivecrd.Drive{
		TypeMeta: v1.TypeMeta{Kind: "Drive", APIVersion: apiV1.APIV1Version},
		ObjectMeta: v1.ObjectMeta{
			Name:              drive1.UUID,
			Namespace:         testNs,
			CreationTimestamp: v1.Time{Time: time.Now()},
		},
		Spec: drive1,
	}

	volCR = vcrd.Volume{
		TypeMeta: v1.TypeMeta{Kind: "Volume", APIVersion: apiV1.APIV1Version},
		ObjectMeta: v1.ObjectMeta{
			Name:              testID,
			Namespace:         testNs,
			CreationTimestamp: v1.Time{Time: time.Now()},
		},
		Spec: api.Volume{
			Id:           testID,
			Size:         1024 * 1024 * 1024 * 150,
			StorageClass: apiV1.StorageClassHDD,
			// TODO location cannot be empty - need to add check
			Location:  drive1UUID,
			CSIStatus: apiV1.Creating,
			NodeId:    nodeID,
			Mode:      apiV1.ModeFS,
			Type:      string(fs.XFS),
		},
	}

	testLVGCR = lvgcrd.LogicalVolumeGroup{
		TypeMeta: v1.TypeMeta{
			Kind:       "LogicalVolumeGroup",
			APIVersion: apiV1.APIV1Version,
		},
		ObjectMeta: v1.ObjectMeta{
			Name:      testLVGName,
			Namespace: testNs,
		},
		Spec: api.LogicalVolumeGroup{
			Name:       testLVGName,
			Node:       nodeID,
			Locations:  []string{drive1.UUID},
			Size:       int64(1024 * 500 * util.GBYTE),
			Status:     apiV1.Created,
			VolumeRefs: []string{},
		},
	}

	testVolumeLVGCR = vcrd.Volume{
		TypeMeta: v1.TypeMeta{Kind: "Volume", APIVersion: apiV1.APIV1Version},
		ObjectMeta: v1.ObjectMeta{
			Name:              volLVGName,
			Namespace:         testNs,
			CreationTimestamp: v1.Time{Time: time.Now()},
		},
		Spec: api.Volume{
			Id:           volLVGName,
			Size:         1024 * 1024 * 1024 * 150,
			StorageClass: apiV1.StorageClassHDDLVG,
			Location:     testLVGCR.Name,
			CSIStatus:    apiV1.Creating,
			NodeId:       nodeID,
			Mode:         apiV1.ModeFS,
			Type:         string(fs.XFS),
		},
	}

	acCR = accrd.AvailableCapacity{
		TypeMeta:   v1.TypeMeta{Kind: "AvailableCapacity", APIVersion: apiV1.APIV1Version},
		ObjectMeta: v1.ObjectMeta{Name: driveUUID, Namespace: testNs},
		Spec: api.AvailableCapacity{
			Size:         drive1.Size,
			StorageClass: apiV1.StorageClassHDD,
			Location:     "drive-uuid",
			NodeId:       drive1.NodeId},
	}
)

func getTestDrive(id, sn string) *api.Drive {
	return &api.Drive{
		UUID:         id,
		SerialNumber: sn,
		Size:         1024 * 1024 * 1024 * 500,
		NodeId:       nodeID,
		Type:         apiV1.DriveTypeHDD,
		Status:       apiV1.DriveStatusOnline,
		Health:       apiV1.HealthGood,
	}
}

func TestVolumeManager_NewVolumeManager(t *testing.T) {
	kubeClient, err := k8s.GetFakeKubeClient(testNs, testLogger)
	assert.Nil(t, err)
	vm := NewVolumeManager(nil, nil, testLogger, kubeClient, kubeClient, new(mocks.NoOpRecorder), nodeID)
	assert.NotNil(t, vm)
	assert.Nil(t, vm.driveMgrClient)
	assert.NotNil(t, vm.fsOps)
	assert.NotNil(t, vm.lvmOps)
	assert.NotNil(t, vm.listBlk)
	assert.NotNil(t, vm.partOps)
	assert.True(t, len(vm.provisioners) > 0)
	assert.NotNil(t, vm.acProvider)
	assert.NotNil(t, vm.crHelper)
}

func TestReconcile_MultipleRequest(t *testing.T) {
	//Try to create volume multiple time in go routine, expect that CSI status Created and volume was created without error
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNs, Name: volCR.Name}}
	kubeClient, err := k8s.GetFakeKubeClient(testNs, testLogger)
	assert.Nil(t, err)
	vm := NewVolumeManager(nil, nil, testLogger, kubeClient, kubeClient, new(mocks.NoOpRecorder), nodeID)
	volCR.Spec.CSIStatus = apiV1.Creating
	err = vm.k8sClient.CreateCR(testCtx, volCR.Name, &volCR)
	assert.Nil(t, err)

	pMock := mockProv.GetMockProvisionerSuccess("/some/path")
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.DriveBasedVolumeType: pMock})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		res, err := vm.Reconcile(req)
		assert.Nil(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		wg.Done()
	}()
	wg.Add(1)
	go func() {
		res, err := vm.Reconcile(req)
		assert.Nil(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		wg.Done()
	}()
	wg.Wait()
	volume := &vcrd.Volume{}
	err = vm.k8sClient.ReadCR(testCtx, req.Name, testNs, volume)
	assert.Nil(t, err)
	assert.Equal(t, apiV1.Created, volume.Spec.CSIStatus)
}

func TestReconcile_SuccessNotFound(t *testing.T) {
	kubeClient, err := k8s.GetFakeKubeClient(testNs, testLogger)
	assert.Nil(t, err)
	vm := NewVolumeManager(nil, nil, testLogger, kubeClient, kubeClient, new(mocks.NoOpRecorder), nodeID)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNs, Name: "not-found-that-name"}}
	res, err := vm.Reconcile(req)
	assert.Nil(t, err)
	assert.Equal(t, res, ctrl.Result{})
}

func TestVolumeManager_prepareVolume(t *testing.T) {
	var (
		vm     *VolumeManager
		req    = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNs, Name: volCR.Name}}
		volume = &vcrd.Volume{}
		pMock  *mockProv.MockProvisioner
		res    ctrl.Result
		err    error
	)

	// happy pass
	vm = prepareSuccessVolumeManager(t)
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, volCR.Name, &volCR))
	pMock = mockProv.GetMockProvisionerSuccess("/some/path")
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.DriveBasedVolumeType: pMock})

	testVol := volCR
	res, err = vm.prepareVolume(testCtx, &testVol)
	assert.Nil(t, err)
	assert.Equal(t, res, ctrl.Result{})
	err = vm.k8sClient.ReadCR(testCtx, req.Name, testNs, volume)
	assert.Nil(t, err)
	assert.Equal(t, volume.Spec.CSIStatus, apiV1.Created)

	// failed to update
	vm = prepareSuccessVolumeManager(t)
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.DriveBasedVolumeType: pMock})

	res, err = vm.prepareVolume(testCtx, &testVol)
	assert.NotNil(t, err)
	assert.True(t, res.Requeue)

	// PrepareVolume failed
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, volCR.Name, &volCR))
	pMock = &mockProv.MockProvisioner{}
	pMock.On("PrepareVolume", volCR.Spec).Return(testErr)
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.DriveBasedVolumeType: pMock})

	res, err = vm.prepareVolume(testCtx, &volCR)
	assert.NotNil(t, err)
	assert.Equal(t, res, ctrl.Result{})
	err = vm.k8sClient.ReadCR(testCtx, req.Name, testNs, volume)
	assert.Nil(t, err)
	assert.Equal(t, volume.Spec.CSIStatus, apiV1.Failed)
}

func TestVolumeManager_handleRemovingStatus(t *testing.T) {
	var (
		vm     *VolumeManager
		req    = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNs, Name: volCR.Name}}
		volume = &vcrd.Volume{}
		res    ctrl.Result
		err    error
	)

	// happy path
	vm = prepareSuccessVolumeManager(t)
	testVol := volCR
	testVol.Spec.CSIStatus = apiV1.Removing
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, volCR.Name, &testVol))
	pMock := mockProv.GetMockProvisionerSuccess("/some/path")
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.DriveBasedVolumeType: pMock})

	res, err = vm.handleRemovingStatus(testCtx, &testVol)
	assert.Nil(t, err)
	assert.Equal(t, res, ctrl.Result{})
	err = vm.k8sClient.ReadCR(testCtx, req.Name, testNs, volume)
	assert.Nil(t, err)
	assert.Equal(t, apiV1.Removed, volume.Spec.CSIStatus)

	// failed to update
	vm = prepareSuccessVolumeManager(t)
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.DriveBasedVolumeType: pMock})

	res, err = vm.handleRemovingStatus(testCtx, &volCR)
	assert.NotNil(t, err)
	assert.True(t, res.Requeue)

	// ReleaseVolume failed
	testVol = volCR
	testVol.Spec.CSIStatus = apiV1.Removing
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, volCR.Name, &volCR))
	pMock = &mockProv.MockProvisioner{}
	pMock.On("ReleaseVolume", volCR.Spec).Return(testErr)
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.DriveBasedVolumeType: pMock})

	res, err = vm.handleRemovingStatus(testCtx, &volCR)
	assert.NotNil(t, err)
	assert.Equal(t, res, ctrl.Result{})
	err = vm.k8sClient.ReadCR(testCtx, req.Name, testNs, volume)
	assert.Nil(t, err)
	assert.Equal(t, volume.Spec.CSIStatus, apiV1.Failed)

}

func TestVolumeManager_handleRemovingStatus_DeleteVolume(t *testing.T) {
	drive := drive1
	drive.UUID = driveUUID
	drive.Health = apiV1.HealthGood
	testVol := volCR
	testVol.Spec.Location = drive.UUID
	testVol.Spec.CSIStatus = apiV1.Removing

	vm := prepareSuccessVolumeManagerWithDrives([]*api.Drive{&drive}, t)
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testVol.Name, &testVol))

	pMock := &mockProv.MockProvisioner{}
	pMock.On("ReleaseVolume", testVol.Spec).Return(testErr)
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.DriveBasedVolumeType: pMock})

	res, err := vm.handleRemovingStatus(testCtx, &testVol)
	assert.Error(t, testErr)

	drivecrd := &drivecrd.Drive{}
	err = vm.k8sClient.ReadCR(context.Background(), testVol.Spec.Location, "", drivecrd)
	assert.Nil(t, err)

	assert.Equal(t, res, ctrl.Result{})
	assert.Equal(t, apiV1.DriveUsageFailed, drivecrd.Spec.Usage)
}

func TestReconcile_SuccessDeleteVolume(t *testing.T) {
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNs, Name: volCR.Name}}
	kubeClient, err := k8s.GetFakeKubeClient(testNs, testLogger)
	assert.Nil(t, err)
	vm := NewVolumeManager(nil, nil, testLogger, kubeClient, kubeClient, new(mocks.NoOpRecorder), nodeID)
	volCR.Spec.CSIStatus = apiV1.Removed
	err = vm.k8sClient.CreateCR(testCtx, volCR.Name, &volCR)
	assert.Nil(t, err)

	pMock := mockProv.GetMockProvisionerSuccess("/some/path")
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.DriveBasedVolumeType: pMock})

	err = vm.k8sClient.CreateCR(testCtx, testDriveCR.Name, &testDriveCR)
	assert.Nil(t, err)

	//successfully add finalizer
	res, err := vm.Reconcile(req)
	assert.Nil(t, err)
	assert.Equal(t, res, ctrl.Result{})

	//successfully remove finalizer
	volCR.ObjectMeta.DeletionTimestamp = &v1.Time{Time: time.Now()}
	err = vm.k8sClient.UpdateCR(testCtx, &volCR)
	assert.Nil(t, err)

	res, err = vm.Reconcile(req)
	assert.NotNil(t, k8sError.IsNotFound(err))
	assert.Equal(t, res, ctrl.Result{})
}

func TestVolumeManager_handleCreatingVolumeInLVG(t *testing.T) {
	var (
		vm                 *VolumeManager
		pMock              *mockProv.MockProvisioner
		vol                *vcrd.Volume
		lvg                *lvgcrd.LogicalVolumeGroup
		testVol            vcrd.Volume
		testLVG            lvgcrd.LogicalVolumeGroup
		expectedResRequeue = ctrl.Result{Requeue: true, RequeueAfter: base.DefaultRequeueForVolume}
		res                ctrl.Result
		err                error
	)

	// unable to read LogicalVolumeGroup (not found) and unable to update corresponding volume CR
	vm = prepareSuccessVolumeManager(t)

	res, err = vm.handleCreatingVolumeInLVG(testCtx, &testVol)
	assert.NotNil(t, err)
	assert.True(t, k8sError.IsNotFound(err))
	assert.Equal(t, expectedResRequeue, res)

	// LogicalVolumeGroup is not found, volume CR was updated successfully (CSIStatus=failed)
	vm = prepareSuccessVolumeManager(t)
	testVol = testVolumeLVGCR
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testVol.Name, &testVol))

	res, err = vm.handleCreatingVolumeInLVG(testCtx, &testVol)
	assert.Nil(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	vol = &vcrd.Volume{}
	assert.Nil(t, vm.k8sClient.ReadCR(testCtx, testVol.Name, testVol.Namespace, vol))
	assert.Equal(t, apiV1.Failed, vol.Spec.CSIStatus)

	// LogicalVolumeGroup in creating state
	vm = prepareSuccessVolumeManager(t)
	testLVG = testLVGCR
	testLVG.Spec.Status = apiV1.Creating
	testVol = testVolumeLVGCR
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testLVG.Name, &testLVG))

	res, err = vm.handleCreatingVolumeInLVG(testCtx, &testVol)
	assert.Nil(t, err)
	assert.Equal(t, expectedResRequeue, res)

	// LogicalVolumeGroup in failed state and volume is updated successfully
	vm = prepareSuccessVolumeManager(t)
	testLVG = testLVGCR
	testLVG.Spec.Status = apiV1.Failed
	testVol = testVolumeLVGCR
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testLVG.Name, &testLVG))
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testVol.Name, &testVol))

	res, err = vm.handleCreatingVolumeInLVG(testCtx, &testVol)
	assert.Nil(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	vol = &vcrd.Volume{}
	assert.Nil(t, vm.k8sClient.ReadCR(testCtx, testVol.Name, testVol.Namespace, vol))
	assert.Equal(t, apiV1.Failed, vol.Spec.CSIStatus)

	// LogicalVolumeGroup in failed state and volume is failed to update
	vm = prepareSuccessVolumeManager(t)
	testLVG = testLVGCR
	testLVG.Spec.Status = apiV1.Failed
	testVol = testVolumeLVGCR
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testLVG.Name, &testLVG))

	res, err = vm.handleCreatingVolumeInLVG(testCtx, &testVol)
	assert.NotNil(t, err)
	assert.Equal(t, expectedResRequeue, res)
	assert.True(t, k8sError.IsNotFound(err))

	// LogicalVolumeGroup in created state and volume.ID is not in VolumeRefs
	vm = prepareSuccessVolumeManager(t)
	pMock = &mockProv.MockProvisioner{}
	pMock.On("PrepareVolume", mock.Anything).Return(nil)
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.LVMBasedVolumeType: pMock})
	testLVG = testLVGCR
	testLVG.Spec.Status = apiV1.Created
	testVol = testVolumeLVGCR
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testLVG.Name, &testLVG))
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testVol.Name, &testVol))

	res, err = vm.handleCreatingVolumeInLVG(testCtx, &testVol)
	assert.Nil(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	lvg = &lvgcrd.LogicalVolumeGroup{}
	assert.Nil(t, vm.k8sClient.ReadCR(testCtx, testLVG.Name, "", lvg))
	assert.True(t, util.ContainsString(lvg.Spec.VolumeRefs, testVol.Spec.Id))

	// LogicalVolumeGroup in created state and volume.ID is in VolumeRefs
	vm = prepareSuccessVolumeManager(t)
	pMock = &mockProv.MockProvisioner{}
	pMock.On("PrepareVolume", mock.Anything).Return(nil)
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.LVMBasedVolumeType: pMock})
	testVol = testVolumeLVGCR
	testLVG = testLVGCR
	testLVG.Spec.Status = apiV1.Created
	testLVG.Spec.VolumeRefs = []string{testVol.Spec.Id}
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testLVG.Name, &testLVG))
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testVol.Name, &testVol))

	res, err = vm.handleCreatingVolumeInLVG(testCtx, &testVol)
	assert.Nil(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	lvg = &lvgcrd.LogicalVolumeGroup{}
	assert.Nil(t, vm.k8sClient.ReadCR(testCtx, testLVG.Name, "", lvg))
	assert.True(t, util.ContainsString(lvg.Spec.VolumeRefs, testVol.Spec.Id))
	assert.Equal(t, 1, len(lvg.Spec.VolumeRefs))

	// LogicalVolumeGroup state wasn't recognized
	vm = prepareSuccessVolumeManager(t)
	testLVG = testLVGCR
	testLVG.Spec.Status = ""
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testLVG.Name, &testLVG))

	res, err = vm.handleCreatingVolumeInLVG(testCtx, &testVol)
	assert.Nil(t, err)
	assert.Equal(t, expectedResRequeue, res)
}

func TestReconcile_ReconcileDefaultStatus(t *testing.T) {
	var (
		vm  *VolumeManager
		req = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNs, Name: volCR.Name}}
		res ctrl.Result
		err error
	)

	vm = prepareSuccessVolumeManager(t)
	volCR.Spec.CSIStatus = apiV1.Published
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, volCR.Name, &volCR))

	res, err = vm.Reconcile(req)
	assert.Nil(t, err)
	assert.Equal(t, res, ctrl.Result{})
}

func TestNewVolumeManager_SetProvisioners(t *testing.T) {
	vm := NewVolumeManager(nil, mocks.EmptyExecutorSuccess{},
		logrus.New(), nil, nil, new(mocks.NoOpRecorder), nodeID)
	newProv := &mockProv.MockProvisioner{}
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.DriveBasedVolumeType: newProv})
	assert.Equal(t, newProv, vm.provisioners[p.DriveBasedVolumeType])
}

func TestVolumeManager_DiscoverFail(t *testing.T) {
	var (
		vm  *VolumeManager
		err error
	)

	t.Run("driveMgr return error", func(t *testing.T) {
		vm = prepareSuccessVolumeManager(t)
		vm.driveMgrClient = &mocks.MockDriveMgrClientFail{}

		err = vm.Discover()
		assert.NotNil(t, err)
		assert.Equal(t, "drivemgr error", err.Error())
	})

	t.Run("update driveCRs failed", func(t *testing.T) {
		mockK8sClient := &mocks.K8Client{}
		kubeClient := k8s.NewKubeClient(mockK8sClient, testLogger, testNs)
		// expect: updateDrivesCRs failed
		vm = NewVolumeManager(&mocks.MockDriveMgrClient{},
			nil, testLogger, kubeClient, kubeClient, nil, nodeID)
		mockK8sClient.On("List", mock.Anything, mock.Anything, mock.Anything).Return(testErr).Once()

		err = vm.Discover()
		assert.NotNil(t, err)
		assert.Contains(t, err.Error(), "updateDrivesCRs return error")
	})

	t.Run("discoverDataOnDrives failed", func(t *testing.T) {
		mockK8sClient := &mocks.K8Client{}
		kubeClient := k8s.NewKubeClient(mockK8sClient, testLogger, testNs)
		vm = NewVolumeManager(&mocks.MockDriveMgrClient{}, nil, testLogger, kubeClient, kubeClient, nil, nodeID)
		discoverData := &mocklu.MockWrapDataDiscover{}
		discoverData.On("DiscoverData", mock.Anything, mock.Anything).Return(false, testErr).Once()
		vm.dataDiscover = discoverData
		vm.discoverSystemLVG = false
		mockK8sClient.On("List", mock.Anything, &drivecrd.DriveList{}, mock.Anything).Return(nil)
		mockK8sClient.On("List", mock.Anything, &lvgcrd.LogicalVolumeGroupList{}, mock.Anything).Return(nil)
		mockK8sClient.On("List", mock.Anything, &vcrd.VolumeList{}, mock.Anything).Return(testErr)

		err = vm.Discover()
		assert.NotNil(t, err)
		assert.Contains(t, err.Error(), "discoverDataOnDrives return error")
	})
}

func TestVolumeManager_DiscoverSuccess(t *testing.T) {
	var (
		vm             *VolumeManager
		driveMgrClient = mocks.NewMockDriveMgrClient(getDriveMgrRespBasedOnDrives(drive1, drive2))
		err            error
	)

	vm = prepareSuccessVolumeManager(t)
	vm.driveMgrClient = driveMgrClient
	discoverData := &mocklu.MockWrapDataDiscover{}
	discoverData.On("DiscoverData", mock.Anything, mock.Anything).Return(&dataDiscover.DiscoverResult{}, nil)
	vm.dataDiscover = discoverData
	// expect that Volume CRs won't be created because of all drives don't have children
	err = vm.Discover()
	assert.Nil(t, err)
}

func TestVolumeManager_Discover_noncleanDisk(t *testing.T) {
	/*
		test scenario consists of 2 Discover iteration on first:
		 - driveMgr returned 2 drives and there are no partition on them from lsblk response, expect that
		   2 Drive CRs, 2 AC CRs and 0 Volume CR will be created
		on second iteration:
		 - driveMgr returned 2 drives and on one of them lsblk detect partition, expect that amount of Drive CRs won't be changed
		   1 Volume CR will be created and on AC CR will be removed (1 AC remains)
	*/

	// fist iteration
	vm := prepareSuccessVolumeManager(t)
	vm.driveMgrClient = mocks.NewMockDriveMgrClient([]*api.Drive{&drive1, &drive2})
	dItems := getDriveCRsListItems(t, vm.k8sClient)
	assert.Equal(t, 0, len(dItems))

	discoverData := &mocklu.MockWrapDataDiscover{}
	testResultDrive1 := &dataDiscover.DiscoverResult{
		Message: fmt.Sprintf("Drive with path %s, SN %s, doesn't have filesystem, partition table, partitions and PV", drive1.Path, drive1.SerialNumber),
		HasData: false,
	}
	testResultDrive2 := &dataDiscover.DiscoverResult{
		Message: fmt.Sprintf("Drive with path %s, SN %s, doesn't have filesystem, partition table, partitions and PV", drive2.Path, drive2.SerialNumber),
		HasData: false,
	}
	discoverData.On("DiscoverData", drive1.Path, drive1.SerialNumber).Return(testResultDrive1, nil)
	discoverData.On("DiscoverData", drive2.Path, drive2.SerialNumber).Return(testResultDrive2, nil)
	vm.dataDiscover = discoverData

	err := vm.Discover()
	assert.Nil(t, err)

	dItems = getDriveCRsListItems(t, vm.k8sClient)
	assert.Equal(t, 2, len(dItems))
	assert.Equal(t, true, dItems[0].Spec.IsClean)
	assert.Equal(t, true, dItems[1].Spec.IsClean)

	// second iteration
	discoverData = &mocklu.MockWrapDataDiscover{}
	testResultDrive1 = &dataDiscover.DiscoverResult{
		Message: fmt.Sprintf("Drive with path %s, SN %s, has filesystem", drive1.Path, drive1.SerialNumber),
		HasData: true,
	}
	testResultDrive2 = &dataDiscover.DiscoverResult{
		Message: fmt.Sprintf("Drive with path %s, SN %s, doesn't have filesystem, partition table, partitions and PV", drive2.Path, drive2.SerialNumber),
		HasData: false,
	}
	discoverData.On("DiscoverData", drive1.Path, drive1.SerialNumber).Return(testResultDrive1, nil)
	discoverData.On("DiscoverData", drive2.Path, drive2.SerialNumber).Return(testResultDrive2, nil)
	vm.dataDiscover = discoverData
	err = vm.Discover()
	assert.Nil(t, err)

	dItems = getDriveCRsListItems(t, vm.k8sClient)
	assert.Equal(t, 2, len(dItems))
	assert.Equal(t, false, dItems[0].Spec.IsClean)
	assert.Equal(t, true, dItems[1].Spec.IsClean)
}

func TestVolumeManager_updatesDrivesCRs_Success(t *testing.T) {
	vm := prepareSuccessVolumeManager(t)
	driveMgrRespDrives := getDriveMgrRespBasedOnDrives(drive1, drive2)
	vm.driveMgrClient = mocks.NewMockDriveMgrClient(driveMgrRespDrives)

	updates, err := vm.updateDrivesCRs(testCtx, driveMgrRespDrives)
	assert.Nil(t, err)
	driveCRs, err := vm.crHelper.GetDriveCRs(vm.nodeID)
	assert.Nil(t, err)
	assert.Equal(t, len(driveCRs), 2)
	assert.Len(t, updates.Created, 2)

	driveMgrRespDrives[0].Health = apiV1.HealthBad
	updates, err = vm.updateDrivesCRs(testCtx, driveMgrRespDrives)
	assert.Nil(t, err)
	assert.Equal(t, vm.crHelper.GetDriveCRByUUID(driveMgrRespDrives[0].UUID).Spec.Health, apiV1.HealthBad)
	assert.Len(t, updates.Updated, 1)
	assert.Len(t, updates.NotChanged, 1)

	drives := driveMgrRespDrives[1:]
	updates, err = vm.updateDrivesCRs(testCtx, drives)
	assert.Nil(t, err)
	assert.Equal(t, vm.crHelper.GetDriveCRByUUID(driveMgrRespDrives[0].UUID).Spec.Health, apiV1.HealthUnknown)
	assert.Equal(t, vm.crHelper.GetDriveCRByUUID(driveMgrRespDrives[0].UUID).Spec.Status, apiV1.DriveStatusOffline)
	assert.Len(t, updates.Updated, 1)
	assert.Len(t, updates.NotChanged, 1)

	vm = prepareSuccessVolumeManager(t)
	driveCRs, err = vm.crHelper.GetDriveCRs(vm.nodeID)
	assert.Nil(t, err)
	assert.Empty(t, driveCRs)
	updates, err = vm.updateDrivesCRs(testCtx, driveMgrRespDrives)
	assert.Nil(t, err)
	driveCRs, err = vm.crHelper.GetDriveCRs(vm.nodeID)
	assert.Nil(t, err)
	assert.Equal(t, len(driveCRs), 2)
	assert.Len(t, updates.Created, 2)
	driveMgrRespDrives = append(driveMgrRespDrives, &api.Drive{
		UUID:         uuid.New().String(),
		SerialNumber: "hdd3",
		Health:       apiV1.HealthGood,
		Type:         apiV1.DriveTypeHDD,
		Size:         1024 * 1024 * 1024 * 150,
		NodeId:       nodeID,
	})
	updates, err = vm.updateDrivesCRs(testCtx, driveMgrRespDrives)
	assert.Nil(t, err)
	driveCRs, err = vm.crHelper.GetDriveCRs(vm.nodeID)
	assert.Nil(t, err)
	assert.Equal(t, len(driveCRs), 3)
	assert.Len(t, updates.Created, 1)
	assert.Len(t, updates.NotChanged, 2)

	driveMgrRespDrives = append(driveMgrRespDrives, &api.Drive{
		UUID:         uuid.New().String(),
		SerialNumber: "",
		Health:       apiV1.HealthGood,
		Type:         apiV1.DriveTypeHDD,
		Size:         1024 * 1024 * 1024 * 150,
		NodeId:       nodeID,
	})
	updates, err = vm.updateDrivesCRs(testCtx, driveMgrRespDrives)
	assert.Nil(t, err)
	driveCRs, err = vm.crHelper.GetDriveCRs(vm.nodeID)
	assert.Nil(t, err)
	assert.Equal(t, len(driveCRs), 3)
}

func TestVolumeManager_updatesDrivesCRs_Fail(t *testing.T) {
	mockK8sClient := &mocks.K8Client{}
	kubeClient := k8s.NewKubeClient(mockK8sClient, testLogger, testNs)
	vm := NewVolumeManager(nil, nil, testLogger, kubeClient, kubeClient, new(mocks.NoOpRecorder), nodeID)

	var (
		res *driveUpdates
		err error
	)

	// GetDriveCRs failed
	mockK8sClient.On("List", mock.Anything, &drivecrd.DriveList{}, mock.Anything).Return(testErr).Once()

	res, err = vm.updateDrivesCRs(testCtx, nil)
	assert.Nil(t, res)
	assert.NotNil(t, err)
	assert.Equal(t, testErr, err)

	// CreateCR failed
	mockK8sClient.On("List", mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
	mockK8sClient.On("Create", mock.Anything, mock.Anything, mock.Anything).Return(testErr).Twice() // CreateCR will failed

	d1 := drive1
	res, err = vm.updateDrivesCRs(testCtx, []*api.Drive{&d1})
	assert.Nil(t, err)
	assert.NotNil(t, res)
	assert.Equal(t, 1, len(res.Created))
}

func TestVolumeManager_handleDriveStatusChange(t *testing.T) {
	vm := prepareSuccessVolumeManagerWithDrives(nil, t)

	ac := acCR
	err := vm.k8sClient.CreateCR(testCtx, ac.Name, &ac)
	assert.Nil(t, err)

	drive := drive1
	drive.UUID = driveUUID
	drive.Health = apiV1.HealthBad

	// Check AC deletion
	vm.handleDriveStatusChange(testCtx, &drive)
	vol := volCR
	vol.Spec.Location = driveUUID
	err = vm.k8sClient.CreateCR(testCtx, testID, &vol)
	assert.Nil(t, err)

	// Check volume's health change
	vm.handleDriveStatusChange(testCtx, &drive)
	rVolume := &vcrd.Volume{}
	err = vm.k8sClient.ReadCR(testCtx, testID, volCR.Namespace, rVolume)
	assert.Nil(t, err)
	assert.Equal(t, apiV1.HealthBad, rVolume.Spec.Health)

	lvg := testLVGCR
	lvg.Spec.Locations = []string{driveUUID}
	err = vm.k8sClient.CreateCR(testCtx, testLVGName, &lvg)
	assert.Nil(t, err)
	// Check lvg's health change
	vm.handleDriveStatusChange(testCtx, &drive)
	updatedLVG := &lvgcrd.LogicalVolumeGroup{}
	err = vm.k8sClient.ReadCR(testCtx, testLVGName, "", updatedLVG)
	assert.Nil(t, err)
	assert.Equal(t, apiV1.HealthBad, updatedLVG.Spec.Health)
}

func Test_discoverLVGOnSystemDrive_LVGAlreadyExists(t *testing.T) {
	var (
		m     = prepareSuccessVolumeManager(t)
		lvgCR = m.k8sClient.ConstructLVGCR("some-name", api.LogicalVolumeGroup{
			Name:      "some-name",
			Node:      m.nodeID,
			Locations: []string{"some-uuid"},
		})
		lvgList = lvgcrd.LogicalVolumeGroupList{}
		err     error
	)
	lvmOps := &mocklu.MockWrapLVM{}
	lvmOps.On("GetVgFreeSpace", "some-name").Return(int64(0), nil)
	m.lvmOps = lvmOps
	m.systemDrivesUUIDs = append(m.systemDrivesUUIDs, lvgCR.Spec.Locations...)

	err = m.k8sClient.CreateCR(testCtx, lvgCR.Name, lvgCR)
	assert.Nil(t, err)

	err = m.discoverLVGOnSystemDrive()
	assert.Nil(t, err)

	err = m.k8sClient.ReadList(testCtx, &lvgList)
	assert.Nil(t, err)
	assert.Equal(t, 1, len(lvgList.Items))
	assert.Equal(t, lvgCR.Spec, lvgList.Items[0].Spec)

	// increase free space on lvg
	lvmOps = &mocklu.MockWrapLVM{}
	lvmOps.On("GetVgFreeSpace", "some-name").Return(int64(2*1024*1024), nil)
	m.lvmOps = lvmOps

	err = m.k8sClient.CreateCR(testCtx, lvgCR.Name, lvgCR)
	assert.Nil(t, err)

	err = m.discoverLVGOnSystemDrive()
	assert.Nil(t, err)

	err = m.k8sClient.ReadList(testCtx, &lvgList)
	assert.Nil(t, err)
	assert.Equal(t, 1, len(lvgList.Items))
	assert.Equal(t, lvgCR.Spec, lvgList.Items[0].Spec)

}

func Test_discoverLVGOnSystemDrive_LVGCreatedACNo(t *testing.T) {
	var (
		m             = prepareSuccessVolumeManager(t)
		lvgList       = lvgcrd.LogicalVolumeGroupList{}
		listBlk       = &mocklu.MockWrapLsblk{}
		fsOps         = &mockProv.MockFsOpts{}
		lvmOps        = &mocklu.MockWrapLVM{}
		vgName        = "root-vg"
		systemDriveCR = testDriveCR
		err           error
	)

	m.listBlk = listBlk
	m.fsOps = fsOps
	m.lvmOps = lvmOps

	pvName := testDriveCR.Spec.Path + "1"
	lvmOps.On("GetAllPVs").Return([]string{pvName, "/dev/sdx"}, nil)
	lvmOps.On("GetVGNameByPVName", pvName).Return(vgName, nil)
	lvmOps.On("GetVgFreeSpace", vgName).Return(int64(1024), nil)
	lvmOps.On("GetLVsInVG", vgName).Return([]string{"lv_swap", "lv_boot"}, nil).Once()

	assert.Nil(t, m.k8sClient.CreateCR(testCtx, systemDriveCR.Name, &systemDriveCR))

	// expect success, LogicalVolumeGroup CR and AC CR was created
	m.systemDrivesUUIDs = append(m.systemDrivesUUIDs, systemDriveCR.Spec.UUID)
	err = m.discoverLVGOnSystemDrive()
	assert.Nil(t, err)

	err = m.k8sClient.ReadList(testCtx, &lvgList)
	assert.Nil(t, err)
	assert.Equal(t, 1, len(lvgList.Items))
	lvg := lvgList.Items[0]
	assert.Equal(t, 1, len(lvg.Spec.Locations))
	assert.Equal(t, testDriveCR.Spec.UUID, lvg.Spec.Locations[0])
	assert.Equal(t, apiV1.Created, lvg.Spec.Status)
	assert.Equal(t, 2, len(lvg.Spec.VolumeRefs))

	// unable to read LVs in system vg
	m = prepareSuccessVolumeManager(t)
	// mocks were setup for previous scenario
	m.listBlk = listBlk
	m.fsOps = fsOps
	m.lvmOps = lvmOps

	lvmOps.On("GetLVsInVG", vgName).Return(nil, testErr)

	assert.Nil(t, m.k8sClient.CreateCR(testCtx, systemDriveCR.Name, &systemDriveCR))
	m.systemDrivesUUIDs = append(m.systemDrivesUUIDs, systemDriveCR.Spec.UUID)

	err = m.discoverLVGOnSystemDrive()
	assert.NotNil(t, err)
	assert.Contains(t, err.Error(), "unable to determine LVs in system VG")

	assert.Nil(t, m.k8sClient.ReadList(testCtx, &lvgList))
	assert.Equal(t, 0, len(lvgList.Items))
}

func TestVolumeManager_createEventsForDriveUpdates(t *testing.T) {
	k, err := k8s.GetFakeKubeClient(testNs, testLogger)
	assert.Nil(t, err)

	drive1CR := k.ConstructDriveCR(drive1UUID, *getTestDrive(drive1UUID, "SN1"))
	drive2CR := k.ConstructDriveCR(drive2UUID, *getTestDrive(drive1UUID, "SN2"))

	var (
		rec *mocks.NoOpRecorder
		mgr *VolumeManager
	)

	init := func() {
		rec = &mocks.NoOpRecorder{}
		mgr = &VolumeManager{recorder: rec}
	}

	expectEvent := func(drive *drivecrd.Drive, eventtype, reason string) bool {
		for _, c := range rec.Calls {
			driveObj, ok := c.Object.(*drivecrd.Drive)
			if !ok {
				continue
			}
			if driveObj.Name != drive.Name {
				continue
			}
			if c.Eventtype == eventtype && c.Reason == reason {
				return true
			}
		}
		return false
	}

	t.Run("Healthy drives discovered", func(t *testing.T) {
		init()
		upd := &driveUpdates{
			Created: []*drivecrd.Drive{drive1CR, drive2CR},
		}
		mgr.createEventsForDriveUpdates(upd)
		assert.NotEmpty(t, rec.Calls)
		msgDiscovered := "DriveDiscovered event should exist for drive"
		msgHealth := "DriveHealthGood event should exist for drive"
		assert.True(t, expectEvent(drive1CR, eventing.NormalType, eventing.DriveDiscovered), msgDiscovered)
		assert.True(t, expectEvent(drive2CR, eventing.NormalType, eventing.DriveDiscovered), msgDiscovered)
		assert.True(t, expectEvent(drive1CR, eventing.NormalType, eventing.DriveHealthGood), msgHealth)
		assert.True(t, expectEvent(drive2CR, eventing.NormalType, eventing.DriveHealthGood), msgHealth)
	})

	t.Run("No changes", func(t *testing.T) {
		init()
		upd := &driveUpdates{
			NotChanged: []*drivecrd.Drive{drive1CR, drive2CR},
		}
		mgr.createEventsForDriveUpdates(upd)
		assert.Empty(t, rec.Calls)
	})

	t.Run("Drive status and health changed", func(t *testing.T) {
		init()
		modifiedDrive := drive1CR.DeepCopy()
		modifiedDrive.Spec.Status = apiV1.DriveStatusOffline
		modifiedDrive.Spec.Health = apiV1.HealthUnknown

		upd := &driveUpdates{
			Updated: []updatedDrive{{
				PreviousState: drive1CR,
				CurrentState:  modifiedDrive}},
		}
		mgr.createEventsForDriveUpdates(upd)
		assert.True(t, expectEvent(drive1CR, eventing.ErrorType, eventing.DriveStatusOffline))
		assert.True(t, expectEvent(drive1CR, eventing.WarningType, eventing.DriveHealthUnknown))
	})
}

func TestVolumeManager_isShouldBeReconciled(t *testing.T) {
	var (
		vm  *VolumeManager
		vol vcrd.Volume
	)

	vm = prepareSuccessVolumeManager(t)
	vol = testVolumeCR1
	vol.Spec.NodeId = vm.nodeID
	assert.True(t, vm.isCorrespondedToNodePredicate(&vol))

	vol.Spec.NodeId = ""
	assert.False(t, vm.isCorrespondedToNodePredicate(&vol))

}

func TestVolumeManager_isDriveIsInLVG(t *testing.T) {
	vm := prepareSuccessVolumeManager(t)
	drive1 := api.Drive{UUID: drive1UUID}
	drive2 := api.Drive{UUID: drive2UUID}
	lvgCR := lvgcrd.LogicalVolumeGroup{
		TypeMeta: v1.TypeMeta{
			Kind:       "LogicalVolumeGroup",
			APIVersion: apiV1.APIV1Version,
		},
		ObjectMeta: v1.ObjectMeta{
			Name:      testLVGName,
			Namespace: testNs,
		},
		Spec: api.LogicalVolumeGroup{
			Name:       testLVGName,
			Node:       nodeID,
			Locations:  []string{drive1.UUID},
			Size:       int64(1024 * 500 * util.GBYTE),
			Status:     apiV1.Created,
			VolumeRefs: []string{},
		},
	}
	// there are no LogicalVolumeGroup CRs
	assert.False(t, vm.isDriveInLVG(drive1))
	// create LogicalVolumeGroup CR
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, lvgCR.Name, &lvgCR))

	assert.True(t, vm.isDriveInLVG(drive1))
	assert.False(t, vm.isDriveInLVG(drive2))
}

func TestVolumeManager_handleExpandingStatus(t *testing.T) {
	var (
		vm      *VolumeManager
		pMock   *mockProv.MockProvisioner
		vol     *vcrd.Volume
		testVol vcrd.Volume
		res     ctrl.Result
		err     error
	)

	vm = prepareSuccessVolumeManager(t)
	pMock = &mockProv.MockProvisioner{}
	pMock.On("GetVolumePath", testVol.Spec).Return("path", testErr)
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.LVMBasedVolumeType: pMock})
	res, err = vm.handleExpandingStatus(testCtx, &testVol)
	assert.NotNil(t, err)

	testVol = testVolumeLVGCR
	assert.Nil(t, vm.k8sClient.CreateCR(testCtx, testVol.Name, &testVol))

	pMock = &mockProv.MockProvisioner{}
	pMock.On("GetVolumePath", testVol.Spec).Return("path", nil)
	vm.SetProvisioners(map[p.VolumeType]p.Provisioner{p.LVMBasedVolumeType: pMock})
	lvmOps := &mocklu.MockWrapLVM{}
	lvmOps.On("ExpandLV", "path", testVol.Spec.Size).Return(fmt.Errorf("error"))
	vm.lvmOps = lvmOps
	res, err = vm.handleExpandingStatus(testCtx, &testVol)
	assert.NotNil(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	vol = &vcrd.Volume{}
	assert.Nil(t, vm.k8sClient.ReadCR(testCtx, testVol.Name, testVol.Namespace, vol))
	assert.Equal(t, apiV1.Failed, vol.Spec.CSIStatus)

	pMock.On("GetVolumePath", vol.Spec).Return("path", nil)

	lvmOps = &mocklu.MockWrapLVM{}
	lvmOps.On("ExpandLV", "path", vol.Spec.Size).Return(nil)
	vm.lvmOps = lvmOps
	assert.Nil(t, vm.k8sClient.UpdateCR(testCtx, &testVol))
	res, err = vm.handleExpandingStatus(testCtx, &testVol)
	assert.Nil(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	vol = &vcrd.Volume{}
	assert.Nil(t, vm.k8sClient.ReadCR(testCtx, testVol.Name, testVol.Namespace, vol))
	assert.Equal(t, apiV1.Resized, vol.Spec.CSIStatus)
}

func TestVolumeManager_discoverDataOnDrives(t *testing.T) {
	t.Run("Disk has data", func(t *testing.T) {
		var vm *VolumeManager
		vm = prepareSuccessVolumeManager(t)
		testDrive := testDriveCR
		testDrive.Spec.Path = "/dev/sda"
		testDrive.Spec.IsClean = true
		discoverData := &mocklu.MockWrapDataDiscover{}
		testResult := &dataDiscover.DiscoverResult{
			Message: fmt.Sprintf("Drive with path %s, SN %s, has filesystem", testDrive.Spec.Path, testDrive.Spec.SerialNumber),
			HasData: true,
		}
		discoverData.On("DiscoverData", testDrive.Spec.Path, testDrive.Spec.SerialNumber).Return(testResult, nil).Once()
		vm.dataDiscover = discoverData

		err := vm.k8sClient.CreateCR(testCtx, testDrive.Name, &testDrive)
		assert.Nil(t, err)

		err = vm.discoverDataOnDrives()
		assert.Nil(t, err)

		newDrive := &drivecrd.Drive{}
		err = vm.k8sClient.ReadCR(testCtx, testDrive.Name, "", newDrive)
		assert.Nil(t, err)

		assert.Equal(t, false, newDrive.Spec.IsClean)
	})
	t.Run("Disk has data and field IsClean is false", func(t *testing.T) {
		var vm *VolumeManager
		vm = prepareSuccessVolumeManager(t)

		testDrive := testDriveCR
		testDrive.Spec.Path = "/dev/sda"
		testDrive.Spec.IsClean = false

		discoverData := &mocklu.MockWrapDataDiscover{}
		testResult := &dataDiscover.DiscoverResult{
			Message: fmt.Sprintf("Drive with path %s, SN %s, has filesystem", testDrive.Spec.Path, testDrive.Spec.SerialNumber),
			HasData: true,
		}
		discoverData.On("DiscoverData", testDrive.Spec.Path, testDrive.Spec.SerialNumber).Return(testResult, nil).Once()
		vm.dataDiscover = discoverData

		err := vm.k8sClient.CreateCR(testCtx, testDrive.Name, &testDrive)
		assert.Nil(t, err)

		err = vm.discoverDataOnDrives()
		assert.Nil(t, err)

		newDrive := &drivecrd.Drive{}
		err = vm.k8sClient.ReadCR(testCtx, testDrive.Name, "", newDrive)
		assert.Nil(t, err)

		assert.Equal(t, false, newDrive.Spec.IsClean)
	})
	t.Run("DiscoverData function failed", func(t *testing.T) {
		var vm *VolumeManager
		vm = prepareSuccessVolumeManager(t)
		testDrive := testDriveCR
		testDrive.Spec.Path = "/dev/sda"
		discoverData := &mocklu.MockWrapDataDiscover{}

		discoverData.On("DiscoverData", testDrive.Spec.Path, testDrive.Spec.SerialNumber).
			Return(&dataDiscover.DiscoverResult{}, testErr).Once()
		vm.dataDiscover = discoverData

		err := vm.k8sClient.CreateCR(testCtx, testDrive.Name, &testDrive)
		assert.Nil(t, err)

		err = vm.discoverDataOnDrives()
		assert.Nil(t, err)
		newDrive := &drivecrd.Drive{}
		err = vm.k8sClient.ReadCR(testCtx, testDriveCR.Name, "", newDrive)
		assert.Nil(t, err)
		assert.Equal(t, false, newDrive.Spec.IsClean)
	})

	t.Run("Drive doesn't have data", func(t *testing.T) {
		var vm *VolumeManager
		vm = prepareSuccessVolumeManager(t)
		testDrive := testDriveCR
		testDrive.Spec.Path = "/dev/sda"

		discoverData := &mocklu.MockWrapDataDiscover{}
		vm.dataDiscover = discoverData
		testResult := &dataDiscover.DiscoverResult{
			Message: fmt.Sprintf("Drive with path %s, SN %s doesn't have filesystem, partition table, partitions and PV", testDrive.Spec.Path, testDrive.Spec.SerialNumber),
			HasData: false,
		}
		discoverData.On("DiscoverData", testDrive.Spec.Path, testDrive.Spec.SerialNumber).Return(testResult, nil).Once()
		err := vm.k8sClient.CreateCR(testCtx, testDrive.Name, &testDrive)
		assert.Nil(t, err)

		err = vm.discoverDataOnDrives()
		assert.Nil(t, err)
		newDrive := &drivecrd.Drive{}
		err = vm.k8sClient.ReadCR(testCtx, testDriveCR.Name, "", newDrive)
		assert.Nil(t, err)
		assert.Equal(t, true, newDrive.Spec.IsClean)
	})
}

func prepareSuccessVolumeManager(t *testing.T) *VolumeManager {
	c := mocks.NewMockDriveMgrClient(nil)
	// create map of commands which must be mocked
	cmds := make(map[string]mocks.CmdOut)
	// list of all devices
	cmds[lsblkAllDevicesCmd] = mocks.CmdOut{Stdout: mocks.LsblkTwoDevicesStr}
	// list partitions of specific device
	cmds[lsblkSingleDeviceCmd] = mocks.CmdOut{Stdout: mocks.LsblkListPartitionsStr}
	e := mocks.NewMockExecutor(cmds)
	e.SetSuccessIfNotFound(true)

	kubeClient, err := k8s.GetFakeKubeClient(testNs, testLogger)
	assert.Nil(t, err)
	vm := NewVolumeManager(c, e, testLogger, kubeClient, kubeClient, new(mocks.NoOpRecorder), nodeID)
	vm.discoverSystemLVG = false
	return vm
}

func prepareSuccessVolumeManagerWithDrives(drives []*api.Drive, t *testing.T) *VolumeManager {
	nVM := prepareSuccessVolumeManager(t)
	nVM.driveMgrClient = mocks.NewMockDriveMgrClient(drives)
	for _, d := range drives {
		dCR := nVM.k8sClient.ConstructDriveCR(d.UUID, *d)
		if err := nVM.k8sClient.CreateCR(testCtx, dCR.Name, dCR); err != nil {
			panic(err)
		}
	}
	return nVM
}

func addDriveCRs(k *k8s.KubeClient, drives ...*drivecrd.Drive) {
	var err error
	for _, d := range drives {
		if err = k.CreateCR(testCtx, d.Name, d); err != nil {
			panic(err)
		}
	}
}

func getDriveMgrRespBasedOnDrives(drives ...api.Drive) []*api.Drive {
	resp := make([]*api.Drive, len(drives))
	for i, d := range drives {
		dd := d
		resp[i] = &dd
	}
	return resp
}

func TestVolumeManager_isDriveSystem(t *testing.T) {
	driveMgrRespDrives := getDriveMgrRespBasedOnDrives(drive1, drive2)
	hwMgrClient := mocks.NewMockDriveMgrClient(driveMgrRespDrives)
	kubeClient, err := k8s.GetFakeKubeClient(testNs, testLogger)
	assert.Nil(t, err)
	listBlk := &mocklu.MockWrapLsblk{}
	vm := NewVolumeManager(hwMgrClient, nil, testLogger, kubeClient, kubeClient, new(mocks.NoOpRecorder), nodeID)
	listBlk.On("GetBlockDevices", drive2.Path).Return([]lsblk.BlockDevice{bdev1}, nil).Once()
	vm.listBlk = listBlk
	isSystem, err := vm.isDriveSystem("/dev/sdb")
	assert.Nil(t, err)
	assert.Equal(t, false, isSystem)

	bdev1.MountPoint = base.KubeletRootPath
	listBlk.On("GetBlockDevices", drive2.Path).Return([]lsblk.BlockDevice{bdev1}, nil).Once()
	vm.listBlk = listBlk
	isSystem, err = vm.isDriveSystem("/dev/sdb")
	assert.Nil(t, err)
	assert.Equal(t, isSystem, true)

	bdev1.MountPoint = base.HostRootPath
	listBlk.On("GetBlockDevices", drive2.Path).Return([]lsblk.BlockDevice{bdev1}, nil).Once()
	vm.listBlk = listBlk
	isSystem, err = vm.isDriveSystem("/dev/sdb")
	assert.Nil(t, err)
	assert.Equal(t, isSystem, true)

	listBlk.On("GetBlockDevices", drive2.Path).Return([]lsblk.BlockDevice{bdev1}, testErr).Once()
	vm.listBlk = listBlk
	isSystem, err = vm.isDriveSystem("/dev/sdb")
	assert.NotNil(t, err)
	assert.Equal(t, isSystem, false)
}
