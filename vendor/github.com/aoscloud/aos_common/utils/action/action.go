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

package action

import (
	"container/list"
	"sync"
)

/*******************************************************************************
 * Types
 ******************************************************************************/

// Handler action handler
type Handler struct {
	sync.Mutex

	maxConcurrentActions int
	wg                   sync.WaitGroup
	waitQueue            *list.List
	workQueue            *list.List
}

type action struct {
	id       string
	doAction func(id string)
}

/*******************************************************************************
 * Public
 ******************************************************************************/

// New creates new action handler
func New(maxConcurrentActions int) (handler *Handler) {
	handler = &Handler{
		maxConcurrentActions: maxConcurrentActions,
		workQueue:            list.New(),
		waitQueue:            list.New(),
	}

	return handler
}

// Execute executes action
func (handler *Handler) Execute(id string, doAction func(id string)) {
	handler.Lock()
	defer handler.Unlock()

	handler.wg.Add(1)

	newAction := action{
		id:       id,
		doAction: doAction,
	}

	if handler.isIDInWorkQueue(newAction.id) {
		handler.waitQueue.PushBack(newAction)
		return
	}

	if handler.workQueue.Len() >= handler.maxConcurrentActions {
		handler.waitQueue.PushBack(newAction)
		return
	}

	go handler.processAction(handler.workQueue.PushBack(newAction))
}

// Wait waits all actions are executed
func (handler *Handler) Wait() {
	handler.wg.Wait()
}

/*******************************************************************************
 * Private
 ******************************************************************************/

func (handler *Handler) isIDInWorkQueue(id string) (result bool) {
	for item := handler.workQueue.Front(); item != nil; item = item.Next() {
		if item.Value.(action).id == id {
			return true
		}
	}

	return false
}

func (handler *Handler) processAction(item *list.Element) {
	defer handler.wg.Done()

	currentAction := item.Value.(action)

	currentAction.doAction(currentAction.id)

	handler.Lock()
	defer handler.Unlock()

	handler.workQueue.Remove(item)

	for waitItem := handler.waitQueue.Front(); waitItem != nil; waitItem = waitItem.Next() {
		if handler.isIDInWorkQueue(waitItem.Value.(action).id) {
			continue
		}

		go handler.processAction(handler.workQueue.PushBack(handler.waitQueue.Remove(waitItem)))

		break
	}
}
