// Copyright (c) 2013 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

// metadata service implementation leveraging gometa
package metadata_svc

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/couchbase/goxdcr/base"
	"github.com/couchbase/goxdcr/log"
	"github.com/couchbase/goxdcr/metadata"
	"github.com/couchbase/goxdcr/service_def"
	"github.com/couchbase/goxdcr/utils"
	"strings"
)

const (
	// parent dir of all Replication Specs
	ReplicationSpecsCatalogKey = "replicationSpec"
)

var ReplicationSpecAlreadyExistErrorMessage = "Replication to the same remote cluster and bucket already exists"
var ReplicationSpecNotFoundErrorMessage = "Requested resource not found"
var InvalidReplicationSpecError = errors.New("Invalid Replication spec")

//replication spec and its derived object
//This is what is put into the cache
type ReplicationSpecVal struct {
	spec       *metadata.ReplicationSpecification
	derivedObj interface{}
}

type ReplicationSpecService struct {
	xdcr_comp_topology_svc service_def.XDCRCompTopologySvc
	metadata_svc           service_def.MetadataSvc
	uilog_svc              service_def.UILogSvc
	remote_cluster_svc     service_def.RemoteClusterSvc
	cluster_info_svc       service_def.ClusterInfoSvc
	cache                  *MetadataCache
	logger                 *log.CommonLogger
}

func NewReplicationSpecService(uilog_svc service_def.UILogSvc, remote_cluster_svc service_def.RemoteClusterSvc,
	metadata_svc service_def.MetadataSvc, xdcr_comp_topology_svc service_def.XDCRCompTopologySvc, cluster_info_svc service_def.ClusterInfoSvc,
	logger_ctx *log.LoggerContext) (*ReplicationSpecService, error) {
	logger := log.NewLogger("ReplicationSpecService", logger_ctx)
	svc := &ReplicationSpecService{
		metadata_svc:           metadata_svc,
		uilog_svc:              uilog_svc,
		remote_cluster_svc:     remote_cluster_svc,
		xdcr_comp_topology_svc: xdcr_comp_topology_svc,
		cluster_info_svc:       cluster_info_svc,
		cache:                  nil,
		logger:                 logger,
	}

	err := svc.initCache()
	if err != nil {
		return nil, err
	}
	return svc, nil
}

func (service *ReplicationSpecService) initCache() error {
	service.logger.Info("Init cache for ReplicationSpecService...")
	cache := NewMetadataCache(service.logger)

	entries, err := service.metadata_svc.GetAllMetadataFromCatalog(ReplicationSpecsCatalogKey)
	if err != nil {
		service.logger.Errorf("Failed to get all entries, err=%v\n", err)
		return err
	}

	for _, entry := range entries {
		spec, err := constructReplicationSpec(entry.Value, entry.Rev)
		if err != nil || spec == nil {
			service.logger.Errorf("failed to contruct replication spec, key=%v, err=%v\n", entry.Key, err)
			return err
		}
		service.cacheSpec(cache, spec.Id, spec)
	}
	service.cache = cache
	service.logger.Info("Cache is initialized for ReplicationSpecService")
	return nil
}

func (service *ReplicationSpecService) getCache() (*MetadataCache, error) {
	if service.cache == nil {
		return nil, errors.New("Can't instantiate cahe")
	}
	return service.cache, nil
}

func (service *ReplicationSpecService) ReplicationSpec(replicationId string) (*metadata.ReplicationSpecification, error) {
	cache, err := service.getCache()
	if err != nil {
		return nil, err
	}

	val, ok := cache.Get(replicationId)
	if !ok || val == nil || val.(*ReplicationSpecVal).spec == nil {
		return nil, errors.New(ReplicationSpecNotFoundErrorMessage)
	}

	return val.(*ReplicationSpecVal).spec, nil
}

func (service *ReplicationSpecService) ValidateNewReplicationSpec(sourceBucket, targetCluster, targetBucket string, settings map[string]interface{}) (string, string, *metadata.RemoteClusterReference, map[string]error) {
	service.logger.Infof("Start ValidateAddReplicationSpec, sourceBucket=%v, targetCluster=%v, targetBucket=%v\n", sourceBucket, targetCluster, targetBucket)

	errorMap := make(map[string]error)

	var sourceBucketUUID string

	//validate the existence of source bucket
	local_connStr, _ := service.xdcr_comp_topology_svc.MyConnectionStr()
	if local_connStr == "" {
		panic("XDCRTopologySvc.MyConnectionStr() should not return empty string")
	}

	var err_source error
	sourceBucketUUID, err_source = utils.LocalBucketUUID(local_connStr, sourceBucket)

	if err_source == utils.NonExistentBucketError {
		service.logger.Errorf("Spec [sourceBucket=%v, targetClusterUuid=%v, targetBucket=%v] refers to non-existent bucket\n", sourceBucket, targetCluster, targetBucket)
		errorMap[base.FromBucket] = utils.BucketNotFoundError(sourceBucket)
	}

	// validate remote cluster ref
	targetClusterRef, err := service.remote_cluster_svc.RemoteClusterByRefName(targetCluster, false)
	if err != nil {
		errorMap[base.ToCluster] = utils.NewEnhancedError("cannot find remote cluster", err)
		return "", "", nil, errorMap
	}

	// validate that the source bucket and target bucket are not the same bucket
	// i.e., validate that the following are not both true:
	// 1. sourceBucketName == targetBucketName
	// 2. sourceClusterUuid == targetClusterUuid
	if sourceBucket == targetBucket {
		sourceClusterUuid, err := service.xdcr_comp_topology_svc.MyClusterUuid()
		if err != nil {
			panic("cannot get local cluster uuid")
		}

		if sourceClusterUuid == targetClusterRef.Uuid {
			errorMap[base.PlaceHolderFieldKey] = errors.New("Replication from a bucket to the same bucket is not allowed")
			return "", "", nil, errorMap
		}
	}

	remote_connStr, err := targetClusterRef.MyConnectionStr()
	if err != nil {
		errorMap[base.ToCluster] = utils.NewEnhancedError("invalid remote cluster, MyConnectionStr() failed.", err)
		return "", "", nil, errorMap
	}
	remote_userName, remote_password, err := targetClusterRef.MyCredentials()
	if err != nil {
		errorMap[base.ToCluster] = utils.NewEnhancedError("invalid remote cluster, MyCredentials() failed.", err)
		return "", "", nil, errorMap
	}

	//validate target bucket
	targetBucketUUID, err_target := utils.RemoteBucketUUID(remote_connStr, remote_userName, remote_password, targetBucket)
	if err_target == utils.NonExistentBucketError {
		service.logger.Errorf("Spec [sourceBucket=%v, targetClusterUuid=%v, targetBucket=%v] refers to non-existent target bucket\n", sourceBucket, targetCluster, targetBucket)
		errorMap[base.ToBucket] = utils.BucketNotFoundError(targetBucket)
	}

	repId := metadata.ReplicationId(sourceBucket, targetClusterRef.Uuid, targetBucket)
	_, err = service.ReplicationSpec(repId)
	if err == nil {
		errorMap[base.PlaceHolderFieldKey] = errors.New(ReplicationSpecAlreadyExistErrorMessage)
	}

	// if replication type is set to xmem, validate that the target cluster is xmem compatible
	repl_type, ok := settings[metadata.ReplicationType]
	if !ok || repl_type == metadata.ReplicationTypeXmem {
		xmemCompatible, err := service.cluster_info_svc.IsClusterCompatible(targetClusterRef, []int{2, 2})
		if err != nil {
			errMsg := fmt.Sprintf("Failed to get cluster version information, err=%v\n", err)
			service.logger.Error(errMsg)
			errorMap[base.ToCluster] = errors.New(errMsg)
		} else {
			if !xmemCompatible {
				errorMap[base.ToCluster] = errors.New("Version 2 replication is disallowed. Cluster has nodes with versions less than 2.2.")
			}
		}
	}

	return sourceBucketUUID, targetBucketUUID, targetClusterRef, errorMap
}

func (service *ReplicationSpecService) AddReplicationSpec(spec *metadata.ReplicationSpecification) error {
	service.logger.Infof("Start AddReplicationSpec, spec=%v\n", spec)

	value, err := json.Marshal(spec)
	if err != nil {
		return err
	}

	service.logger.Info("Adding it to metadata store...")
	key := getKeyFromReplicationId(spec.Id)
	err = service.metadata_svc.AddWithCatalog(ReplicationSpecsCatalogKey, key, value)
	if err != nil {
		return err
	}

	_, rev, err := service.metadata_svc.Get(key)
	if err != nil {
		return fmt.Errorf("Failed to query newly added replication spec %v back, err=%v\n", spec.Id, err)
	}
	spec.Revision = rev
	cache, err := service.getCache()
	if err != nil {
		panic("cache is not initialized for ReplicationSpecService")
	}
	service.cacheSpec(cache, spec.Id, spec)

	service.writeUiLog(spec, "created", "")
	return nil
}

func (service *ReplicationSpecService) SetReplicationSpec(spec *metadata.ReplicationSpecification) error {
	value, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	key := getKeyFromReplicationId(spec.Id)
	err = service.metadata_svc.Set(key, value, spec.Revision)
	if err != nil {
		return err
	}

	_, rev, err := service.metadata_svc.Get(key)
	spec.Revision = rev
	cache, err := service.getCache()
	if err != nil {
		panic("cache is not initialized for ReplicationSpecService")
	}
	service.cacheSpec(cache, spec.Id, spec)

	service.logger.Infof("replication spec %s is updated, rev=%v\n", rev)

	return nil
}

func (service *ReplicationSpecService) DelReplicationSpec(replicationId string) (*metadata.ReplicationSpecification, error) {
	return service.delReplicationSpec_internal(replicationId, "")
}

func (service *ReplicationSpecService) delReplicationSpec_internal(replicationId, reason string) (*metadata.ReplicationSpecification, error) {
	spec, err := service.ReplicationSpec(replicationId)
	if err != nil {
		return nil, errors.New(ReplicationSpecNotFoundErrorMessage)
	}

	key := getKeyFromReplicationId(replicationId)
	err = service.metadata_svc.DelWithCatalog(ReplicationSpecsCatalogKey, key, spec.Revision)
	if err != nil {
		service.logger.Errorf("Failed to delete replication spec, key=%v, rev=%v\n", key, spec.Revision)
		return nil, err
	}

	cache, err := service.getCache()
	if err != nil {
		return nil, errors.New("Can't instantiate cahe")
	}
	
	//soft remove it from cache by setting SpecVal.spec = nil, but keep the key there 
	//so that the derived object can still be retrieved and be acted on for cleaning-up.
	val, ok := cache.Get(key)
	if ok && val != nil {
		specVal, ok1 := val.(*ReplicationSpecVal)
		if ok1 {
			specVal.spec = nil
		}
	}
	service.writeUiLog(spec, "removed", "")

	return spec, nil
}

func (service *ReplicationSpecService) AllReplicationSpecs() (map[string]*metadata.ReplicationSpecification, error) {
	specs := make(map[string]*metadata.ReplicationSpecification, 0)
	cache, err := service.getCache()
	if err != nil {
		return nil, errors.New("Can't instantiate cahe")
	}
	values_map := cache.GetMap()
	for key, val := range values_map {
		if val.(*ReplicationSpecVal).spec != nil {
			specs[key] = val.(*ReplicationSpecVal).spec
		}
	}
	return specs, nil
}

func (service *ReplicationSpecService) AllReplicationSpecIds() ([]string, error) {
	repIds := []string{}
	rep_map, err := service.AllReplicationSpecs()
	if err != nil {
		return nil, err
	}
	for key, _ := range rep_map {
		repIds = append(repIds, key)
	}
	return repIds, nil
}

func (service *ReplicationSpecService) AllReplicationSpecIdsForBucket(bucket string) ([]string, error) {
	var repIds []string
	allRepIds, err := service.AllReplicationSpecIds()
	if err != nil {
		return nil, err
	}

	if allRepIds != nil {
		for _, repId := range allRepIds {
			if metadata.IsReplicationIdForSourceBucket(repId, bucket) {
				repIds = append(repIds, repId)
			}
		}
	}
	return repIds, nil
}

func constructReplicationSpec(value []byte, rev interface{}) (*metadata.ReplicationSpecification, error) {
	if value == nil {
		return nil, nil
	}

	spec := &metadata.ReplicationSpecification{}
	err := json.Unmarshal(value, spec)
	if err != nil {
		return nil, err
	}
	spec.Revision = rev
	return spec, nil
}

// Implement callback function for metakv
func (service *ReplicationSpecService) ReplicationSpecServiceCallback(path string, value []byte, rev interface{}) (string, interface{}, interface{}, error) {
	service.logger.Infof("ReplicationSpecServiceCallback called on path = %v\n", path)

	spec, err := constructReplicationSpec(value, rev)
	if err != nil {
		service.logger.Errorf("Error marshaling replication spec. value=%v, err=%v\n", string(value), err)
		return "", nil, nil, err
	}

	repId := service.getReplicationIdFromKey(GetKeyFromPath(path))

	cache, err := service.getCache()
	if err != nil || cache == nil {
		return "", nil, nil, err
	}

	if spec != nil {
		service.cacheSpec(cache, repId, spec)
	} else {
		//the spec is deleted, remove it from the cache
		service.cacheSpec(cache, repId, nil)
	}

	return repId, nil, spec, nil

}

func (service *ReplicationSpecService) writeUiLog(spec *metadata.ReplicationSpecification, action, reason string) {
	if service.uilog_svc != nil {
		var uiLogMsg string
		remoteClusterName := service.remote_cluster_svc.GetRemoteClusterNameFromClusterUuid(spec.TargetClusterUUID)
		if reason != "" {
			uiLogMsg = fmt.Sprintf("Replication from bucket \"%s\" to bucket \"%s\" on cluster \"%s\" %s, since %s", spec.SourceBucketName, spec.TargetBucketName, remoteClusterName, action, reason)
		} else {
			uiLogMsg = fmt.Sprintf("Replication from bucket \"%s\" to bucket \"%s\" on cluster \"%s\" %s.", spec.SourceBucketName, spec.TargetBucketName, remoteClusterName, action)
		}
		service.uilog_svc.Write(uiLogMsg)
	}
}

func (service *ReplicationSpecService) IsReplicationValidationError(err error) bool {
	if err != nil {
		return strings.HasPrefix(err.Error(), ReplicationSpecAlreadyExistErrorMessage) || strings.HasPrefix(err.Error(), ReplicationSpecNotFoundErrorMessage)
	} else {
		return false
	}
}

func getKeyFromReplicationId(replicationId string) string {
	return ReplicationSpecsCatalogKey + base.KeyPartsDelimiter + replicationId
}

func (service *ReplicationSpecService) getReplicationIdFromKey(key string) string {
	prefix := ReplicationSpecsCatalogKey + base.KeyPartsDelimiter
	if !strings.HasPrefix(key, prefix) {
		// should never get here.
		panic(fmt.Sprintf("Got unexpected key %v for replication spec", key))
	}
	return key[len(prefix):]
}

func (service *ReplicationSpecService) ValidateExistingReplicationSpec(spec *metadata.ReplicationSpecification) (error, error) {
	//validate the existence of source bucket
	local_connStr, _ := service.xdcr_comp_topology_svc.MyConnectionStr()
	if local_connStr == "" {
		panic("XDCRTopologySvc.MyConnectionStr() should not return empty string")
	}
	sourceBucketUuid, err_source := utils.LocalBucketUUID(local_connStr, spec.SourceBucketName)

	if err_source == utils.NonExistentBucketError {
		errMsg := fmt.Sprintf("spec %v refers to non-existent source bucket \"%v\"", spec.Id, spec.SourceBucketName)
		service.logger.Error(errMsg)
		return InvalidReplicationSpecError, errors.New(errMsg)
	}

	if spec.SourceBucketUUID != "" && spec.SourceBucketUUID != sourceBucketUuid {
		//spec is referring to a deleted bucket
		errMsg := fmt.Sprintf("spec %v refers to bucket %v which was deleted and recreated", spec.Id, spec.SourceBucketName)
		service.logger.Error(errMsg)
		return InvalidReplicationSpecError, errors.New(errMsg)
	}

	//validate target cluster
	targetClusterRef, err := service.remote_cluster_svc.RemoteClusterByUuid(spec.TargetClusterUUID, false)
	if err == service_def.MetadataNotFoundErr {
		//remote cluster is no longer valid
		errMsg := fmt.Sprintf("spec %v refers to non-existent remote cluster reference \"%v\"", spec.Id, spec.TargetClusterUUID)
		service.logger.Errorf(errMsg)
		return InvalidReplicationSpecError, errors.New(errMsg)
	} else if err != nil {
		return err, nil
	}

	remote_connStr, err := targetClusterRef.MyConnectionStr()
	if err != nil {
		errMsg := fmt.Sprintf("spec %v refers to an invalid remote cluster reference \"%v\", as RemoteClusterRef.MyConnectionStr() returns err=%v\n", spec.Id, spec.TargetClusterUUID, err)
		service.logger.Errorf(errMsg)
		return InvalidReplicationSpecError, errors.New(errMsg)
	}
	remote_userName, remote_password, err := targetClusterRef.MyCredentials()
	if err != nil {
		errMsg := fmt.Sprintf("spec %v refers to an invalid remote cluster reference \"%v\", as RemoteClusterRef.MyCredentials() returns err=%v\n", spec.Id, spec.TargetClusterUUID, err)
		service.logger.Errorf(errMsg)
		return InvalidReplicationSpecError, errors.New(errMsg)
	}

	//validate target bucket
	targetBucketUuid, err_target := utils.RemoteBucketUUID(remote_connStr, remote_userName, remote_password, spec.TargetBucketName)
	if err_target == utils.NonExistentBucketError {
		errMsg := fmt.Sprintf("spec %v refers to non-existent target bucket \"%v\"\n", spec.Id, spec.TargetBucketName)
		service.logger.Errorf(errMsg)
		return InvalidReplicationSpecError, errors.New(errMsg)
	}

	if spec.TargetBucketUUID != "" && spec.TargetBucketUUID != targetBucketUuid {
		//spec is referring to a deleted bucket
		errMsg := fmt.Sprintf("spec %v refers to bucket %v which was deleted and recreated\n", spec.Id, spec.TargetBucketName)
		service.logger.Errorf(errMsg)
		return InvalidReplicationSpecError, errors.New(errMsg)
	}
	return nil, nil
}

func (service *ReplicationSpecService) ValidateAndGC(spec *metadata.ReplicationSpecification) {
	err, detail_err := service.ValidateExistingReplicationSpec(spec)
	if err == InvalidReplicationSpecError {
		service.logger.Errorf("Replication specification %v is no longer valid, garbage collect it. error=%v\n", spec.Id, detail_err)
		_, err1 := service.delReplicationSpec_internal(spec.Id, detail_err.Error())
		if err1 != nil {
			service.logger.Infof("Failed to garbage collect spec %v, err=%v\n", spec.Id, err1)
		}
	}
}

func (service *ReplicationSpecService) sourceBucketUUID(bucketName string) (string, error) {
	local_connStr, _ := service.xdcr_comp_topology_svc.MyConnectionStr()
	if local_connStr == "" {
		panic("XDCRTopologySvc.MyConnectionStr() should not return empty string")
	}
	return utils.LocalBucketUUID(local_connStr, bucketName)
}

func (service *ReplicationSpecService) targetBucketUUID(targetClusterUUID, bucketName string) (string, error) {
	ref, err_target := service.remote_cluster_svc.RemoteClusterByUuid(targetClusterUUID, false)
	if err_target != nil {
		return "", err_target
	}
	remote_connStr, err_target := ref.MyConnectionStr()
	if err_target != nil {
		return "", err_target
	}
	remote_userName, remote_password, err_target := ref.MyCredentials()
	if err_target != nil {
		return "", err_target
	}

	return utils.RemoteBucketUUID(remote_connStr, remote_userName, remote_password, bucketName)
}

func (service *ReplicationSpecService) ConstructNewReplicationSpec(sourceBucketName, targetClusterUUID, targetBucketName string) (*metadata.ReplicationSpecification, error) {
	sourceBucketUUID, err := service.sourceBucketUUID(sourceBucketName)
	if err != nil {
		return nil, err
	}

	targetBucketUUID, err := service.targetBucketUUID(targetClusterUUID, targetBucketName)
	if err != nil {
		return nil, err
	}

	spec := metadata.NewReplicationSpecification(sourceBucketName, sourceBucketUUID, targetClusterUUID, targetBucketName, targetBucketUUID)
	return spec, nil
}

func (service *ReplicationSpecService) cacheSpec(cache *MetadataCache, specId string, spec *metadata.ReplicationSpecification) {
	var cachedObj *ReplicationSpecVal = nil
	var ok1 bool
	cachedVal, ok := cache.Get(specId)
	if ok && cachedVal != nil {
		cachedObj, ok1 = cachedVal.(*ReplicationSpecVal)
		if !ok1 || cachedObj == nil {
			panic("Object in ReplicationSpecServcie cache is not of type *replciationSpecVal")
		}
		cachedObj.spec = spec
	} else {
		//never being cached before
		cachedObj = &ReplicationSpecVal{spec: spec}
	}
	cache.Upsert(specId, cachedObj)
}

func (service *ReplicationSpecService) SetDerivedObj(specId string, derivedObj interface{}) error {
	cache, err := service.getCache()
	if err != nil {
		return errors.New("Can't instantiate cahe")
	}

	cachedVal, ok := cache.Get(specId)
	if !ok || cachedVal == nil {
		return fmt.Errorf(ReplicationSpecNotFoundErrorMessage)
	}
	cachedObj, ok := cachedVal.(*ReplicationSpecVal)
	if !ok {
		panic("Object in ReplicationSpecServcie cache is not of type *replciationSpecVal")
	}
	cachedObj.derivedObj = derivedObj

	if cachedObj.spec == nil && cachedObj.derivedObj == nil {
		//remove it from the cache
		service.logger.Infof("Remove spec %v from the cache\n", specId)
		cache.Delete(specId)
	} else {
		cache.Upsert(specId, cachedObj)
	}
	return nil
}

func (service *ReplicationSpecService) GetDerviedObj(specId string) (interface{}, error) {
	cache, err := service.getCache()
	if err != nil {
		return nil, errors.New("Can't instantiate cahe")
	}
	cachedVal, ok := cache.Get(specId)
	if !ok || cachedVal == nil {
		return nil, fmt.Errorf(ReplicationSpecNotFoundErrorMessage)
	}

	cachedObj, ok := cachedVal.(*ReplicationSpecVal)
	if !ok || cachedObj == nil {
		panic("Object in ReplicationSpecServcie cache is not of type *replciationSpecVal")
	}
	return cachedObj.derivedObj, nil
}
