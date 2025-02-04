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

package etcd3

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/etcd3/metrics"
	"k8s.io/apiserver/pkg/storage/value"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	utiltrace "k8s.io/utils/trace"
)

// authenticatedDataString satisfies the value.Context interface. It uses the key to
// authenticate the stored data. This does not defend against reuse of previously
// encrypted values under the same key, but will prevent an attacker from using an
// encrypted value from a different key. A stronger authenticated data segment would
// include the etcd3 Version field (which is incremented on each write to a key and
// reset when the key is deleted), but an attacker with write access to etcd can
// force deletion and recreation of keys to weaken that angle.
type authenticatedDataString string

// AuthenticatedData implements the value.Context interface.
func (d authenticatedDataString) AuthenticatedData() []byte {
	return []byte(string(d))
}

var _ value.Context = authenticatedDataString("")

type store struct {
	client        *clientv3.Client
	codec         runtime.Codec
	versioner     storage.Versioner
	transformer   value.Transformer
	pathPrefix    string
	watcher       *watcher
	pagingEnabled bool
	leaseManager  *leaseManager
}

type objState struct {
	obj   runtime.Object
	meta  *storage.ResponseMeta
	rev   int64
	data  []byte
	stale bool
}

// New returns an etcd3 implementation of storage.Interface.
func New(c *clientv3.Client, codec runtime.Codec, newFunc func() runtime.Object, prefix string, transformer value.Transformer, pagingEnabled bool, leaseManagerConfig LeaseManagerConfig) storage.Interface {
	return newStore(c, codec, newFunc, prefix, transformer, pagingEnabled, leaseManagerConfig)
}

func newStore(c *clientv3.Client, codec runtime.Codec, newFunc func() runtime.Object, prefix string, transformer value.Transformer, pagingEnabled bool, leaseManagerConfig LeaseManagerConfig) *store {
	versioner := APIObjectVersioner{}
	result := &store{
		client:        c,
		codec:         codec,
		versioner:     versioner,
		transformer:   transformer,
		pagingEnabled: pagingEnabled,
		// for compatibility with etcd2 impl.
		// no-op for default prefix of '/registry'.
		// keeps compatibility with etcd2 impl for custom prefixes that don't start with '/'
		pathPrefix:   path.Join("/", prefix),
		watcher:      newWatcher(c, codec, newFunc, versioner, transformer),
		leaseManager: newDefaultLeaseManager(c, leaseManagerConfig),
	}
	return result
}

// Versioner implements storage.Interface.Versioner.
func (s *store) Versioner() storage.Versioner {
	return s.versioner
}

// Get implements storage.Interface.Get.
func (s *store) Get(ctx context.Context, key string, opts storage.GetOptions, out runtime.Object) error {
	clusterName, err := genericapirequest.ClusterNameFrom(ctx)
	if err != nil {
		klog.Errorf("No cluster defined in Get action for key %s : %s", key, err.Error())
	}

	key = path.Join(s.pathPrefix, key)
	startTime := time.Now()
	getResp, err := s.client.KV.Get(ctx, key)
	metrics.RecordEtcdRequestLatency("get", getTypeName(out), startTime)
	if err != nil {
		return err
	}
	if err = s.validateMinimumResourceVersion(opts.ResourceVersion, uint64(getResp.Header.Revision)); err != nil {
		return err
	}

	if len(getResp.Kvs) == 0 {
		if opts.IgnoreNotFound {
			return runtime.SetZeroValue(out)
		}
		return storage.NewKeyNotFoundError(key, 0)
	}
	kv := getResp.Kvs[0]

	data, _, err := s.transformer.TransformFromStorage(kv.Value, authenticatedDataString(key))
	if err != nil {
		return storage.NewInternalError(err.Error())
	}

	return decode(s.codec, s.versioner, data, out, kv.ModRevision, clusterName)
}

// Create implements storage.Interface.Create.
func (s *store) Create(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error {
	clusterName, err := genericapirequest.ClusterNameFrom(ctx)
	if err != nil {
		klog.Errorf("No cluster defined in Create action for key %s : %s", key, err.Error())
	}

	if version, err := s.versioner.ObjectResourceVersion(obj); err == nil && version != 0 {
		return errors.New("resourceVersion should not be set on objects to be created")
	}
	if err := s.versioner.PrepareObjectForStorage(obj); err != nil {
		return fmt.Errorf("PrepareObjectForStorage failed: %v", err)
	}
	data, err := runtime.Encode(s.codec, obj)
	if err != nil {
		return err
	}
	key = path.Join(s.pathPrefix, key)

	opts, err := s.ttlOpts(ctx, int64(ttl))
	if err != nil {
		return err
	}

	newData, err := s.transformer.TransformToStorage(data, authenticatedDataString(key))
	if err != nil {
		return storage.NewInternalError(err.Error())
	}

	startTime := time.Now()
	txnResp, err := s.client.KV.Txn(ctx).If(
		notFound(key),
	).Then(
		clientv3.OpPut(key, string(newData), opts...),
	).Commit()
	metrics.RecordEtcdRequestLatency("create", getTypeName(obj), startTime)
	if err != nil {
		return err
	}
	if !txnResp.Succeeded {
		return storage.NewKeyExistsError(key, 0)
	}

	if out != nil {
		putResp := txnResp.Responses[0].GetResponsePut()
		return decode(s.codec, s.versioner, data, out, putResp.Header.Revision, clusterName)
	}
	return nil
}

// Delete implements storage.Interface.Delete.
func (s *store) Delete(
	ctx context.Context, key string, out runtime.Object, preconditions *storage.Preconditions,
	validateDeletion storage.ValidateObjectFunc, cachedExistingObject runtime.Object) error {
	v, err := conversion.EnforcePtr(out)
	if err != nil {
		return fmt.Errorf("unable to convert output object to pointer: %v", err)
	}
	key = path.Join(s.pathPrefix, key)
	return s.conditionalDelete(ctx, key, out, v, preconditions, validateDeletion, cachedExistingObject)
}

func (s *store) conditionalDelete(
	ctx context.Context, key string, out runtime.Object, v reflect.Value, preconditions *storage.Preconditions,
	validateDeletion storage.ValidateObjectFunc, cachedExistingObject runtime.Object) error {
	var err error
	var clusterName string
	clusterName, err = genericapirequest.ClusterNameFrom(ctx)
	if err != nil {
		klog.Errorf("No cluster defined in conditionalDelete action for key %s : %s", key, err.Error())
	}
	
	getCurrentState := func() (*objState, error) {
		startTime := time.Now()
		getResp, err := s.client.KV.Get(ctx, key)
		metrics.RecordEtcdRequestLatency("get", getTypeName(out), startTime)
		if err != nil {
			return nil, err
		}
		return s.getState(getResp, key, v, false, clusterName)
	}

	var origState *objState
	var origStateIsCurrent bool
	if cachedExistingObject != nil {
		origState, err = s.getStateFromObject(cachedExistingObject)
	} else {
		origState, err = getCurrentState()
		origStateIsCurrent = true
	}
	if err != nil {
		return err
	}

	for {
		if preconditions != nil {
			if err := preconditions.Check(key, origState.obj); err != nil {
				if origStateIsCurrent {
					return err
				}

				// It's possible we're working with stale data.
				// Actually fetch
				origState, err = getCurrentState()
				if err != nil {
					return err
				}
				origStateIsCurrent = true
				// Retry
				continue
			}
		}
		if err := validateDeletion(ctx, origState.obj); err != nil {
			if origStateIsCurrent {
				return err
			}

			// It's possible we're working with stale data.
			// Actually fetch
			origState, err = getCurrentState()
			if err != nil {
				return err
			}
			origStateIsCurrent = true
			// Retry
			continue
		}

		startTime := time.Now()
		txnResp, err := s.client.KV.Txn(ctx).If(
			clientv3.Compare(clientv3.ModRevision(key), "=", origState.rev),
		).Then(
			clientv3.OpDelete(key),
		).Else(
			clientv3.OpGet(key),
		).Commit()
		metrics.RecordEtcdRequestLatency("delete", getTypeName(out), startTime)
		if err != nil {
			return err
		}
		if !txnResp.Succeeded {
			getResp := (*clientv3.GetResponse)(txnResp.Responses[0].GetResponseRange())
			klog.V(4).Infof("deletion of %s failed because of a conflict, going to retry", key)
			origState, err = s.getState(getResp, key, v, false, clusterName)
			if err != nil {
				return err
			}
			origStateIsCurrent = true
			continue
		}
		return decode(s.codec, s.versioner, origState.data, out, origState.rev, clusterName)
	}
}

// GuaranteedUpdate implements storage.Interface.GuaranteedUpdate.
func (s *store) GuaranteedUpdate(
	ctx context.Context, key string, out runtime.Object, ignoreNotFound bool,
	preconditions *storage.Preconditions, tryUpdate storage.UpdateFunc, cachedExistingObject runtime.Object) error {
	trace := utiltrace.New("GuaranteedUpdate etcd3", utiltrace.Field{"type", getTypeName(out)})
	defer trace.LogIfLong(500 * time.Millisecond)

	clusterName, err := genericapirequest.ClusterNameFrom(ctx)
	if err != nil {
		klog.Errorf("No cluster defined in GuaranteedUpdate action for key %s : %s", key, err.Error())
	}

	v, err := conversion.EnforcePtr(out)
	if err != nil {
		return fmt.Errorf("unable to convert output object to pointer: %v", err)
	}
	key = path.Join(s.pathPrefix, key)

	getCurrentState := func() (*objState, error) {
		startTime := time.Now()
		getResp, err := s.client.KV.Get(ctx, key)
		metrics.RecordEtcdRequestLatency("get", getTypeName(out), startTime)
		if err != nil {
			return nil, err
		}
		return s.getState(getResp, key, v, ignoreNotFound, clusterName)
	}

	var origState *objState
	var origStateIsCurrent bool
	if cachedExistingObject != nil {
		origState, err = s.getStateFromObject(cachedExistingObject)
	} else {
		origState, err = getCurrentState()
		origStateIsCurrent = true
	}
	if err != nil {
		return err
	}
	trace.Step("initial value restored")

	transformContext := authenticatedDataString(key)
	for {
		if err := preconditions.Check(key, origState.obj); err != nil {
			// If our data is already up to date, return the error
			if origStateIsCurrent {
				return err
			}

			// It's possible we were working with stale data
			// Actually fetch
			origState, err = getCurrentState()
			if err != nil {
				return err
			}
			origStateIsCurrent = true
			// Retry
			continue
		}

		ret, ttl, err := s.updateState(origState, tryUpdate)
		if err != nil {
			// If our data is already up to date, return the error
			if origStateIsCurrent {
				return err
			}

			// It's possible we were working with stale data
			// Actually fetch
			origState, err = getCurrentState()
			if err != nil {
				return err
			}
			origStateIsCurrent = true
			// Retry
			continue
		}

		data, err := runtime.Encode(s.codec, ret)
		if err != nil {
			return err
		}
		if !origState.stale && bytes.Equal(data, origState.data) {
			// if we skipped the original Get in this loop, we must refresh from
			// etcd in order to be sure the data in the store is equivalent to
			// our desired serialization
			if !origStateIsCurrent {
				origState, err = getCurrentState()
				if err != nil {
					return err
				}
				origStateIsCurrent = true
				if !bytes.Equal(data, origState.data) {
					// original data changed, restart loop
					continue
				}
			}
			// recheck that the data from etcd is not stale before short-circuiting a write
			if !origState.stale {
				return decode(s.codec, s.versioner, origState.data, out, origState.rev, clusterName)
			}
		}

		newData, err := s.transformer.TransformToStorage(data, transformContext)
		if err != nil {
			return storage.NewInternalError(err.Error())
		}

		opts, err := s.ttlOpts(ctx, int64(ttl))
		if err != nil {
			return err
		}
		trace.Step("Transaction prepared")

		startTime := time.Now()
		txnResp, err := s.client.KV.Txn(ctx).If(
			clientv3.Compare(clientv3.ModRevision(key), "=", origState.rev),
		).Then(
			clientv3.OpPut(key, string(newData), opts...),
		).Else(
			clientv3.OpGet(key),
		).Commit()
		metrics.RecordEtcdRequestLatency("update", getTypeName(out), startTime)
		if err != nil {
			return err
		}
		trace.Step("Transaction committed")
		if !txnResp.Succeeded {
			getResp := (*clientv3.GetResponse)(txnResp.Responses[0].GetResponseRange())
			klog.V(4).Infof("GuaranteedUpdate of %s failed because of a conflict, going to retry", key)
			origState, err = s.getState(getResp, key, v, ignoreNotFound, clusterName)
			if err != nil {
				return err
			}
			trace.Step("Retry value restored")
			origStateIsCurrent = true
			continue
		}
		putResp := txnResp.Responses[0].GetResponsePut()

		return decode(s.codec, s.versioner, data, out, putResp.Header.Revision, clusterName)
	}
}

// GetToList implements storage.Interface.GetToList.
func (s *store) GetToList(ctx context.Context, key string, listOpts storage.ListOptions, listObj runtime.Object) error {
	resourceVersion := listOpts.ResourceVersion
	match := listOpts.ResourceVersionMatch
	pred := listOpts.Predicate
	trace := utiltrace.New("GetToList etcd3",
		utiltrace.Field{"key", key},
		utiltrace.Field{"resourceVersion", resourceVersion},
		utiltrace.Field{"resourceVersionMatch", match},
		utiltrace.Field{"limit", pred.Limit},
		utiltrace.Field{"continue", pred.Continue})
	defer trace.LogIfLong(500 * time.Millisecond)

	clusterName, err := genericapirequest.ClusterNameFrom(ctx)
	if err != nil {
		klog.Errorf("No cluster defined in GetToList action for key %s : %s", key, err.Error())
	}

	listPtr, err := meta.GetItemsPtr(listObj)
	if err != nil {
		return err
	}
	v, err := conversion.EnforcePtr(listPtr)
	if err != nil || v.Kind() != reflect.Slice {
		return fmt.Errorf("need ptr to slice: %v", err)
	}

	newItemFunc := getNewItemFunc(listObj, v)

	key = path.Join(s.pathPrefix, key)
	startTime := time.Now()
	var opts []clientv3.OpOption
	if len(resourceVersion) > 0 && match == metav1.ResourceVersionMatchExact {
		rv, err := s.versioner.ParseResourceVersion(resourceVersion)
		if err != nil {
			return apierrors.NewBadRequest(fmt.Sprintf("invalid resource version: %v", err))
		}
		opts = append(opts, clientv3.WithRev(int64(rv)))
	}

	getResp, err := s.client.KV.Get(ctx, key, opts...)
	metrics.RecordEtcdRequestLatency("get", getTypeName(listPtr), startTime)
	if err != nil {
		return err
	}
	if err = s.validateMinimumResourceVersion(resourceVersion, uint64(getResp.Header.Revision)); err != nil {
		return err
	}

	if len(getResp.Kvs) > 0 {
		data, _, err := s.transformer.TransformFromStorage(getResp.Kvs[0].Value, authenticatedDataString(key))
		if err != nil {
			return storage.NewInternalError(err.Error())
		}
		if err := appendListItem(v, data, uint64(getResp.Kvs[0].ModRevision), pred, s.codec, s.versioner, newItemFunc, clusterName); err != nil {
			return err
		}
	}
	// update version with cluster level revision
	return s.versioner.UpdateList(listObj, uint64(getResp.Header.Revision), "", nil)
}

func getNewItemFunc(listObj runtime.Object, v reflect.Value) func() runtime.Object {
	// For unstructured lists with a target group/version, preserve the group/version in the instantiated list items
	if unstructuredList, isUnstructured := listObj.(*unstructured.UnstructuredList); isUnstructured {
		if apiVersion := unstructuredList.GetAPIVersion(); len(apiVersion) > 0 {
			return func() runtime.Object {
				return &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": apiVersion}}
			}
		}
	}

	// Otherwise just instantiate an empty item
	elem := v.Type().Elem()
	return func() runtime.Object {
		return reflect.New(elem).Interface().(runtime.Object)
	}
}

func (s *store) Count(key string) (int64, error) {
	key = path.Join(s.pathPrefix, key)

	// We need to make sure the key ended with "/" so that we only get children "directories".
	// e.g. if we have key "/a", "/a/b", "/ab", getting keys with prefix "/a" will return all three,
	// while with prefix "/a/" will return only "/a/b" which is the correct answer.
	if !strings.HasSuffix(key, "/") {
		key += "/"
	}

	startTime := time.Now()
	getResp, err := s.client.KV.Get(context.Background(), key, clientv3.WithRange(clientv3.GetPrefixRangeEnd(key)), clientv3.WithCountOnly())
	metrics.RecordEtcdRequestLatency("listWithCount", key, startTime)
	if err != nil {
		return 0, err
	}
	return getResp.Count, nil
}

// continueToken is a simple structured object for encoding the state of a continue token.
// TODO: if we change the version of the encoded from, we can't start encoding the new version
// until all other servers are upgraded (i.e. we need to support rolling schema)
// This is a public API struct and cannot change.
type continueToken struct {
	APIVersion      string `json:"v"`
	ResourceVersion int64  `json:"rv"`
	StartKey        string `json:"start"`
}

// parseFrom transforms an encoded predicate from into a versioned struct.
// TODO: return a typed error that instructs clients that they must relist
func decodeContinue(continueValue, keyPrefix string) (fromKey string, rv int64, err error) {
	data, err := base64.RawURLEncoding.DecodeString(continueValue)
	if err != nil {
		return "", 0, fmt.Errorf("continue key is not valid: %v", err)
	}
	var c continueToken
	if err := json.Unmarshal(data, &c); err != nil {
		return "", 0, fmt.Errorf("continue key is not valid: %v", err)
	}
	switch c.APIVersion {
	case "meta.k8s.io/v1":
		if c.ResourceVersion == 0 {
			return "", 0, fmt.Errorf("continue key is not valid: incorrect encoded start resourceVersion (version meta.k8s.io/v1)")
		}
		if len(c.StartKey) == 0 {
			return "", 0, fmt.Errorf("continue key is not valid: encoded start key empty (version meta.k8s.io/v1)")
		}
		// defend against path traversal attacks by clients - path.Clean will ensure that startKey cannot
		// be at a higher level of the hierarchy, and so when we append the key prefix we will end up with
		// continue start key that is fully qualified and cannot range over anything less specific than
		// keyPrefix.
		key := c.StartKey
		if !strings.HasPrefix(key, "/") {
			key = "/" + key
		}
		cleaned := path.Clean(key)
		if cleaned != key {
			return "", 0, fmt.Errorf("continue key is not valid: %s", c.StartKey)
		}
		return keyPrefix + cleaned[1:], c.ResourceVersion, nil
	default:
		return "", 0, fmt.Errorf("continue key is not valid: server does not recognize this encoded version %q", c.APIVersion)
	}
}

// encodeContinue returns a string representing the encoded continuation of the current query.
func encodeContinue(key, keyPrefix string, resourceVersion int64) (string, error) {
	nextKey := strings.TrimPrefix(key, keyPrefix)
	if nextKey == key {
		return "", fmt.Errorf("unable to encode next field: the key and key prefix do not match")
	}
	out, err := json.Marshal(&continueToken{APIVersion: "meta.k8s.io/v1", ResourceVersion: resourceVersion, StartKey: nextKey})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// List implements storage.Interface.List.
func (s *store) List(ctx context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
	resourceVersion := opts.ResourceVersion
	match := opts.ResourceVersionMatch
	pred := opts.Predicate
	trace := utiltrace.New("List etcd3",
		utiltrace.Field{"key", key},
		utiltrace.Field{"resourceVersion", resourceVersion},
		utiltrace.Field{"resourceVersionMatch", match},
		utiltrace.Field{"limit", pred.Limit},
		utiltrace.Field{"continue", pred.Continue})
	defer trace.LogIfLong(500 * time.Millisecond)
	listPtr, err := meta.GetItemsPtr(listObj)
	if err != nil {
		return err
	}
	v, err := conversion.EnforcePtr(listPtr)
	if err != nil || v.Kind() != reflect.Slice {
		return fmt.Errorf("need ptr to slice: %v", err)
	}

	if s.pathPrefix != "" {
		key = path.Join(s.pathPrefix, key)
	}
	// We need to make sure the key ended with "/" so that we only get children "directories".
	// e.g. if we have key "/a", "/a/b", "/ab", getting keys with prefix "/a" will return all three,
	// while with prefix "/a/" will return only "/a/b" which is the correct answer.
	if !strings.HasSuffix(key, "/") {
		key += "/"
	}
	keyPrefix := key

	// set the appropriate clientv3 options to filter the returned data set
	var paging bool
	options := make([]clientv3.OpOption, 0, 4)
	if s.pagingEnabled && pred.Limit > 0 {
		paging = true
		options = append(options, clientv3.WithLimit(pred.Limit))
	}

	newItemFunc := getNewItemFunc(listObj, v)

	var fromRV *uint64
	if len(resourceVersion) > 0 {
		parsedRV, err := s.versioner.ParseResourceVersion(resourceVersion)
		if err != nil {
			return apierrors.NewBadRequest(fmt.Sprintf("invalid resource version: %v", err))
		}
		fromRV = &parsedRV
	}

	var returnedRV, continueRV, withRev int64
	var continueKey string
	switch {
	case s.pagingEnabled && len(pred.Continue) > 0:
		continueKey, continueRV, err = decodeContinue(pred.Continue, keyPrefix)
		if err != nil {
			return apierrors.NewBadRequest(fmt.Sprintf("invalid continue token: %v", err))
		}

		if len(resourceVersion) > 0 && resourceVersion != "0" {
			return apierrors.NewBadRequest("specifying resource version is not allowed when using continue")
		}

		rangeEnd := clientv3.GetPrefixRangeEnd(keyPrefix)
		options = append(options, clientv3.WithRange(rangeEnd))
		key = continueKey

		// If continueRV > 0, the LIST request needs a specific resource version.
		// continueRV==0 is invalid.
		// If continueRV < 0, the request is for the latest resource version.
		if continueRV > 0 {
			withRev = continueRV
			returnedRV = continueRV
		}
	case s.pagingEnabled && pred.Limit > 0:
		if fromRV != nil {
			switch match {
			case metav1.ResourceVersionMatchNotOlderThan:
				// The not older than constraint is checked after we get a response from etcd,
				// and returnedRV is then set to the revision we get from the etcd response.
			case metav1.ResourceVersionMatchExact:
				returnedRV = int64(*fromRV)
				withRev = returnedRV
			case "": // legacy case
				if *fromRV > 0 {
					returnedRV = int64(*fromRV)
					withRev = returnedRV
				}
			default:
				return fmt.Errorf("unknown ResourceVersionMatch value: %v", match)
			}
		}

		rangeEnd := clientv3.GetPrefixRangeEnd(keyPrefix)
		options = append(options, clientv3.WithRange(rangeEnd))
	default:
		if fromRV != nil {
			switch match {
			case metav1.ResourceVersionMatchNotOlderThan:
				// The not older than constraint is checked after we get a response from etcd,
				// and returnedRV is then set to the revision we get from the etcd response.
			case metav1.ResourceVersionMatchExact:
				returnedRV = int64(*fromRV)
				withRev = returnedRV
			case "": // legacy case
			default:
				return fmt.Errorf("unknown ResourceVersionMatch value: %v", match)
			}
		}

		options = append(options, clientv3.WithPrefix())
	}
	if withRev != 0 {
		options = append(options, clientv3.WithRev(withRev))
	}

	// loop until we have filled the requested limit from etcd or there are no more results
	var lastKey []byte
	var hasMore bool
	var getResp *clientv3.GetResponse
	for {
		startTime := time.Now()
		getResp, err = s.client.KV.Get(ctx, key, options...)
		metrics.RecordEtcdRequestLatency("list", getTypeName(listPtr), startTime)
		if err != nil {
			return interpretListError(err, len(pred.Continue) > 0, continueKey, keyPrefix)
		}
		if err = s.validateMinimumResourceVersion(resourceVersion, uint64(getResp.Header.Revision)); err != nil {
			return err
		}
		hasMore = getResp.More

		if len(getResp.Kvs) == 0 && getResp.More {
			return fmt.Errorf("no results were found, but etcd indicated there were more values remaining")
		}

		// avoid small allocations for the result slice, since this can be called in many
		// different contexts and we don't know how significantly the result will be filtered
		if pred.Empty() {
			growSlice(v, len(getResp.Kvs))
		} else {
			growSlice(v, 2048, len(getResp.Kvs))
		}

		// take items from the response until the bucket is full, filtering as we go
		for _, kv := range getResp.Kvs {
			if paging && int64(v.Len()) >= pred.Limit {
				hasMore = true
				break
			}
			lastKey = kv.Key

			data, _, err := s.transformer.TransformFromStorage(kv.Value, authenticatedDataString(kv.Key))
			if err != nil {
				return storage.NewInternalErrorf("unable to transform key %q: %v", kv.Key, err)
			}

			cluster, err := genericapirequest.ValidClusterFrom(ctx)
			if err != nil {
				return storage.NewInternalErrorf("unable to get cluster for key %q: %v", kv.Key, err)
			}
			clusterName := cluster.Name
			if cluster.Wildcard {
				sub := strings.TrimPrefix(string(kv.Key), keyPrefix)
				if i := strings.Index(sub, "/"); i != -1 {
					clusterName = sub[:i]
				}
				if clusterName == "" {
					klog.Errorf("the cluster name of extracted object should not be empty for key %q", kv.Key)
				}
			}
			if err := appendListItem(v, data, uint64(kv.ModRevision), pred, s.codec, s.versioner, newItemFunc, clusterName); err != nil {
				return err
			}
		}

		// indicate to the client which resource version was returned
		if returnedRV == 0 {
			returnedRV = getResp.Header.Revision
		}

		// no more results remain or we didn't request paging
		if !hasMore || !paging {
			break
		}
		// we're paging but we have filled our bucket
		if int64(v.Len()) >= pred.Limit {
			break
		}
		key = string(lastKey) + "\x00"
		if withRev == 0 {
			withRev = returnedRV
			options = append(options, clientv3.WithRev(withRev))
		}
	}

	// instruct the client to begin querying from immediately after the last key we returned
	// we never return a key that the client wouldn't be allowed to see
	if hasMore {
		// we want to start immediately after the last key
		next, err := encodeContinue(string(lastKey)+"\x00", keyPrefix, returnedRV)
		if err != nil {
			return err
		}
		var remainingItemCount *int64
		// getResp.Count counts in objects that do not match the pred.
		// Instead of returning inaccurate count for non-empty selectors, we return nil.
		// Only set remainingItemCount if the predicate is empty.
		if utilfeature.DefaultFeatureGate.Enabled(features.RemainingItemCount) {
			if pred.Empty() {
				c := int64(getResp.Count - pred.Limit)
				remainingItemCount = &c
			}
		}
		return s.versioner.UpdateList(listObj, uint64(returnedRV), next, remainingItemCount)
	}

	// no continuation
	return s.versioner.UpdateList(listObj, uint64(returnedRV), "", nil)
}

// growSlice takes a slice value and grows its capacity up
// to the maximum of the passed sizes or maxCapacity, whichever
// is smaller. Above maxCapacity decisions about allocation are left
// to the Go runtime on append. This allows a caller to make an
// educated guess about the potential size of the total list while
// still avoiding overly aggressive initial allocation. If sizes
// is empty maxCapacity will be used as the size to grow.
func growSlice(v reflect.Value, maxCapacity int, sizes ...int) {
	cap := v.Cap()
	max := cap
	for _, size := range sizes {
		if size > max {
			max = size
		}
	}
	if len(sizes) == 0 || max > maxCapacity {
		max = maxCapacity
	}
	if max <= cap {
		return
	}
	if v.Len() > 0 {
		extra := reflect.MakeSlice(v.Type(), 0, max)
		reflect.Copy(extra, v)
		v.Set(extra)
	} else {
		extra := reflect.MakeSlice(v.Type(), 0, max)
		v.Set(extra)
	}
}

// Watch implements storage.Interface.Watch.
func (s *store) Watch(ctx context.Context, key string, opts storage.ListOptions) (watch.Interface, error) {
	return s.watch(ctx, key, opts, false)
}

// WatchList implements storage.Interface.WatchList.
func (s *store) WatchList(ctx context.Context, key string, opts storage.ListOptions) (watch.Interface, error) {
	return s.watch(ctx, key, opts, true)
}

func (s *store) watch(ctx context.Context, key string, opts storage.ListOptions, recursive bool) (watch.Interface, error) {
	rev, err := s.versioner.ParseResourceVersion(opts.ResourceVersion)
	if err != nil {
		return nil, err
	}
	key = path.Join(s.pathPrefix, key)

	// HACK: would need to be an argument to storage (or a change to how decoding works for key structure)
	cluster, err := genericapirequest.ValidClusterFrom(ctx)
	if err != nil {
		return nil, storage.NewInternalError(fmt.Sprintf("Invalid cluster for key %s : %v", key, err))
	}
	extractClusterSegmentFromKey := false
	clusterName := cluster.Name
	if cluster.Wildcard {
		clusterName = "*"
		extractClusterSegmentFromKey = true
	}

	klog.Infof("DEBUG: key=%s willExtractCluster=%t", key, extractClusterSegmentFromKey)
	return s.watcher.Watch(ctx, key, int64(rev), recursive, clusterName, opts.ProgressNotify, opts.Predicate)
}

func (s *store) getState(getResp *clientv3.GetResponse, key string, v reflect.Value, ignoreNotFound bool, clusterName string) (*objState, error) {
	state := &objState{
		meta: &storage.ResponseMeta{},
	}

	if u, ok := v.Addr().Interface().(runtime.Unstructured); ok {
		state.obj = u.NewEmptyInstance()
	} else {
		state.obj = reflect.New(v.Type()).Interface().(runtime.Object)
	}

	if len(getResp.Kvs) == 0 {
		if !ignoreNotFound {
			return nil, storage.NewKeyNotFoundError(key, 0)
		}
		if err := runtime.SetZeroValue(state.obj); err != nil {
			return nil, err
		}
	} else {
		data, stale, err := s.transformer.TransformFromStorage(getResp.Kvs[0].Value, authenticatedDataString(key))
		if err != nil {
			return nil, storage.NewInternalError(err.Error())
		}
		state.rev = getResp.Kvs[0].ModRevision
		state.meta.ResourceVersion = uint64(state.rev)
		state.data = data
		state.stale = stale
		if err := decode(s.codec, s.versioner, state.data, state.obj, state.rev, clusterName); err != nil {
			return nil, err
		}
	}
	return state, nil
}

func (s *store) getStateFromObject(obj runtime.Object) (*objState, error) {
	state := &objState{
		obj:  obj,
		meta: &storage.ResponseMeta{},
	}

	rv, err := s.versioner.ObjectResourceVersion(obj)
	if err != nil {
		return nil, fmt.Errorf("couldn't get resource version: %v", err)
	}
	state.rev = int64(rv)
	state.meta.ResourceVersion = uint64(state.rev)

	// Compute the serialized form - for that we need to temporarily clean
	// its resource version field (those are not stored in etcd).
	if err := s.versioner.PrepareObjectForStorage(obj); err != nil {
		return nil, fmt.Errorf("PrepareObjectForStorage failed: %v", err)
	}
	state.data, err = runtime.Encode(s.codec, obj)
	if err != nil {
		return nil, err
	}
	if err := s.versioner.UpdateObject(state.obj, uint64(rv)); err != nil {
		klog.Errorf("failed to update object version: %v", err)
	}
	return state, nil
}

func (s *store) updateState(st *objState, userUpdate storage.UpdateFunc) (runtime.Object, uint64, error) {
	ret, ttlPtr, err := userUpdate(st.obj, *st.meta)
	if err != nil {
		return nil, 0, err
	}

	if err := s.versioner.PrepareObjectForStorage(ret); err != nil {
		return nil, 0, fmt.Errorf("PrepareObjectForStorage failed: %v", err)
	}
	var ttl uint64
	if ttlPtr != nil {
		ttl = *ttlPtr
	}
	return ret, ttl, nil
}

// ttlOpts returns client options based on given ttl.
// ttl: if ttl is non-zero, it will attach the key to a lease with ttl of roughly the same length
func (s *store) ttlOpts(ctx context.Context, ttl int64) ([]clientv3.OpOption, error) {
	if ttl == 0 {
		return nil, nil
	}
	id, err := s.leaseManager.GetLease(ctx, ttl)
	if err != nil {
		return nil, err
	}
	return []clientv3.OpOption{clientv3.WithLease(id)}, nil
}

// validateMinimumResourceVersion returns a 'too large resource' version error when the provided minimumResourceVersion is
// greater than the most recent actualRevision available from storage.
func (s *store) validateMinimumResourceVersion(minimumResourceVersion string, actualRevision uint64) error {
	if minimumResourceVersion == "" {
		return nil
	}
	minimumRV, err := s.versioner.ParseResourceVersion(minimumResourceVersion)
	if err != nil {
		return apierrors.NewBadRequest(fmt.Sprintf("invalid resource version: %v", err))
	}
	// Enforce the storage.Interface guarantee that the resource version of the returned data
	// "will be at least 'resourceVersion'".
	if minimumRV > actualRevision {
		return storage.NewTooLargeResourceVersionError(minimumRV, actualRevision, 0)
	}
	return nil
}

// decode decodes value of bytes into object. It will also set the object resource version to rev.
// On success, objPtr would be set to the object.
func decode(codec runtime.Codec, versioner storage.Versioner, value []byte, objPtr runtime.Object, rev int64, clusterName string) error {
	if _, err := conversion.EnforcePtr(objPtr); err != nil {
		return fmt.Errorf("unable to convert output object to pointer: %v", err)
	}
	_, _, err := codec.Decode(value, nil, objPtr)
	if err != nil {
		return err
	}
	// being unable to set the version does not prevent the object from being extracted
	if err := versioner.UpdateObject(objPtr, uint64(rev)); err != nil {
		klog.Errorf("failed to update object version: %v", err)
	}
	// HACK: in order to support CRD tenancy, the clusterName, which is extracted from the object etcd key,
	// should be set on the decoded object.
	// This is done here since we want to set the logical cluster the object is part of,
	// without storing the clusterName inside the etcd object itself (as it has been until now).
	// The etcd key is ultimately the only thing that links us to a cluster
	if clusterName != "" {
		if s, ok := objPtr.(metav1.ObjectMetaAccessor); ok {
			klog.Infof("Setting ClusterName %s in appendListItem", clusterName)
			s.GetObjectMeta().SetClusterName(clusterName)
		} else if s, ok := objPtr.(metav1.Object); ok {
			klog.Infof("Setting ClusterName %s in appendListItem", clusterName)
			s.SetClusterName(clusterName)
		} else if s, ok := objPtr.(*unstructured.Unstructured); ok {
			klog.Infof("SUB: %s", clusterName)
			s.SetClusterName(clusterName)
		} else {
			klog.Infof("Could not set ClusterName %s in appendListItem on object: %T", clusterName, objPtr)
		}
	} else {
		klog.Errorf("Cluster should not be unknown")
	}

	return nil
}

// appendListItem decodes and appends the object (if it passes filter) to v, which must be a slice.
func appendListItem(v reflect.Value, data []byte, rev uint64, pred storage.SelectionPredicate, codec runtime.Codec, versioner storage.Versioner, newItemFunc func() runtime.Object, clusterName string) error {
	obj, _, err := codec.Decode(data, nil, newItemFunc())
	if err != nil {
		return err
	}
	// being unable to set the version does not prevent the object from being extracted
	if err := versioner.UpdateObject(obj, rev); err != nil {
		klog.Errorf("failed to update object version: %v", err)
	}

	// HACK: in order to support CRD tenancy, the clusterName, which is extracted from the object etcd key,
	// should be set on the decoded object.
	// This is done here since we want to set the logical cluster the object is part of,
	// without storing the clusterName inside the etcd object itself (as it has been until now).
	// The etcd key is ultimately the only thing that links us to a cluster
	if clusterName != "" {
		if s, ok := obj.(metav1.ObjectMetaAccessor); ok {
			klog.Infof("Setting ClusterName %s in appendListItem", clusterName)
			s.GetObjectMeta().SetClusterName(clusterName)
		} else if s, ok := obj.(metav1.Object); ok {
			klog.Infof("Setting ClusterName %s in appendListItem", clusterName)
			s.SetClusterName(clusterName)
		} else if s, ok := obj.(*unstructured.Unstructured); ok {
			klog.Infof("SUB: %s", clusterName)
			s.SetClusterName(clusterName)
		} else {
			klog.Infof("Could not set ClusterName %s in appendListItem on object: %T", clusterName, obj)
		}
	} else {
		klog.Errorf("Cluster should not be unknown")
	}

	if matched, err := pred.Matches(obj); err == nil && matched {
		v.Set(reflect.Append(v, reflect.ValueOf(obj).Elem()))
	}
	return nil
}

func notFound(key string) clientv3.Cmp {
	return clientv3.Compare(clientv3.ModRevision(key), "=", 0)
}

// getTypeName returns type name of an object for reporting purposes.
func getTypeName(obj interface{}) string {
	return reflect.TypeOf(obj).String()
}
