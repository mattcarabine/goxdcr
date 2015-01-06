// Copyright (c) 2013 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package supervisor

import (
	"errors"
	"fmt"
	"github.com/couchbase/goxdcr/base"
	"github.com/couchbase/goxdcr/common"
	"github.com/couchbase/goxdcr/gen_server"
	"github.com/couchbase/goxdcr/log"
	"github.com/couchbase/goxdcr/utils"
	"reflect"
	"sync"
	"time"
)

// Generic implementation of the Supervisor interface

//configuration settings
const (
	// interval of sending heart beat signals
	HEARTBEAT_INTERVAL = "heartbeat_interval"
	// child is considered to have missed a heart beat if it did not respond within this timeout period
	HEARTBEAT_TIMEOUT = "heartbeat_timeout"
	// child is considered to be broken if it had missed this number of heart beats consecutively
	MISSED_HEARTBEAT_THRESHOLD = "missed_heartbeat_threshold"

	default_heartbeat_interval         time.Duration = 1000 * time.Millisecond
	default_heartbeat_timeout          time.Duration = 4000 * time.Millisecond
	default_missed_heartbeat_threshold               = 5
)

var supervisor_setting_defs base.SettingDefinitions = base.SettingDefinitions{HEARTBEAT_TIMEOUT: base.NewSettingDef(reflect.TypeOf((*time.Duration)(nil)), false),
	HEARTBEAT_INTERVAL:         base.NewSettingDef(reflect.TypeOf((*time.Duration)(nil)), false),
	MISSED_HEARTBEAT_THRESHOLD: base.NewSettingDef(reflect.TypeOf((*uint16)(nil)), false)}

type heartbeatRespStatus int

const (
	skip            heartbeatRespStatus = iota
	notYetResponded heartbeatRespStatus = iota
	respondedOk     heartbeatRespStatus = iota
	respondedNotOk  heartbeatRespStatus = iota
)

type GenericSupervisor struct {
	id string
	gen_server.GenServer
	children                   map[string]common.Supervisable
	children_lock              sync.RWMutex
	loggerContext              *log.LoggerContext
	heartbeat_timeout          time.Duration
	heartbeat_interval         time.Duration
	missed_heartbeat_threshold uint16
	// key - child Id; value - number of consecutive heart beat misses
	childrenBeatMissedMap map[string]uint16
	heartbeat_ticker      *time.Ticker
	failure_handler       common.SupervisorFailureHandler
	finch                 chan bool
	childrenWaitGrp       sync.WaitGroup
	err_ch                chan bool
	resp_waiter_chs       []chan bool
	parent_supervisor     *GenericSupervisor
}

func NewGenericSupervisor(id string, logger_ctx *log.LoggerContext, failure_handler common.SupervisorFailureHandler, parent_supervisor *GenericSupervisor) *GenericSupervisor {
	server := gen_server.NewGenServer(nil,
		nil, nil, logger_ctx, "GenericSupervisor")
	supervisor := &GenericSupervisor{id: id,
		GenServer:                  server,
		children:                   make(map[string]common.Supervisable, 0),
		loggerContext:              logger_ctx,
		heartbeat_timeout:          default_heartbeat_timeout,
		heartbeat_interval:         default_heartbeat_interval,
		missed_heartbeat_threshold: default_missed_heartbeat_threshold,
		childrenBeatMissedMap:      make(map[string]uint16, 0),
		failure_handler:            failure_handler,
		finch:                      make(chan bool, 1),
		childrenWaitGrp:            sync.WaitGroup{},
		err_ch:                     make(chan bool, 1),
		resp_waiter_chs:            []chan bool{},
		parent_supervisor:			parent_supervisor}

	if parent_supervisor != nil {
		parent_supervisor.AddChild(supervisor)
	}
	
	return supervisor
}

func (supervisor *GenericSupervisor) Id() string {
	return supervisor.id
}

func (supervisor *GenericSupervisor) LoggerContext() *log.LoggerContext {
	return supervisor.loggerContext
}

func (supervisor *GenericSupervisor) AddChild(child common.Supervisable) error {
	supervisor.Logger().Infof("Adding child %v to supervisor %v\n", child.Id(), supervisor.Id())

	supervisor.children_lock.Lock()
	defer supervisor.children_lock.Unlock()
	supervisor.children[child.Id()] = child
	return nil
}

func (supervisor *GenericSupervisor) RemoveChild(childId string) error {
	supervisor.Logger().Infof("Removing child %v from supervisor %v\n", childId, supervisor.Id())
	supervisor.children_lock.Lock()
	defer supervisor.children_lock.Unlock()
	// TODO should we return error when childId does not exist?
	delete(supervisor.children, childId)
	return nil
}

func (supervisor *GenericSupervisor) Child(childId string) (common.Supervisable, error) {
	supervisor.children_lock.RLock()
	defer supervisor.children_lock.RUnlock()
	if child, ok := supervisor.children[childId]; ok {
		return child, nil
	} else {
		return nil, errors.New(fmt.Sprintf("Cannot find child %v of supervisor %v\n", childId, supervisor.Id()))
	}
}

func (supervisor *GenericSupervisor) Start(settings map[string]interface{}) error {
	supervisor.Logger().Infof("Starting supervisor %v.\n", supervisor.Id())

	err := supervisor.Init(settings)
	if err == nil {
		//start heartbeat ticker
		supervisor.heartbeat_ticker = time.NewTicker(supervisor.heartbeat_interval)

		supervisor.childrenWaitGrp.Add(1)
		go supervisor.supervising()

		//start the sever looping
		supervisor.GenServer.Start_server()

		supervisor.Logger().Infof("Started supervisor %v.\n", supervisor.Id())
	} else {
		supervisor.Logger().Errorf("Failed to start supervisor %v. error=%v\n", supervisor.Id(), err)
	}
	return err
}

func (supervisor *GenericSupervisor) Stop() error {
	supervisor.Logger().Infof("Stopping supervisor %v.\n", supervisor.Id())

	// make waiting for response routines finish to avoid receiving spurious timeout errors
	supervisor.notifyWaitersToFinish()

	// stop gen_server
	err := supervisor.Stop_server()

	// stop supervising routine
	close(supervisor.finch)

	supervisor.Logger().Debug("Wait for children goroutines to exit")
	supervisor.childrenWaitGrp.Wait()

	supervisor.heartbeat_ticker.Stop()
	supervisor.Logger().Infof("Stopped supervisor %v.\n", supervisor.Id())
	
	supervisor.parent_supervisor.RemoveChild (supervisor.Id())
	return err
}

func (supervisor *GenericSupervisor) supervising() error {
	defer supervisor.childrenWaitGrp.Done()

	//heart beat
	count := 0
loop:
	for {
		count++
		select {
		case <-supervisor.finch:
			break loop
		case <-supervisor.heartbeat_ticker.C:
			supervisor.Logger().Debugf("heart beat tick from super %v\n", supervisor.Id())
			select {
			case supervisor.err_ch <- true:
				supervisor.sendHeartBeats()
			}
		}
	}

	supervisor.Logger().Infof("Supervisor %v exited\n", supervisor.Id())
	return nil
}

func (supervisor *GenericSupervisor) sendHeartBeats() {
	supervisor.Logger().Debugf("Sending heart beat msg from supervisor %v\n", supervisor.Id())

	supervisor.children_lock.RLock()
	defer supervisor.children_lock.RUnlock()

	if len(supervisor.children) > 0 {
		heartbeat_report := make(map[string]heartbeatRespStatus)
		heartbeat_resp_chs := make(map[string]chan []interface{})

		for childId, child := range supervisor.children {
			respch := make(chan []interface{}, 1)
			supervisor.Logger().Debugf("heart beat sent to child %v from super %v\n", childId, supervisor.Id())
			err := child.HeartBeat_async(respch, time.Now())
			heartbeat_resp_chs[childId] = respch
			if err != nil {
				heartbeat_report[childId] = skip
			} else {
				heartbeat_report[childId] = notYetResponded
			}
		}
		fin_ch := make(chan bool, 1)
		supervisor.resp_waiter_chs = append(supervisor.resp_waiter_chs, fin_ch)
		go supervisor.waitForResponse(heartbeat_report, heartbeat_resp_chs, fin_ch)
	} else {
		<-supervisor.err_ch
	}

	return
}

func (supervisor *GenericSupervisor) Init(settings map[string]interface{}) error {
	//initialize settings
	err := utils.ValidateSettings(supervisor_setting_defs, settings, supervisor.Logger())
	if err != nil {
		supervisor.Logger().Errorf("The setting for supervisor %v is not valid. err=%v", supervisor.Id(), err)
		return err
	}

	if val, ok := settings[HEARTBEAT_INTERVAL]; ok {
		supervisor.heartbeat_interval = val.(time.Duration)
	}
	if val, ok := settings[HEARTBEAT_TIMEOUT]; ok {
		supervisor.heartbeat_timeout = val.(time.Duration)
	}

	return nil
}

func (supervisor *GenericSupervisor) waitForResponse(heartbeat_report map[string]heartbeatRespStatus, heartbeat_resp_chs map[string]chan []interface{}, finch chan bool) {
	defer func() {
		<-supervisor.err_ch
		supervisor.Logger().Debugf("Exiting waitForResponse from supervisor %v\n", supervisor.Id())
	}()

	//start a timer
	ping_time := time.Now()
	heartbeat_timeout_ch := time.After(supervisor.heartbeat_timeout)
	responded_count := 0

	for {
		select {
		case <-finch:
			supervisor.Logger().Infof("Wait routine is exiting because parent supervisor %v has been stopped\n", supervisor.Id())
			//the supervisor is stopping
			return
		case <-heartbeat_timeout_ch:
			//time is up
			supervisor.Logger().Errorf("Heartbeat timeout in supervisor %v! not_yet_resp_count=%v\n", supervisor.Id(), len(heartbeat_report)-responded_count)
			goto REPORT
		default:
			for childId, status := range heartbeat_report {
				if status == notYetResponded {
					select {
					case <-heartbeat_resp_chs[childId]:
						responded_count++
						supervisor.Logger().Debugf("Child %v has responded to the heartbeat ping sent at %v to supervisor %v\n", childId, ping_time, supervisor.Id())
						heartbeat_report[childId] = respondedOk
					default:
					}
				}
			}

			if responded_count == len(heartbeat_report) {
				goto REPORT
			}
		}
	}

	//process the result
REPORT:
	supervisor.processReport(heartbeat_report)
}

func (supervisor *GenericSupervisor) processReport(heartbeat_report map[string]heartbeatRespStatus) {
	supervisor.Logger().Debugf("***********ProcessReport for supervisor %v*************\n", supervisor.Id())
	supervisor.Logger().Debugf("len(heartbeat_report)=%v\n", len(heartbeat_report))
	brokenChildren := make(map[string]error)
	for childId, status := range heartbeat_report {
		supervisor.Logger().Debugf("childId=%v, status=%v\n", childId, status)

		if status == respondedNotOk || status == notYetResponded {
			var missedCount uint16
			// missedCount would be zero when child is not yet in the map, which would be the correct value
			missedCount, _ = supervisor.childrenBeatMissedMap[childId]
			missedCount++
			supervisor.Logger().Infof("Child %v of supervisor %v missed %v consecutive heart beats\n", childId, supervisor.Id(), missedCount)
			supervisor.childrenBeatMissedMap[childId] = missedCount
			if missedCount > supervisor.missed_heartbeat_threshold {
				// report the child as broken if it exceeded the beat_missed_threshold
				brokenChildren[childId] = errors.New("Not responding")
			}
		} else {
			// reset missed count to 0 when child responds
			supervisor.childrenBeatMissedMap[childId] = 0
		}
	}

	if len(brokenChildren) > 0 {
		supervisor.ReportFailure(brokenChildren)
	}
}

func (supervisor *GenericSupervisor) ReportFailure(errors map[string]error) {
	//report the failure to decision maker
	if supervisor.heartbeat_ticker != nil {
		supervisor.heartbeat_ticker.Stop()
	}
	supervisor.notifyWaitersToFinish()
	supervisor.failure_handler.OnError(supervisor, errors)
}

func (supervisor *GenericSupervisor) notifyWaitersToFinish() {
	for _, ctrl_ch := range supervisor.resp_waiter_chs {
		select {
		case ctrl_ch <- true:
		default:
		}
	}

}
