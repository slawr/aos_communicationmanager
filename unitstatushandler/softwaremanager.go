// SPDX-License-Identifier: Apache-2.0
//
// Copyright (C) 2021 Renesas Electronics Corporation.
// Copyright (C) 2021 EPAM Systems, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package unitstatushandler

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/aoscloud/aos_common/aoserrors"
	"github.com/aoscloud/aos_common/utils/action"
	"github.com/looplab/fsm"
	log "github.com/sirupsen/logrus"

	"aos_communicationmanager/cloudprotocol"
	"aos_communicationmanager/cmserver"
)

/***********************************************************************************************************************
 * Consts
 **********************************************************************************************************************/

const maxConcurrentActions = 10

/***********************************************************************************************************************
 * Types
 **********************************************************************************************************************/

type softwareStatusHandler interface {
	download(ctx context.Context, request map[string]cloudprotocol.DecryptDataStruct,
		continueOnError bool, notifier statusNotifier,
		chains []cloudprotocol.CertificateChain, certs []cloudprotocol.Certificate) (result map[string]*downloadResult)
	updateLayerStatus(layerInfo cloudprotocol.LayerInfo)
	updateServiceStatus(serviceInfo cloudprotocol.ServiceInfo)
}

type softwareUpdate struct {
	Schedule         cloudprotocol.ScheduleRule           `json:"schedule,omitempty"`
	DownloadServices []cloudprotocol.ServiceInfoFromCloud `json:"downloadServices,omitempty"`
	InstallServices  []cloudprotocol.ServiceInfoFromCloud `json:"installServices,omitempty"`
	RemoveServices   []cloudprotocol.ServiceInfo          `json:"removeServices,omitempty"`
	DownloadLayers   []cloudprotocol.LayerInfoFromCloud   `json:"downloadLayers,omitempty"`
	InstallLayers    []cloudprotocol.LayerInfoFromCloud   `json:"installLayers,omitempty"`
	RemoveLayers     []cloudprotocol.LayerInfo            `json:"removeLayers,omitempty"`
	CertChains       []cloudprotocol.CertificateChain     `json:"certChains,omitempty"`
	Certs            []cloudprotocol.Certificate          `json:"certs,omitempty"`
}

type softwareManager struct {
	sync.Mutex

	statusChannel chan cmserver.UpdateSOTAStatus

	statusHandler   softwareStatusHandler
	softwareUpdater SoftwareUpdater
	storage         Storage

	stateMachine  *updateStateMachine
	actionHandler *action.Handler
	statusMutex   sync.RWMutex
	pendingUpdate *softwareUpdate
	currentUsers  []string

	LayerStatuses   map[string]*cloudprotocol.LayerInfo   `json:"layerStatuses,omitempty"`
	ServiceStatuses map[string]*cloudprotocol.ServiceInfo `json:"serviceStatuses,omitempty"`
	CurrentUpdate   *softwareUpdate                       `json:"currentUpdate,omitempty"`
	DownloadResult  map[string]*downloadResult            `json:"downloadResult,omitempty"`
	CurrentState    string                                `json:"currentState,omitempty"`
	UpdateErr       string                                `json:"updateErr,omitempty"`
	TTLDate         time.Time                             `json:"ttlDate,omitempty"`
}

/***********************************************************************************************************************
 * Interface
 **********************************************************************************************************************/

func newSoftwareManager(statusHandler softwareStatusHandler,
	softwareUpdater SoftwareUpdater, storage Storage, defaultTTL time.Duration) (manager *softwareManager, err error) {
	manager = &softwareManager{
		statusChannel:   make(chan cmserver.UpdateSOTAStatus, 1),
		statusHandler:   statusHandler,
		softwareUpdater: softwareUpdater,
		actionHandler:   action.New(maxConcurrentActions),
		storage:         storage,
		CurrentState:    stateNoUpdate,
	}

	if err = manager.loadState(); err != nil {
		return nil, aoserrors.Wrap(err)
	}

	log.WithFields(log.Fields{"state": manager.CurrentState, "error": manager.UpdateErr}).Debug("New software manager")

	manager.stateMachine = newUpdateStateMachine(manager.CurrentState, fsm.Events{
		// no update state
		{Name: eventStartDownload, Src: []string{stateNoUpdate}, Dst: stateDownloading},
		// downloading state
		{Name: eventFinishDownload, Src: []string{stateDownloading}, Dst: stateReadyToUpdate},
		{Name: eventCancel, Src: []string{stateDownloading}, Dst: stateNoUpdate},
		// ready to update state
		{Name: eventCancel, Src: []string{stateReadyToUpdate}, Dst: stateNoUpdate},
		{Name: eventStartUpdate, Src: []string{stateReadyToUpdate}, Dst: stateUpdating},
		// updating state
		{Name: eventFinishUpdate, Src: []string{stateUpdating}, Dst: stateNoUpdate},
		{Name: eventCancel, Src: []string{stateUpdating}, Dst: stateNoUpdate},
	}, manager, defaultTTL)

	if err = manager.stateMachine.init(manager.TTLDate); err != nil {
		return nil, aoserrors.Wrap(err)
	}

	return manager, nil
}

func (manager *softwareManager) close() (err error) {
	manager.Lock()
	defer manager.Unlock()

	log.Debug("Close software manager")

	close(manager.statusChannel)

	if err = manager.stateMachine.close(); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func (manager *softwareManager) getCurrentStatus() (status cmserver.UpdateSOTAStatus) {
	status.State = convertState(manager.CurrentState)
	status.Error = manager.UpdateErr

	if status.State == cmserver.NoUpdate || manager.CurrentUpdate == nil {
		return status
	}

	for _, layer := range manager.CurrentUpdate.DownloadLayers {
		status.InstallLayers = append(status.InstallLayers, cloudprotocol.LayerInfo{
			ID: layer.ID, Digest: layer.Digest, AosVersion: layer.AosVersion})
	}

	for _, layer := range manager.CurrentUpdate.InstallLayers {
		status.InstallLayers = append(status.InstallLayers, cloudprotocol.LayerInfo{
			ID: layer.ID, Digest: layer.Digest, AosVersion: layer.AosVersion})
	}

	for _, layer := range manager.CurrentUpdate.RemoveLayers {
		status.RemoveLayers = append(status.InstallLayers, cloudprotocol.LayerInfo{
			ID: layer.ID, Digest: layer.Digest, AosVersion: layer.AosVersion})
	}

	for _, service := range manager.CurrentUpdate.DownloadServices {
		status.InstallServices = append(status.InstallServices, cloudprotocol.ServiceInfo{
			ID: service.ID, AosVersion: service.AosVersion})
	}

	for _, service := range manager.CurrentUpdate.InstallServices {
		status.InstallServices = append(status.InstallServices, cloudprotocol.ServiceInfo{
			ID: service.ID, AosVersion: service.AosVersion})
	}

	for _, service := range manager.CurrentUpdate.RemoveServices {
		status.RemoveServices = append(status.RemoveServices, cloudprotocol.ServiceInfo{
			ID: service.ID, AosVersion: service.AosVersion})
	}

	return status
}

func (manager *softwareManager) getCurrentUpdateState() (status cmserver.UpdateState) {
	manager.Lock()
	defer manager.Unlock()

	return convertState(manager.CurrentState)
}

func (manager *softwareManager) processDesiredStatus(desiredStatus cloudprotocol.DecodedDesiredStatus) (err error) {
	manager.Lock()
	defer manager.Unlock()

	update := &softwareUpdate{
		Schedule:         desiredStatus.SOTASchedule,
		DownloadServices: make([]cloudprotocol.ServiceInfoFromCloud, 0),
		InstallServices:  make([]cloudprotocol.ServiceInfoFromCloud, 0),
		RemoveServices:   make([]cloudprotocol.ServiceInfo, 0),
		DownloadLayers:   make([]cloudprotocol.LayerInfoFromCloud, 0),
		InstallLayers:    make([]cloudprotocol.LayerInfoFromCloud, 0),
		RemoveLayers:     make([]cloudprotocol.LayerInfo, 0),
		CertChains:       desiredStatus.CertificateChains,
		Certs:            desiredStatus.Certificates,
	}

	usersServices, usersLayers, err := manager.softwareUpdater.GetUsersStatus(manager.currentUsers)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	allServices, allLayers, err := manager.softwareUpdater.GetAllStatus()
	if err != nil {
		return aoserrors.Wrap(err)
	}

desiredServicesLoop:
	for _, desiredService := range desiredStatus.Services {
		for _, usersService := range usersServices {
			if desiredService.ID == usersService.ID && desiredService.AosVersion == usersService.AosVersion &&
				usersService.Status == cloudprotocol.InstalledStatus {
				continue desiredServicesLoop
			}
		}

		for _, service := range allServices {
			if desiredService.ID == service.ID && desiredService.AosVersion == service.AosVersion &&
				service.Status == cloudprotocol.InstalledStatus {
				update.InstallServices = append(update.InstallServices, desiredService)
				continue desiredServicesLoop
			}
		}

		update.DownloadServices = append(update.DownloadServices, desiredService)
	}

usersServicesLoop:
	for _, usersService := range usersServices {
		if usersService.Status != cloudprotocol.InstalledStatus {
			continue
		}

		for _, desiredService := range desiredStatus.Services {
			if usersService.ID == desiredService.ID {
				continue usersServicesLoop
			}
		}

		update.RemoveServices = append(update.RemoveServices, usersService)
	}

desiredLayersLoop:
	for _, desiredLayer := range desiredStatus.Layers {
		for _, usersLayer := range usersLayers {
			if desiredLayer.Digest == usersLayer.Digest && usersLayer.Status == cloudprotocol.InstalledStatus {
				continue desiredLayersLoop
			}
		}

		for _, layer := range allLayers {
			if desiredLayer.Digest == layer.Digest && layer.Status == cloudprotocol.InstalledStatus {
				update.InstallLayers = append(update.InstallLayers, desiredLayer)
				continue desiredLayersLoop
			}
		}

		update.DownloadLayers = append(update.DownloadLayers, desiredLayer)
	}

usersLayersLoop:
	for _, installedLayer := range usersLayers {
		if installedLayer.Status != cloudprotocol.InstalledStatus {
			continue
		}

		for _, desiredLayer := range desiredStatus.Layers {
			if installedLayer.Digest == desiredLayer.Digest {
				continue usersLayersLoop
			}
		}

		update.RemoveLayers = append(update.RemoveLayers, installedLayer)
	}

	if len(update.DownloadServices) != 0 || len(update.InstallServices) != 0 || len(update.RemoveServices) != 0 ||
		len(update.DownloadLayers) != 0 || len(update.InstallLayers) != 0 || len(update.RemoveLayers) != 0 {
		if err := manager.newUpdate(update); err != nil {
			return aoserrors.Wrap(err)
		}
	}

	return nil
}

func (manager *softwareManager) startUpdate() (err error) {
	manager.Lock()
	defer manager.Unlock()

	log.Debug("Start software update")

	if err = manager.stateMachine.sendEvent(eventStartUpdate, ""); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func (manager *softwareManager) getItemStatuses() (serviceStatuses []cloudprotocol.ServiceInfo,
	layerStatuses []cloudprotocol.LayerInfo, err error) {
	manager.Lock()
	defer manager.Unlock()

	manager.statusMutex.RLock()
	defer manager.statusMutex.RUnlock()

	serviceInfo, layerInfo, err := manager.softwareUpdater.GetUsersStatus(manager.currentUsers)
	if err != nil {
		return nil, nil, aoserrors.Wrap(err)
	}

	// Get installed info

	for _, service := range serviceInfo {
		if service.Status == cloudprotocol.InstalledStatus {
			serviceStatuses = append(serviceStatuses, service)
		}
	}

	for _, layer := range layerInfo {
		if layer.Status == cloudprotocol.InstalledStatus {
			layerStatuses = append(layerStatuses, layer)
		}
	}

	// Append currently processing info

	if manager.CurrentState == stateNoUpdate {
		return serviceStatuses, layerStatuses, nil
	}

	for _, service := range manager.ServiceStatuses {
		serviceStatuses = append(serviceStatuses, *service)
	}

	for _, layer := range manager.LayerStatuses {
		layerStatuses = append(layerStatuses, *layer)
	}

	return serviceStatuses, layerStatuses, nil
}

func (manager *softwareManager) setUsers(users []string) (err error) {
	manager.Lock()
	defer manager.Unlock()

	if isUsersEqual(manager.currentUsers, users) {
		return nil
	}

	if manager.stateMachine.canTransit(eventCancel) {
		if err = manager.stateMachine.sendEvent(eventCancel, ""); err != nil {
			return aoserrors.Wrap(err)
		}
	}

	manager.currentUsers = users

	return nil
}

/***********************************************************************************************************************
 * Implementer
 **********************************************************************************************************************/

func (manager *softwareManager) stateChanged(event, state string, updateErr string) {
	if event == eventCancel {
		for id, status := range manager.LayerStatuses {
			if status.Status != cloudprotocol.ErrorStatus {
				manager.updateLayerStatusByID(id, cloudprotocol.ErrorStatus, updateErr)
			}
		}

		for id, status := range manager.ServiceStatuses {
			if status.Status != cloudprotocol.ErrorStatus {
				manager.updateServiceStatusByID(id, cloudprotocol.ErrorStatus, updateErr, "")
			}
		}
	}

	manager.CurrentState = state
	manager.UpdateErr = updateErr

	log.WithFields(log.Fields{
		"state": state,
		"event": event}).Debug("Software manager state changed")

	if updateErr != "" {
		log.Errorf("Software update error: %s", updateErr)
	}

	manager.sendCurrentStatus()

	if err := manager.saveState(); err != nil {
		log.Errorf("Can't save current software manager state: %s", err)
	}
}

func (manager *softwareManager) noUpdate() {
	// Remove downloaded files
	for _, result := range manager.DownloadResult {
		if result.FileName != "" {
			log.WithField("file", result.FileName).Debug("Remove software update file")

			if err := os.RemoveAll(result.FileName); err != nil {
				log.WithField("file", result.FileName).Errorf("Can't remove update file: %s", err)
			}
		}
	}

	if manager.pendingUpdate != nil {
		log.Debug("Schedule pending software update")

		manager.CurrentUpdate = manager.pendingUpdate
		manager.pendingUpdate = nil

		go func() {
			manager.Lock()
			defer manager.Unlock()

			var err error

			if manager.TTLDate, err = manager.stateMachine.startNewUpdate(
				time.Duration(manager.CurrentUpdate.Schedule.TTL) * time.Second); err != nil {
				log.Errorf("Can't start new software update: %s", err)
			}
		}()
	}
}

func (manager *softwareManager) download(ctx context.Context) {
	var (
		downloadErr string
		finishEvent = eventFinishDownload
	)

	defer func() {
		go func() {
			manager.Lock()
			defer manager.Unlock()

			manager.stateMachine.finishOperation(ctx, finishEvent, downloadErr)
		}()
	}()

	manager.DownloadResult = nil

	manager.statusMutex.Lock()

	manager.LayerStatuses = make(map[string]*cloudprotocol.LayerInfo)
	manager.ServiceStatuses = make(map[string]*cloudprotocol.ServiceInfo)

	request := make(map[string]cloudprotocol.DecryptDataStruct)

	for _, service := range manager.CurrentUpdate.DownloadServices {
		log.WithFields(log.Fields{
			"id":      service.ID,
			"version": service.AosVersion,
		}).Debug("Download service")

		request[service.ID] = service.DecryptDataStruct
		manager.ServiceStatuses[service.ID] = &cloudprotocol.ServiceInfo{
			ID:         service.ID,
			AosVersion: service.AosVersion,
			Status:     cloudprotocol.DownloadingStatus,
		}
	}

	for _, layer := range manager.CurrentUpdate.DownloadLayers {
		log.WithFields(log.Fields{
			"id":      layer.ID,
			"digest":  layer.Digest,
			"version": layer.AosVersion,
		}).Debug("Download layer")

		request[layer.Digest] = layer.DecryptDataStruct
		manager.LayerStatuses[layer.Digest] = &cloudprotocol.LayerInfo{
			ID:         layer.ID,
			AosVersion: layer.AosVersion,
			Digest:     layer.Digest,
			Status:     cloudprotocol.DownloadingStatus,
		}
	}

	manager.statusMutex.Unlock()

	// Set pending status for install services and layers

	for _, service := range manager.CurrentUpdate.InstallServices {
		manager.ServiceStatuses[service.ID] = &cloudprotocol.ServiceInfo{
			ID:         service.ID,
			AosVersion: service.AosVersion,
			Status:     cloudprotocol.PendingStatus,
		}

		manager.updateServiceStatusByID(service.ID, cloudprotocol.PendingStatus, "", "")
	}

	for _, layer := range manager.CurrentUpdate.InstallLayers {
		manager.LayerStatuses[layer.Digest] = &cloudprotocol.LayerInfo{
			ID:         layer.ID,
			AosVersion: layer.AosVersion,
			Digest:     layer.Digest,
			Status:     cloudprotocol.PendingStatus,
		}

		manager.updateLayerStatusByID(layer.Digest, cloudprotocol.PendingStatus, "")
	}

	// Nothing to download
	if len(request) == 0 {
		return
	}

	manager.DownloadResult = manager.statusHandler.download(ctx, request, true, manager.updateStatusByID,
		manager.CurrentUpdate.CertChains, manager.CurrentUpdate.Certs)

	// Set pending state

	for id := range manager.DownloadResult {
		if layerStatus, ok := manager.LayerStatuses[id]; ok {
			if layerStatus.Status == cloudprotocol.ErrorStatus {
				log.WithFields(log.Fields{
					"id":      layerStatus.ID,
					"digest":  layerStatus.Digest,
					"version": layerStatus.AosVersion,
				}).Errorf("Error downloading layer: %s", layerStatus.Error)
				continue
			}

			log.WithFields(log.Fields{
				"id":      layerStatus.ID,
				"digest":  layerStatus.Digest,
				"version": layerStatus.AosVersion,
			}).Debug("Layer successfully downloaded")

			manager.updateLayerStatusByID(id, cloudprotocol.PendingStatus, "")
		} else if serviceStatus, ok := manager.ServiceStatuses[id]; ok {
			if serviceStatus.Status == cloudprotocol.ErrorStatus {
				log.WithFields(log.Fields{
					"id":      serviceStatus.ID,
					"version": serviceStatus.AosVersion,
				}).Errorf("Error downloading service: %s", serviceStatus.Error)
				continue
			}

			log.WithFields(log.Fields{
				"id":      serviceStatus.ID,
				"version": serviceStatus.AosVersion,
			}).Debug("Service successfully downloaded")

			manager.updateServiceStatusByID(id, cloudprotocol.PendingStatus, "", "")
		}
	}

	downloadErr = getDownloadError(manager.DownloadResult)

	numDownloadErrors := 0

	for _, item := range manager.DownloadResult {
		if item.Error != "" {
			numDownloadErrors++
		}
	}

	// All downloads failed and there is nothing to update (not counting remove layers) then cancel
	if numDownloadErrors == len(manager.DownloadResult) && len(manager.CurrentUpdate.RemoveServices) == 0 {
		finishEvent = eventCancel
	}
}

func (manager *softwareManager) readyToUpdate() {
	manager.stateMachine.scheduleUpdate(manager.CurrentUpdate.Schedule)
}

func (manager *softwareManager) update(ctx context.Context) {
	var updateErr string

	defer func() {
		go func() {
			manager.Lock()
			defer manager.Unlock()

			manager.stateMachine.finishOperation(ctx, eventFinishUpdate, updateErr)
		}()
	}()

	if errorStr := manager.installLayers(); errorStr != "" {
		if updateErr == "" {
			updateErr = errorStr
		}
	}

	if errorStr := manager.installServices(); errorStr != "" {
		if updateErr == "" {
			updateErr = errorStr
		}
	}

	if errorStr := manager.removeServices(); errorStr != "" {
		if updateErr == "" {
			updateErr = errorStr
		}
	}

	if errorStr := manager.removeLayers(); errorStr != "" {
		if updateErr == "" {
			updateErr = errorStr
		}
	}
}

/***********************************************************************************************************************
 * Private
 **********************************************************************************************************************/

func (manager *softwareManager) newUpdate(update *softwareUpdate) (err error) {
	log.Debug("New software update")

	// Set default schedule type
	switch update.Schedule.Type {
	case "":
		update.Schedule.Type = cloudprotocol.ForceUpdate

	case cloudprotocol.TimetableUpdate:
		if err = validateTimetable(update.Schedule.Timetable); err != nil {
			return aoserrors.Wrap(err)
		}

	case cloudprotocol.ForceUpdate, cloudprotocol.TriggerUpdate:

	default:
		return aoserrors.New("wrong update type")
	}

	switch manager.CurrentState {
	case stateNoUpdate:
		manager.CurrentUpdate = update

		if manager.TTLDate, err = manager.stateMachine.startNewUpdate(
			time.Duration(manager.CurrentUpdate.Schedule.TTL) * time.Second); err != nil {
			return aoserrors.Wrap(err)
		}

	default:
		if reflect.DeepEqual(update.InstallLayers, manager.CurrentUpdate.InstallLayers) &&
			reflect.DeepEqual(update.RemoveLayers, manager.CurrentUpdate.RemoveLayers) &&
			reflect.DeepEqual(update.InstallServices, manager.CurrentUpdate.InstallServices) &&
			reflect.DeepEqual(update.RemoveServices, manager.CurrentUpdate.RemoveServices) {
			if reflect.DeepEqual(update.Schedule, manager.CurrentUpdate.Schedule) {
				return nil
			}

			// Schedule changed: in ready to update state we can reschedule update. Except current update is forced type,
			// because in this case force update is already scheduled
			if manager.CurrentState == stateReadyToUpdate && (manager.CurrentUpdate.Schedule.Type != cloudprotocol.ForceUpdate) {
				manager.CurrentUpdate.Schedule = update.Schedule

				manager.stateMachine.scheduleUpdate(manager.CurrentUpdate.Schedule)

				return nil
			}
		}

		manager.pendingUpdate = update

		// If current state can't be canceled, wait until it is finished
		if !manager.stateMachine.canTransit(eventCancel) {
			return nil
		}

		if err = manager.stateMachine.sendEvent(eventCancel, ""); err != nil {
			return aoserrors.Wrap(err)
		}
	}

	return nil
}

func (manager *softwareManager) sendCurrentStatus() {
	manager.statusChannel <- manager.getCurrentStatus()
}

func (manager *softwareManager) updateStatusByID(id string, status string, errorStr string) {
	if _, ok := manager.LayerStatuses[id]; ok {
		manager.updateLayerStatusByID(id, status, errorStr)
	} else if _, ok := manager.ServiceStatuses[id]; ok {
		manager.updateServiceStatusByID(id, status, errorStr, "")
	} else {
		log.Errorf("Software update ID not found: %s", id)
	}
}

func (manager *softwareManager) updateLayerStatusByID(id, status, layerErr string) {
	manager.statusMutex.Lock()
	defer manager.statusMutex.Unlock()

	info, ok := manager.LayerStatuses[id]
	if !ok {
		log.Errorf("Can't update software layer status: id %s not found", id)
		return
	}

	info.Status = status
	info.Error = layerErr

	manager.statusHandler.updateLayerStatus(*info)
}

func (manager *softwareManager) updateServiceStatusByID(id, status, serviceErr, stateChecksum string) {
	manager.statusMutex.Lock()
	defer manager.statusMutex.Unlock()

	info, ok := manager.ServiceStatuses[id]
	if !ok {
		log.Errorf("Can't update software service status: id %s not found", id)
		return
	}

	info.Status = status
	info.Error = serviceErr
	info.StateChecksum = stateChecksum

	manager.statusHandler.updateServiceStatus(*info)
}

func (manager *softwareManager) loadState() (err error) {
	stateJSON, err := manager.storage.GetSoftwareUpdateState()
	if err != nil {
		return aoserrors.Wrap(err)
	}

	if len(stateJSON) == 0 {
		return nil
	}

	if err = json.Unmarshal(stateJSON, manager); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func (manager *softwareManager) saveState() (err error) {
	stateJSON, err := json.Marshal(manager)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	if err = manager.storage.SetSoftwareUpdateState(stateJSON); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func (manager *softwareManager) installLayers() (installErr string) {
	var mutex sync.Mutex

	handleError := func(layer cloudprotocol.LayerInfoFromCloud, layerErr string) {
		log.WithFields(log.Fields{
			"digest":     layer.Digest,
			"id":         layer.ID,
			"aosVersion": layer.AosVersion,
		}).Errorf("Can't install layer: %s", layerErr)

		if isCancelError(layerErr) {
			return
		}

		manager.updateLayerStatusByID(layer.Digest, cloudprotocol.ErrorStatus, layerErr)

		mutex.Lock()
		defer mutex.Unlock()

		if installErr == "" {
			installErr = layerErr
		}
	}

	installLayers := make([]cloudprotocol.LayerInfoFromCloud, 0,
		len(manager.CurrentUpdate.DownloadLayers)+len(manager.CurrentUpdate.InstallLayers))

	for _, layer := range manager.CurrentUpdate.DownloadLayers {
		downloadInfo, ok := manager.DownloadResult[layer.Digest]
		if !ok {
			handleError(layer, aoserrors.New("can't get download result").Error())
			continue
		}

		// Do not install not downloaded layers
		if downloadInfo.Error != "" {
			continue
		}

		url := url.URL{
			Scheme: "file",
			Path:   downloadInfo.FileName,
		}

		layer.DecryptDataStruct = cloudprotocol.DecryptDataStruct{
			URLs:   []string{url.String()},
			Size:   downloadInfo.FileInfo.Size,
			Sha256: downloadInfo.FileInfo.Sha256,
			Sha512: downloadInfo.FileInfo.Sha512,
		}

		installLayers = append(installLayers, layer)
	}

	installLayers = append(installLayers, manager.CurrentUpdate.InstallLayers...)

	for _, layer := range installLayers {
		log.WithFields(log.Fields{
			"id":         layer.ID,
			"aosVersion": layer.AosVersion,
			"digest":     layer.Digest,
		}).Debug("Install layer")

		manager.updateLayerStatusByID(layer.Digest, cloudprotocol.InstallingStatus, "")

		// Create new variable to be captured by action function
		layerInfo := layer

		manager.actionHandler.Execute(layerInfo.Digest, func(digest string) {
			if err := manager.softwareUpdater.InstallLayer(layerInfo); err != nil {
				handleError(layerInfo, aoserrors.Wrap(err).Error())
				return
			}

			log.WithFields(log.Fields{
				"id":         layerInfo.ID,
				"aosVersion": layerInfo.AosVersion,
				"digest":     layerInfo.Digest,
			}).Info("Layer successfully installed")

			manager.updateLayerStatusByID(layerInfo.Digest, cloudprotocol.InstalledStatus, "")
		})
	}

	manager.actionHandler.Wait()

	return installErr
}

func (manager *softwareManager) removeLayers() (removeErr string) {
	for _, layer := range manager.CurrentUpdate.RemoveLayers {
		log.WithFields(log.Fields{
			"id":         layer.ID,
			"aosVersion": layer.AosVersion,
			"digest":     layer.Digest,
		}).Debug("Remove layer")

		// Create status for remove layers. For install layer it is created in download function.
		manager.statusMutex.Lock()
		manager.LayerStatuses[layer.Digest] = &cloudprotocol.LayerInfo{
			ID:         layer.ID,
			AosVersion: layer.AosVersion,
			Digest:     layer.Digest,
		}
		manager.statusMutex.Unlock()

		log.WithFields(log.Fields{
			"id":         layer.ID,
			"aosVersion": layer.AosVersion,
			"digest":     layer.Digest,
		}).Info("Layer successfully removed")

		// As we do not perform layer deleting, just update status
		manager.updateLayerStatusByID(layer.Digest, cloudprotocol.RemovedStatus, "")
	}

	return ""
}

func (manager *softwareManager) installServices() (installErr string) {
	var mutex sync.Mutex

	handleError := func(service cloudprotocol.ServiceInfoFromCloud, serviceErr string) {
		log.WithFields(log.Fields{
			"id":         service.ID,
			"aosVersion": service.AosVersion,
		}).Errorf("Can't install service: %s", serviceErr)

		if isCancelError(serviceErr) {
			return
		}

		manager.updateStatusByID(service.ID, cloudprotocol.ErrorStatus, serviceErr)

		mutex.Lock()
		defer mutex.Unlock()

		if installErr == "" {
			installErr = serviceErr
		}
	}

	installServices := make([]cloudprotocol.ServiceInfoFromCloud, 0,
		len(manager.CurrentUpdate.DownloadServices)+len(manager.CurrentUpdate.InstallServices))

	for _, service := range manager.CurrentUpdate.DownloadServices {
		downloadInfo, ok := manager.DownloadResult[service.ID]
		if !ok {
			handleError(service, aoserrors.New("can't get download result").Error())
			continue
		}

		// Skip not downloaded services
		if downloadInfo.Error != "" {
			continue
		}

		url := url.URL{
			Scheme: "file",
			Path:   downloadInfo.FileName,
		}

		service.DecryptDataStruct = cloudprotocol.DecryptDataStruct{
			URLs:   []string{url.String()},
			Size:   downloadInfo.FileInfo.Size,
			Sha256: downloadInfo.FileInfo.Sha256,
			Sha512: downloadInfo.FileInfo.Sha512,
		}

		installServices = append(installServices, service)
	}

	installServices = append(installServices, manager.CurrentUpdate.InstallServices...)

	for _, service := range installServices {
		log.WithFields(log.Fields{
			"id":         service.ID,
			"aosVersion": service.AosVersion,
		}).Debug("Install service")

		manager.updateServiceStatusByID(service.ID, cloudprotocol.InstallingStatus, "", "")

		// Create new variable to be captured by action function
		serviceInfo := service

		manager.actionHandler.Execute(serviceInfo.ID, func(serviceID string) {
			stateChecksum, err := manager.softwareUpdater.InstallService(manager.currentUsers, serviceInfo)
			if err != nil {
				handleError(serviceInfo, aoserrors.Wrap(err).Error())
				return
			}

			log.WithFields(log.Fields{
				"id":            serviceInfo.ID,
				"aosVersion":    serviceInfo.AosVersion,
				"stateChecksum": stateChecksum,
			}).Info("Service successfully installed")

			manager.updateServiceStatusByID(serviceInfo.ID, cloudprotocol.InstalledStatus, "", stateChecksum)
		})
	}

	manager.actionHandler.Wait()

	return installErr
}

func (manager *softwareManager) removeServices() (removeErr string) {
	var mutex sync.Mutex

	handleError := func(service cloudprotocol.ServiceInfo, serviceErr string) {
		log.WithFields(log.Fields{
			"id":         service.ID,
			"aosVersion": service.AosVersion,
		}).Errorf("Can't install service: %s", serviceErr)

		if isCancelError(serviceErr) {
			return
		}

		manager.updateStatusByID(service.ID, cloudprotocol.ErrorStatus, serviceErr)

		mutex.Lock()
		defer mutex.Unlock()

		if removeErr == "" {
			removeErr = serviceErr
		}
	}

	for _, service := range manager.CurrentUpdate.RemoveServices {
		log.WithFields(log.Fields{
			"id":         service.ID,
			"aosVersion": service.AosVersion,
		}).Debug("Remove service")

		// Create status for remove layers. For install layer it is created in download function.
		manager.statusMutex.Lock()
		manager.ServiceStatuses[service.ID] = &cloudprotocol.ServiceInfo{
			ID:         service.ID,
			AosVersion: service.AosVersion,
			Status:     cloudprotocol.RemovingStatus,
		}
		manager.statusMutex.Unlock()

		manager.updateServiceStatusByID(service.ID, cloudprotocol.RemovingStatus, "", "")

		// Create new variable to be captured by action function
		serviceStatus := service

		manager.actionHandler.Execute(serviceStatus.ID, func(serviceID string) {
			if err := manager.softwareUpdater.RemoveService(manager.currentUsers, serviceStatus); err != nil {
				handleError(serviceStatus, err.Error())
				return
			}

			log.WithFields(log.Fields{
				"id":         serviceStatus.ID,
				"aosVersion": serviceStatus.AosVersion,
			}).Info("Service successfully removed")

			manager.updateServiceStatusByID(serviceStatus.ID, cloudprotocol.RemovedStatus, "", "")
		})
	}

	manager.actionHandler.Wait()

	return removeErr
}

func isUsersEqual(users1, users2 []string) (result bool) {
	if users1 == nil && users2 == nil {
		return true
	}

	if users1 == nil || users2 == nil {
		return false
	}

	if len(users1) != len(users2) {
		return false
	}

	for i := range users1 {
		if users1[i] != users2[i] {
			return false
		}
	}

	return true
}
