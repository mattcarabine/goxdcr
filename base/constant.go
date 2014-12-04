// Copyright (c) 2013 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package base

import (
	"errors"
)

//constants
var DefaultConnectionSize = 70
var DefaultPoolName = "default"

// URL Paths for retrieving cluster info
var PoolsPath = "/pools"
var SSLPortsPath = "/nodes/self/xdcrSSLPorts"

var SSLPortKey = "httpsMgmt" 

// URL related constants
var UrlDelimiter = "/"
var UrlPortNumberDelimiter = ":"

// http request method types
const (
	MethodGet = "GET"
	MethodPost = "POST"
	MethodDelete = "DELETE"
)

// delimiter for multiple parts in a key
var KeyPartsDelimiter = "_"

//constants for adminport
var AdminportUrlPrefix = UrlDelimiter
var AdminportNumber = 12100
// AdminportReadTimeout timeout, in milliseconds, is read timeout for
// golib's http server.
var AdminportReadTimeout = 0
// AdminportWriteTimeout timeout, in milliseconds, is write timeout for
// golib's http server.
var AdminportWriteTimeout = 0

//outgoing nozzle type
type XDCROutgoingNozzleType int

// used as default value for tests
var GometaRequestPortNumber = 11000

const (
	Xmem XDCROutgoingNozzleType = iota
	Capi XDCROutgoingNozzleType = iota
)

const (
	PIPELINE_SUPERVISOR_SVC string = "PipelineSupervisor"
	CHECKPOINT_MGR_SVC string = "CheckpointManager"
	STATISTICS_MGR_SVC string = "StatisticsManager"
)

// supervisor related constants
const(
	ReplicationManagerSupervisorId = "ReplicationManagerSupervisor"
	PipelineMasterSupervisorId = "PipelineMasterSupervisor"
	AdminportSupervisorId = "AdminportSupervisor"
	PipelineSupervisorIdPrefix = "PipelineSupervisor_"
)

// constants for integer parsing
var ParseIntBase    = 10
var ParseIntBitSize = 64

var ErrorNotMyVbucket = errors.New("NOT_MY_VBUCKET")

