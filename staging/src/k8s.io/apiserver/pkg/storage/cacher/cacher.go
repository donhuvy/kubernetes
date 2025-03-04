/*
Copyright 2015 The Kubernetes Authors.

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

package cacher

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/cacher/metrics"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/tracing"

	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

var (
	emptyFunc = func(bool) {}
)

const (
	// storageWatchListPageSize is the cacher's request chunk size of
	// initial and resync watch lists to storage.
	storageWatchListPageSize = int64(10000)
	// defaultBookmarkFrequency defines how frequently watch bookmarks should be send
	// in addition to sending a bookmark right before watch deadline.
	//
	// NOTE: Update `eventFreshDuration` when changing this value.
	defaultBookmarkFrequency = time.Minute
)

// Config contains the configuration for a given Cache.
type Config struct {
	// An underlying storage.Interface.
	Storage storage.Interface

	// An underlying storage.Versioner.
	Versioner storage.Versioner

	// The GroupResource the cacher is caching. Used for disambiguating *unstructured.Unstructured (CRDs) in logging
	// and metrics.
	GroupResource schema.GroupResource

	// The Cache will be caching objects of a given Type and assumes that they
	// are all stored under ResourcePrefix directory in the underlying database.
	ResourcePrefix string

	// KeyFunc is used to get a key in the underlying storage for a given object.
	KeyFunc func(runtime.Object) (string, error)

	// GetAttrsFunc is used to get object labels, fields
	GetAttrsFunc func(runtime.Object) (label labels.Set, field fields.Set, err error)

	// IndexerFuncs is used for optimizing amount of watchers that
	// needs to process an incoming event.
	IndexerFuncs storage.IndexerFuncs

	// Indexers is used to accelerate the list operation, falls back to regular list
	// operation if no indexer found.
	Indexers *cache.Indexers

	// NewFunc is a function that creates new empty object storing a object of type Type.
	NewFunc func() runtime.Object

	// NewList is a function that creates new empty object storing a list of
	// objects of type Type.
	NewListFunc func() runtime.Object

	Codec runtime.Codec

	Clock clock.Clock
}

type watchersMap map[int]*cacheWatcher

func (wm watchersMap) addWatcher(w *cacheWatcher, number int) {
	wm[number] = w
}

func (wm watchersMap) deleteWatcher(number int, done func(*cacheWatcher)) {
	if watcher, ok := wm[number]; ok {
		delete(wm, number)
		done(watcher)
	}
}

func (wm watchersMap) terminateAll(done func(*cacheWatcher)) {
	for key, watcher := range wm {
		delete(wm, key)
		done(watcher)
	}
}

type indexedWatchers struct {
	allWatchers   map[namespacedName]watchersMap
	valueWatchers map[string]watchersMap
}

func (i *indexedWatchers) addWatcher(w *cacheWatcher, number int, scope namespacedName, value string, supported bool) {
	if supported {
		if _, ok := i.valueWatchers[value]; !ok {
			i.valueWatchers[value] = watchersMap{}
		}
		i.valueWatchers[value].addWatcher(w, number)
	} else {
		scopedWatchers, ok := i.allWatchers[scope]
		if !ok {
			scopedWatchers = watchersMap{}
			i.allWatchers[scope] = scopedWatchers
		}
		scopedWatchers.addWatcher(w, number)
	}
}

func (i *indexedWatchers) deleteWatcher(number int, scope namespacedName, value string, supported bool, done func(*cacheWatcher)) {
	if supported {
		i.valueWatchers[value].deleteWatcher(number, done)
		if len(i.valueWatchers[value]) == 0 {
			delete(i.valueWatchers, value)
		}
	} else {
		i.allWatchers[scope].deleteWatcher(number, done)
		if len(i.allWatchers[scope]) == 0 {
			delete(i.allWatchers, scope)
		}
	}
}

func (i *indexedWatchers) terminateAll(groupResource schema.GroupResource, done func(*cacheWatcher)) {
	// note that we don't have to call setDrainInputBufferLocked method on the watchers
	// because we take advantage of the default value - stop immediately
	// also watchers that have had already its draining strategy set
	// are no longer available (they were removed from the allWatchers and the valueWatchers maps)
	if len(i.allWatchers) > 0 || len(i.valueWatchers) > 0 {
		klog.Warningf("Terminating all watchers from cacher %v", groupResource)
	}
	for _, watchers := range i.allWatchers {
		watchers.terminateAll(done)
	}
	for _, watchers := range i.valueWatchers {
		watchers.terminateAll(done)
	}
	i.allWatchers = map[namespacedName]watchersMap{}
	i.valueWatchers = map[string]watchersMap{}
}

// As we don't need a high precision here, we keep all watchers timeout within a
// second in a bucket, and pop up them once at the timeout. To be more specific,
// if you set fire time at X, you can get the bookmark within (X-1,X+1) period.
type watcherBookmarkTimeBuckets struct {
	lock sync.Mutex
	// the key of watcherBuckets is the number of seconds since createTime
	watchersBuckets   map[int64][]*cacheWatcher
	createTime        time.Time
	startBucketID     int64
	clock             clock.Clock
	bookmarkFrequency time.Duration
}

func newTimeBucketWatchers(clock clock.Clock, bookmarkFrequency time.Duration) *watcherBookmarkTimeBuckets {
	return &watcherBookmarkTimeBuckets{
		watchersBuckets:   make(map[int64][]*cacheWatcher),
		createTime:        clock.Now(),
		startBucketID:     0,
		clock:             clock,
		bookmarkFrequency: bookmarkFrequency,
	}
}

// adds a watcher to the bucket, if the deadline is before the start, it will be
// added to the first one.
func (t *watcherBookmarkTimeBuckets) addWatcher(w *cacheWatcher) bool {
	// note that the returned time can be before t.createTime,
	// especially in cases when the nextBookmarkTime method
	// give us the zero value of type Time
	// so buckedID can hold a negative value
	nextTime, ok := w.nextBookmarkTime(t.clock.Now(), t.bookmarkFrequency)
	if !ok {
		return false
	}
	bucketID := int64(nextTime.Sub(t.createTime) / time.Second)
	t.lock.Lock()
	defer t.lock.Unlock()
	if bucketID < t.startBucketID {
		bucketID = t.startBucketID
	}
	watchers := t.watchersBuckets[bucketID]
	t.watchersBuckets[bucketID] = append(watchers, w)
	return true
}

func (t *watcherBookmarkTimeBuckets) popExpiredWatchers() [][]*cacheWatcher {
	currentBucketID := int64(t.clock.Since(t.createTime) / time.Second)
	// There should be one or two elements in almost all cases
	expiredWatchers := make([][]*cacheWatcher, 0, 2)
	t.lock.Lock()
	defer t.lock.Unlock()
	for ; t.startBucketID <= currentBucketID; t.startBucketID++ {
		if watchers, ok := t.watchersBuckets[t.startBucketID]; ok {
			delete(t.watchersBuckets, t.startBucketID)
			expiredWatchers = append(expiredWatchers, watchers)
		}
	}
	return expiredWatchers
}

type filterWithAttrsFunc func(key string, l labels.Set, f fields.Set) bool

type indexedTriggerFunc struct {
	indexName   string
	indexerFunc storage.IndexerFunc
}

// Cacher is responsible for serving WATCH and LIST requests for a given
// resource from its internal cache and updating its cache in the background
// based on the underlying storage contents.
// Cacher implements storage.Interface (although most of the calls are just
// delegated to the underlying storage).
type Cacher struct {
	// HighWaterMarks for performance debugging.
	// Important: Since HighWaterMark is using sync/atomic, it has to be at the top of the struct due to a bug on 32-bit platforms
	// See: https://golang.org/pkg/sync/atomic/ for more information
	incomingHWM storage.HighWaterMark
	// Incoming events that should be dispatched to watchers.
	incoming chan watchCacheEvent

	resourcePrefix string

	sync.RWMutex

	// Before accessing the cacher's cache, wait for the ready to be ok.
	// This is necessary to prevent users from accessing structures that are
	// uninitialized or are being repopulated right now.
	// ready needs to be set to false when the cacher is paused or stopped.
	// ready needs to be set to true when the cacher is ready to use after
	// initialization.
	ready *ready

	// Underlying storage.Interface.
	storage storage.Interface

	// Expected type of objects in the underlying cache.
	objectType reflect.Type
	// Used for logging, to disambiguate *unstructured.Unstructured (CRDs)
	groupResource schema.GroupResource

	// "sliding window" of recent changes of objects and the current state.
	watchCache *watchCache
	reflector  *cache.Reflector

	// Versioner is used to handle resource versions.
	versioner storage.Versioner

	// newFunc is a function that creates new empty object storing a object of type Type.
	newFunc func() runtime.Object

	// newListFunc is a function that creates new empty list for storing objects of type Type.
	newListFunc func() runtime.Object

	// indexedTrigger is used for optimizing amount of watchers that needs to process
	// an incoming event.
	indexedTrigger *indexedTriggerFunc
	// watchers is mapping from the value of trigger function that a
	// watcher is interested into the watchers
	watcherIdx int
	watchers   indexedWatchers

	// Defines a time budget that can be spend on waiting for not-ready watchers
	// while dispatching event before shutting them down.
	dispatchTimeoutBudget timeBudget

	// Handling graceful termination.
	stopLock sync.RWMutex
	stopped  bool
	stopCh   chan struct{}
	stopWg   sync.WaitGroup

	clock clock.Clock
	// timer is used to avoid unnecessary allocations in underlying watchers.
	timer *time.Timer

	// dispatching determines whether there is currently dispatching of
	// any event in flight.
	dispatching bool
	// watchersBuffer is a list of watchers potentially interested in currently
	// dispatched event.
	watchersBuffer []*cacheWatcher
	// blockedWatchers is a list of watchers whose buffer is currently full.
	blockedWatchers []*cacheWatcher
	// watchersToStop is a list of watchers that were supposed to be stopped
	// during current dispatching, but stopping was deferred to the end of
	// dispatching that event to avoid race with closing channels in watchers.
	watchersToStop []*cacheWatcher
	// Maintain a timeout queue to send the bookmark event before the watcher times out.
	bookmarkWatchers *watcherBookmarkTimeBuckets
	// expiredBookmarkWatchers is a list of watchers that were expired and need to be schedule for a next bookmark event
	expiredBookmarkWatchers []*cacheWatcher
}

// NewCacherFromConfig creates a new Cacher responsible for servicing WATCH and LIST requests from
// its internal cache and updating its cache in the background based on the
// given configuration.
func NewCacherFromConfig(config Config) (*Cacher, error) {
	stopCh := make(chan struct{})
	obj := config.NewFunc()
	// Give this error when it is constructed rather than when you get the
	// first watch item, because it's much easier to track down that way.
	if err := runtime.CheckCodec(config.Codec, obj); err != nil {
		return nil, fmt.Errorf("storage codec doesn't seem to match given type: %v", err)
	}

	var indexedTrigger *indexedTriggerFunc
	if config.IndexerFuncs != nil {
		// For now, we don't support multiple trigger functions defined
		// for a given resource.
		if len(config.IndexerFuncs) > 1 {
			return nil, fmt.Errorf("cacher %s doesn't support more than one IndexerFunc: ", reflect.TypeOf(obj).String())
		}
		for key, value := range config.IndexerFuncs {
			if value != nil {
				indexedTrigger = &indexedTriggerFunc{
					indexName:   key,
					indexerFunc: value,
				}
			}
		}
	}

	if config.Clock == nil {
		config.Clock = clock.RealClock{}
	}
	objType := reflect.TypeOf(obj)
	cacher := &Cacher{
		resourcePrefix: config.ResourcePrefix,
		ready:          newReady(),
		storage:        config.Storage,
		objectType:     objType,
		groupResource:  config.GroupResource,
		versioner:      config.Versioner,
		newFunc:        config.NewFunc,
		newListFunc:    config.NewListFunc,
		indexedTrigger: indexedTrigger,
		watcherIdx:     0,
		watchers: indexedWatchers{
			allWatchers:   make(map[namespacedName]watchersMap),
			valueWatchers: make(map[string]watchersMap),
		},
		// TODO: Figure out the correct value for the buffer size.
		incoming:              make(chan watchCacheEvent, 100),
		dispatchTimeoutBudget: newTimeBudget(),
		// We need to (potentially) stop both:
		// - wait.Until go-routine
		// - reflector.ListAndWatch
		// and there are no guarantees on the order that they will stop.
		// So we will be simply closing the channel, and synchronizing on the WaitGroup.
		stopCh:           stopCh,
		clock:            config.Clock,
		timer:            time.NewTimer(time.Duration(0)),
		bookmarkWatchers: newTimeBucketWatchers(config.Clock, defaultBookmarkFrequency),
	}

	// Ensure that timer is stopped.
	if !cacher.timer.Stop() {
		// Consume triggered (but not yet received) timer event
		// so that future reuse does not get a spurious timeout.
		<-cacher.timer.C
	}

	watchCache := newWatchCache(
		config.KeyFunc, cacher.processEvent, config.GetAttrsFunc, config.Versioner, config.Indexers, config.Clock, config.GroupResource)
	listerWatcher := NewCacherListerWatcher(config.Storage, config.ResourcePrefix, config.NewListFunc)
	reflectorName := "storage/cacher.go:" + config.ResourcePrefix

	reflector := cache.NewNamedReflector(reflectorName, listerWatcher, obj, watchCache, 0)
	// Configure reflector's pager to for an appropriate pagination chunk size for fetching data from
	// storage. The pager falls back to full list if paginated list calls fail due to an "Expired" error.
	reflector.WatchListPageSize = storageWatchListPageSize
	// When etcd loses leader for 3 cycles, it returns error "no leader".
	// We don't want to terminate all watchers as recreating all watchers puts high load on api-server.
	// In most of the cases, leader is reelected within few cycles.
	reflector.MaxInternalErrorRetryDuration = time.Second * 30
	// since the watch-list is provided by the watch cache instruct
	// the reflector to issue a regular LIST against the store
	reflector.UseWatchList = false

	cacher.watchCache = watchCache
	cacher.reflector = reflector

	go cacher.dispatchEvents()

	cacher.stopWg.Add(1)
	go func() {
		defer cacher.stopWg.Done()
		defer cacher.terminateAllWatchers()
		wait.Until(
			func() {
				if !cacher.isStopped() {
					cacher.startCaching(stopCh)
				}
			}, time.Second, stopCh,
		)
	}()

	return cacher, nil
}

func (c *Cacher) startCaching(stopChannel <-chan struct{}) {
	// The 'usable' lock is always 'RLock'able when it is safe to use the cache.
	// It is safe to use the cache after a successful list until a disconnection.
	// We start with usable (write) locked. The below OnReplace function will
	// unlock it after a successful list. The below defer will then re-lock
	// it when this function exits (always due to disconnection), only if
	// we actually got a successful list. This cycle will repeat as needed.
	successfulList := false
	c.watchCache.SetOnReplace(func() {
		successfulList = true
		c.ready.set(true)
		klog.V(1).Infof("cacher (%v): initialized", c.groupResource.String())
		metrics.WatchCacheInitializations.WithLabelValues(c.groupResource.String()).Inc()
	})
	defer func() {
		if successfulList {
			c.ready.set(false)
		}
	}()

	c.terminateAllWatchers()
	// Note that since onReplace may be not called due to errors, we explicitly
	// need to retry it on errors under lock.
	// Also note that startCaching is called in a loop, so there's no need
	// to have another loop here.
	if err := c.reflector.ListAndWatch(stopChannel); err != nil {
		klog.Errorf("cacher (%v): unexpected ListAndWatch error: %v; reinitializing...", c.groupResource.String(), err)
	}
}

// Versioner implements storage.Interface.
func (c *Cacher) Versioner() storage.Versioner {
	return c.storage.Versioner()
}

// Create implements storage.Interface.
func (c *Cacher) Create(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error {
	return c.storage.Create(ctx, key, obj, out, ttl)
}

// Delete implements storage.Interface.
func (c *Cacher) Delete(
	ctx context.Context, key string, out runtime.Object, preconditions *storage.Preconditions,
	validateDeletion storage.ValidateObjectFunc, _ runtime.Object) error {
	// Ignore the suggestion and try to pass down the current version of the object
	// read from cache.
	if elem, exists, err := c.watchCache.GetByKey(key); err != nil {
		klog.Errorf("GetByKey returned error: %v", err)
	} else if exists {
		// DeepCopy the object since we modify resource version when serializing the
		// current object.
		currObj := elem.(*storeElement).Object.DeepCopyObject()
		return c.storage.Delete(ctx, key, out, preconditions, validateDeletion, currObj)
	}
	// If we couldn't get the object, fallback to no-suggestion.
	return c.storage.Delete(ctx, key, out, preconditions, validateDeletion, nil)
}

type namespacedName struct {
	namespace string
	name      string
}

// Watch implements storage.Interface.
func (c *Cacher) Watch(ctx context.Context, key string, opts storage.ListOptions) (watch.Interface, error) {
	pred := opts.Predicate
	// if the watch-list feature wasn't set and the resourceVersion is unset
	// ensure that the rv from which the watch is being served, is the latest
	// one. "latest" is ensured by serving the watch from
	// the underlying storage.
	//
	// it should never happen due to our validation but let's just be super-safe here
	// and disable sendingInitialEvents when the feature wasn't enabled
	if !utilfeature.DefaultFeatureGate.Enabled(features.WatchList) && opts.SendInitialEvents != nil {
		opts.SendInitialEvents = nil
	}
	if opts.SendInitialEvents == nil && opts.ResourceVersion == "" {
		return c.storage.Watch(ctx, key, opts)
	}
	requestedWatchRV, err := c.versioner.ParseResourceVersion(opts.ResourceVersion)
	if err != nil {
		return nil, err
	}

	readyGeneration, err := c.ready.waitAndReadGeneration(ctx)
	if err != nil {
		return nil, errors.NewServiceUnavailable(err.Error())
	}

	// determine the namespace and name scope of the watch, first from the request, secondarily from the field selector
	scope := namespacedName{}
	if requestNamespace, ok := request.NamespaceFrom(ctx); ok && len(requestNamespace) > 0 {
		scope.namespace = requestNamespace
	} else if selectorNamespace, ok := pred.Field.RequiresExactMatch("metadata.namespace"); ok {
		scope.namespace = selectorNamespace
	}
	if requestInfo, ok := request.RequestInfoFrom(ctx); ok && requestInfo != nil && len(requestInfo.Name) > 0 {
		scope.name = requestInfo.Name
	} else if selectorName, ok := pred.Field.RequiresExactMatch("metadata.name"); ok {
		scope.name = selectorName
	}

	triggerValue, triggerSupported := "", false
	if c.indexedTrigger != nil {
		for _, field := range pred.IndexFields {
			if field == c.indexedTrigger.indexName {
				if value, ok := pred.Field.RequiresExactMatch(field); ok {
					triggerValue, triggerSupported = value, true
				}
			}
		}
	}

	// It boils down to a tradeoff between:
	// - having it as small as possible to reduce memory usage
	// - having it large enough to ensure that watchers that need to process
	//   a bunch of changes have enough buffer to avoid from blocking other
	//   watchers on our watcher having a processing hiccup
	chanSize := c.watchCache.suggestedWatchChannelSize(c.indexedTrigger != nil, triggerSupported)

	// Determine a function that computes the bookmarkAfterResourceVersion
	bookmarkAfterResourceVersionFn, err := c.getBookmarkAfterResourceVersionLockedFunc(ctx, requestedWatchRV, opts)
	if err != nil {
		return newErrWatcher(err), nil
	}

	// Determine a function that computes the watchRV we should start from
	startWatchResourceVersionFn, err := c.getStartResourceVersionForWatchLockedFunc(ctx, requestedWatchRV, opts)
	if err != nil {
		return newErrWatcher(err), nil
	}

	// Determine watch timeout('0' means deadline is not set, ignore checking)
	deadline, _ := ctx.Deadline()

	identifier := fmt.Sprintf("key: %q, labels: %q, fields: %q", key, pred.Label, pred.Field)

	// Create a watcher here to reduce memory allocations under lock,
	// given that memory allocation may trigger GC and block the thread.
	// Also note that emptyFunc is a placeholder, until we will be able
	// to compute watcher.forget function (which has to happen under lock).
	watcher := newCacheWatcher(
		chanSize,
		filterWithAttrsFunction(key, pred),
		emptyFunc,
		c.versioner,
		deadline,
		pred.AllowWatchBookmarks,
		c.groupResource,
		identifier,
	)

	// We explicitly use thread unsafe version and do locking ourself to ensure that
	// no new events will be processed in the meantime. The watchCache will be unlocked
	// on return from this function.
	// Note that we cannot do it under Cacher lock, to avoid a deadlock, since the
	// underlying watchCache is calling processEvent under its lock.
	c.watchCache.RLock()
	defer c.watchCache.RUnlock()
	forceAllEvents, err := c.waitUntilWatchCacheFreshAndForceAllEvents(ctx, requestedWatchRV, opts)
	if err != nil {
		return newErrWatcher(err), nil
	}
	startWatchRV := startWatchResourceVersionFn()
	var cacheInterval *watchCacheInterval
	if forceAllEvents {
		cacheInterval, err = c.watchCache.getIntervalFromStoreLocked()
	} else {
		cacheInterval, err = c.watchCache.getAllEventsSinceLocked(startWatchRV)
	}
	if err != nil {
		// To match the uncached watch implementation, once we have passed authn/authz/admission,
		// and successfully parsed a resource version, other errors must fail with a watch event of type ERROR,
		// rather than a directly returned error.
		return newErrWatcher(err), nil
	}

	addedWatcher := false
	func() {
		c.Lock()
		defer c.Unlock()

		if generation, ok := c.ready.checkAndReadGeneration(); generation != readyGeneration || !ok {
			// We went unready or are already on a different generation.
			// Avoid registering and starting the watch as it will have to be
			// terminated immediately anyway.
			return
		}

		// Update watcher.forget function once we can compute it.
		watcher.forget = forgetWatcher(c, watcher, c.watcherIdx, scope, triggerValue, triggerSupported)
		// Update the bookMarkAfterResourceVersion
		watcher.setBookmarkAfterResourceVersion(bookmarkAfterResourceVersionFn())
		c.watchers.addWatcher(watcher, c.watcherIdx, scope, triggerValue, triggerSupported)
		addedWatcher = true

		// Add it to the queue only when the client support watch bookmarks.
		if watcher.allowWatchBookmarks {
			c.bookmarkWatchers.addWatcher(watcher)
		}
		c.watcherIdx++
	}()

	if !addedWatcher {
		// Watcher isn't really started at this point, so it's safe to just drop it.
		//
		// We're simulating the immediate watch termination, which boils down to simply
		// closing the watcher.
		return newImmediateCloseWatcher(), nil
	}

	go watcher.processInterval(ctx, cacheInterval, startWatchRV)
	return watcher, nil
}

// Get implements storage.Interface.
func (c *Cacher) Get(ctx context.Context, key string, opts storage.GetOptions, objPtr runtime.Object) error {
	if opts.ResourceVersion == "" {
		// If resourceVersion is not specified, serve it from underlying
		// storage (for backward compatibility).
		return c.storage.Get(ctx, key, opts, objPtr)
	}

	// If resourceVersion is specified, serve it from cache.
	// It's guaranteed that the returned value is at least that
	// fresh as the given resourceVersion.
	getRV, err := c.versioner.ParseResourceVersion(opts.ResourceVersion)
	if err != nil {
		return err
	}

	if getRV == 0 && !c.ready.check() {
		// If Cacher is not yet initialized and we don't require any specific
		// minimal resource version, simply forward the request to storage.
		return c.storage.Get(ctx, key, opts, objPtr)
	}

	// Do not create a trace - it's not for free and there are tons
	// of Get requests. We can add it if it will be really needed.
	if err := c.ready.wait(ctx); err != nil {
		return errors.NewServiceUnavailable(err.Error())
	}

	objVal, err := conversion.EnforcePtr(objPtr)
	if err != nil {
		return err
	}

	obj, exists, readResourceVersion, err := c.watchCache.WaitUntilFreshAndGet(ctx, getRV, key)
	if err != nil {
		return err
	}

	if exists {
		elem, ok := obj.(*storeElement)
		if !ok {
			return fmt.Errorf("non *storeElement returned from storage: %v", obj)
		}
		objVal.Set(reflect.ValueOf(elem.Object).Elem())
	} else {
		objVal.Set(reflect.Zero(objVal.Type()))
		if !opts.IgnoreNotFound {
			return storage.NewKeyNotFoundError(key, int64(readResourceVersion))
		}
	}
	return nil
}

// NOTICE: Keep in sync with shouldListFromStorage function in
//
//	staging/src/k8s.io/apiserver/pkg/util/flowcontrol/request/list_work_estimator.go
func shouldDelegateList(opts storage.ListOptions) bool {
	resourceVersion := opts.ResourceVersion
	pred := opts.Predicate
	match := opts.ResourceVersionMatch
	pagingEnabled := utilfeature.DefaultFeatureGate.Enabled(features.APIListChunking)
	hasContinuation := pagingEnabled && len(pred.Continue) > 0
	hasLimit := pagingEnabled && pred.Limit > 0 && resourceVersion != "0"
	unsupportedMatch := match != "" && match != metav1.ResourceVersionMatchNotOlderThan

	// If resourceVersion is not specified, serve it from underlying
	// storage (for backward compatibility). If a continuation is
	// requested, serve it from the underlying storage as well.
	// Limits are only sent to storage when resourceVersion is non-zero
	// since the watch cache isn't able to perform continuations, and
	// limits are ignored when resource version is zero
	return resourceVersion == "" || hasContinuation || hasLimit || unsupportedMatch
}

func (c *Cacher) listItems(ctx context.Context, listRV uint64, key string, pred storage.SelectionPredicate, recursive bool) ([]interface{}, uint64, string, error) {
	if !recursive {
		obj, exists, readResourceVersion, err := c.watchCache.WaitUntilFreshAndGet(ctx, listRV, key)
		if err != nil {
			return nil, 0, "", err
		}
		if exists {
			return []interface{}{obj}, readResourceVersion, "", nil
		}
		return nil, readResourceVersion, "", nil
	}
	return c.watchCache.WaitUntilFreshAndList(ctx, listRV, pred.MatcherIndex())
}

// GetList implements storage.Interface
func (c *Cacher) GetList(ctx context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
	recursive := opts.Recursive
	resourceVersion := opts.ResourceVersion
	pred := opts.Predicate
	if shouldDelegateList(opts) {
		return c.storage.GetList(ctx, key, opts, listObj)
	}

	// If resourceVersion is specified, serve it from cache.
	// It's guaranteed that the returned value is at least that
	// fresh as the given resourceVersion.
	listRV, err := c.versioner.ParseResourceVersion(resourceVersion)
	if err != nil {
		return err
	}

	if listRV == 0 && !c.ready.check() {
		// If Cacher is not yet initialized and we don't require any specific
		// minimal resource version, simply forward the request to storage.
		return c.storage.GetList(ctx, key, opts, listObj)
	}

	ctx, span := tracing.Start(ctx, "cacher list",
		attribute.String("audit-id", audit.GetAuditIDTruncated(ctx)),
		attribute.Stringer("type", c.groupResource))
	defer span.End(500 * time.Millisecond)

	if err := c.ready.wait(ctx); err != nil {
		return errors.NewServiceUnavailable(err.Error())
	}
	span.AddEvent("Ready")

	// List elements with at least 'listRV' from cache.
	listPtr, err := meta.GetItemsPtr(listObj)
	if err != nil {
		return err
	}
	listVal, err := conversion.EnforcePtr(listPtr)
	if err != nil {
		return err
	}
	if listVal.Kind() != reflect.Slice {
		return fmt.Errorf("need a pointer to slice, got %v", listVal.Kind())
	}
	filter := filterWithAttrsFunction(key, pred)

	objs, readResourceVersion, indexUsed, err := c.listItems(ctx, listRV, key, pred, recursive)
	if err != nil {
		return err
	}
	span.AddEvent("Listed items from cache", attribute.Int("count", len(objs)))
	// store pointer of eligible objects,
	// Why not directly put object in the items of listObj?
	//   the elements in ListObject are Struct type, making slice will bring excessive memory consumption.
	//   so we try to delay this action as much as possible
	var selectedObjects []runtime.Object
	for _, obj := range objs {
		elem, ok := obj.(*storeElement)
		if !ok {
			return fmt.Errorf("non *storeElement returned from storage: %v", obj)
		}
		if filter(elem.Key, elem.Labels, elem.Fields) {
			selectedObjects = append(selectedObjects, elem.Object)
		}
	}
	if len(selectedObjects) == 0 {
		// Ensure that we never return a nil Items pointer in the result for consistency.
		listVal.Set(reflect.MakeSlice(listVal.Type(), 0, 0))
	} else {
		// Resize the slice appropriately, since we already know that size of result set
		listVal.Set(reflect.MakeSlice(listVal.Type(), len(selectedObjects), len(selectedObjects)))
		span.AddEvent("Resized result")
		for i, o := range selectedObjects {
			listVal.Index(i).Set(reflect.ValueOf(o).Elem())
		}
	}
	span.AddEvent("Filtered items", attribute.Int("count", listVal.Len()))
	if c.versioner != nil {
		if err := c.versioner.UpdateList(listObj, readResourceVersion, "", nil); err != nil {
			return err
		}
	}
	metrics.RecordListCacheMetrics(c.resourcePrefix, indexUsed, len(objs), listVal.Len())
	return nil
}

// GuaranteedUpdate implements storage.Interface.
func (c *Cacher) GuaranteedUpdate(
	ctx context.Context, key string, destination runtime.Object, ignoreNotFound bool,
	preconditions *storage.Preconditions, tryUpdate storage.UpdateFunc, _ runtime.Object) error {
	// Ignore the suggestion and try to pass down the current version of the object
	// read from cache.
	if elem, exists, err := c.watchCache.GetByKey(key); err != nil {
		klog.Errorf("GetByKey returned error: %v", err)
	} else if exists {
		// DeepCopy the object since we modify resource version when serializing the
		// current object.
		currObj := elem.(*storeElement).Object.DeepCopyObject()
		return c.storage.GuaranteedUpdate(ctx, key, destination, ignoreNotFound, preconditions, tryUpdate, currObj)
	}
	// If we couldn't get the object, fallback to no-suggestion.
	return c.storage.GuaranteedUpdate(ctx, key, destination, ignoreNotFound, preconditions, tryUpdate, nil)
}

// Count implements storage.Interface.
func (c *Cacher) Count(pathPrefix string) (int64, error) {
	return c.storage.Count(pathPrefix)
}

// baseObjectThreadUnsafe omits locking for cachingObject.
func baseObjectThreadUnsafe(object runtime.Object) runtime.Object {
	if co, ok := object.(*cachingObject); ok {
		return co.object
	}
	return object
}

func (c *Cacher) triggerValuesThreadUnsafe(event *watchCacheEvent) ([]string, bool) {
	if c.indexedTrigger == nil {
		return nil, false
	}

	result := make([]string, 0, 2)
	result = append(result, c.indexedTrigger.indexerFunc(baseObjectThreadUnsafe(event.Object)))
	if event.PrevObject == nil {
		return result, true
	}
	prevTriggerValue := c.indexedTrigger.indexerFunc(baseObjectThreadUnsafe(event.PrevObject))
	if result[0] != prevTriggerValue {
		result = append(result, prevTriggerValue)
	}
	return result, true
}

func (c *Cacher) processEvent(event *watchCacheEvent) {
	if curLen := int64(len(c.incoming)); c.incomingHWM.Update(curLen) {
		// Monitor if this gets backed up, and how much.
		klog.V(1).Infof("cacher (%v): %v objects queued in incoming channel.", c.groupResource.String(), curLen)
	}
	c.incoming <- *event
}

func (c *Cacher) dispatchEvents() {
	// Jitter to help level out any aggregate load.
	bookmarkTimer := c.clock.NewTimer(wait.Jitter(time.Second, 0.25))
	defer bookmarkTimer.Stop()

	lastProcessedResourceVersion := uint64(0)
	for {
		select {
		case event, ok := <-c.incoming:
			if !ok {
				return
			}
			// Don't dispatch bookmarks coming from the storage layer.
			// They can be very frequent (even to the level of subseconds)
			// to allow efficient watch resumption on kube-apiserver restarts,
			// and propagating them down may overload the whole system.
			//
			// TODO: If at some point we decide the performance and scalability
			// footprint is acceptable, this is the place to hook them in.
			// However, we then need to check if this was called as a result
			// of a bookmark event or regular Add/Update/Delete operation by
			// checking if resourceVersion here has changed.
			if event.Type != watch.Bookmark {
				c.dispatchEvent(&event)
			}
			lastProcessedResourceVersion = event.ResourceVersion
			metrics.EventsCounter.WithLabelValues(c.groupResource.String()).Inc()
		case <-bookmarkTimer.C():
			bookmarkTimer.Reset(wait.Jitter(time.Second, 0.25))
			// Never send a bookmark event if we did not see an event here, this is fine
			// because we don't provide any guarantees on sending bookmarks.
			if lastProcessedResourceVersion == 0 {
				// pop expired watchers in case there has been no update
				c.bookmarkWatchers.popExpiredWatchers()
				continue
			}
			bookmarkEvent := &watchCacheEvent{
				Type:            watch.Bookmark,
				Object:          c.newFunc(),
				ResourceVersion: lastProcessedResourceVersion,
			}
			if err := c.versioner.UpdateObject(bookmarkEvent.Object, bookmarkEvent.ResourceVersion); err != nil {
				klog.Errorf("failure to set resourceVersion to %d on bookmark event %+v", bookmarkEvent.ResourceVersion, bookmarkEvent.Object)
				continue
			}
			c.dispatchEvent(bookmarkEvent)
		case <-c.stopCh:
			return
		}
	}
}

func setCachingObjects(event *watchCacheEvent, versioner storage.Versioner) {
	switch event.Type {
	case watch.Added, watch.Modified:
		if object, err := newCachingObject(event.Object); err == nil {
			event.Object = object
		} else {
			klog.Errorf("couldn't create cachingObject from: %#v", event.Object)
		}
		// Don't wrap PrevObject for update event (for create events it is nil).
		// We only encode those to deliver DELETE watch events, so if
		// event.Object is not nil it can be used only for watchers for which
		// selector was satisfied for its previous version and is no longer
		// satisfied for the current version.
		// This is rare enough that it doesn't justify making deep-copy of the
		// object (done by newCachingObject) every time.
	case watch.Deleted:
		// Don't wrap Object for delete events - these are not to deliver any
		// events. Only wrap PrevObject.
		if object, err := newCachingObject(event.PrevObject); err == nil {
			// Update resource version of the object.
			// event.PrevObject is used to deliver DELETE watch events and
			// for them, we set resourceVersion to <current> instead of
			// the resourceVersion of the last modification of the object.
			updateResourceVersion(object, versioner, event.ResourceVersion)
			event.PrevObject = object
		} else {
			klog.Errorf("couldn't create cachingObject from: %#v", event.Object)
		}
	}
}

func (c *Cacher) dispatchEvent(event *watchCacheEvent) {
	c.startDispatching(event)
	defer c.finishDispatching()
	// Watchers stopped after startDispatching will be delayed to finishDispatching,

	// Since add() can block, we explicitly add when cacher is unlocked.
	// Dispatching event in nonblocking way first, which make faster watchers
	// not be blocked by slower ones.
	if event.Type == watch.Bookmark {
		for _, watcher := range c.watchersBuffer {
			watcher.nonblockingAdd(event)
		}
	} else {
		// Set up caching of object serializations only for dispatching this event.
		//
		// Storing serializations in memory would result in increased memory usage,
		// but it would help for caching encodings for watches started from old
		// versions. However, we still don't have a convincing data that the gain
		// from it justifies increased memory usage, so for now we drop the cached
		// serializations after dispatching this event.
		//
		// Given that CachingObject is just wrapping the object and not perfoming
		// deep-copying (until some field is explicitly being modified), we create
		// it unconditionally to ensure safety and reduce deep-copying.
		//
		// Make a shallow copy to allow overwriting Object and PrevObject.
		wcEvent := *event
		setCachingObjects(&wcEvent, c.versioner)
		event = &wcEvent

		c.blockedWatchers = c.blockedWatchers[:0]
		for _, watcher := range c.watchersBuffer {
			if !watcher.nonblockingAdd(event) {
				c.blockedWatchers = append(c.blockedWatchers, watcher)
			}
		}

		if len(c.blockedWatchers) > 0 {
			// dispatchEvent is called very often, so arrange
			// to reuse timers instead of constantly allocating.
			startTime := time.Now()
			timeout := c.dispatchTimeoutBudget.takeAvailable()
			c.timer.Reset(timeout)

			// Send event to all blocked watchers. As long as timer is running,
			// `add` will wait for the watcher to unblock. After timeout,
			// `add` will not wait, but immediately close a still blocked watcher.
			// Hence, every watcher gets the chance to unblock itself while timer
			// is running, not only the first ones in the list.
			timer := c.timer
			for _, watcher := range c.blockedWatchers {
				if !watcher.add(event, timer) {
					// fired, clean the timer by set it to nil.
					timer = nil
				}
			}

			// Stop the timer if it is not fired
			if timer != nil && !timer.Stop() {
				// Consume triggered (but not yet received) timer event
				// so that future reuse does not get a spurious timeout.
				<-timer.C
			}

			c.dispatchTimeoutBudget.returnUnused(timeout - time.Since(startTime))
		}
	}
}

func (c *Cacher) startDispatchingBookmarkEventsLocked() {
	// Pop already expired watchers. However, explicitly ignore stopped ones,
	// as we don't delete watcher from bookmarkWatchers when it is stopped.
	for _, watchers := range c.bookmarkWatchers.popExpiredWatchers() {
		for _, watcher := range watchers {
			// c.Lock() is held here.
			// watcher.stopThreadUnsafe() is protected by c.Lock()
			if watcher.stopped {
				continue
			}
			c.watchersBuffer = append(c.watchersBuffer, watcher)
			c.expiredBookmarkWatchers = append(c.expiredBookmarkWatchers, watcher)
		}
	}
}

// startDispatching chooses watchers potentially interested in a given event
// a marks dispatching as true.
func (c *Cacher) startDispatching(event *watchCacheEvent) {
	// It is safe to call triggerValuesThreadUnsafe here, because at this
	// point only this thread can access this event (we create a separate
	// watchCacheEvent for every dispatch).
	triggerValues, supported := c.triggerValuesThreadUnsafe(event)

	c.Lock()
	defer c.Unlock()

	c.dispatching = true
	// We are reusing the slice to avoid memory reallocations in every
	// dispatchEvent() call. That may prevent Go GC from freeing items
	// from previous phases that are sitting behind the current length
	// of the slice, but there is only a limited number of those and the
	// gain from avoiding memory allocations is much bigger.
	c.watchersBuffer = c.watchersBuffer[:0]

	if event.Type == watch.Bookmark {
		c.startDispatchingBookmarkEventsLocked()
		// return here to reduce following code indentation and diff
		return
	}

	// iterate over watchers for each applicable namespace/name tuple
	namespace := event.ObjFields["metadata.namespace"]
	name := event.ObjFields["metadata.name"]
	if len(namespace) > 0 {
		if len(name) > 0 {
			// namespaced watchers scoped by name
			for _, watcher := range c.watchers.allWatchers[namespacedName{namespace: namespace, name: name}] {
				c.watchersBuffer = append(c.watchersBuffer, watcher)
			}
		}
		// namespaced watchers not scoped by name
		for _, watcher := range c.watchers.allWatchers[namespacedName{namespace: namespace}] {
			c.watchersBuffer = append(c.watchersBuffer, watcher)
		}
	}
	if len(name) > 0 {
		// cluster-wide watchers scoped by name
		for _, watcher := range c.watchers.allWatchers[namespacedName{name: name}] {
			c.watchersBuffer = append(c.watchersBuffer, watcher)
		}
	}
	// cluster-wide watchers unscoped by name
	for _, watcher := range c.watchers.allWatchers[namespacedName{}] {
		c.watchersBuffer = append(c.watchersBuffer, watcher)
	}

	if supported {
		// Iterate over watchers interested in the given values of the trigger.
		for _, triggerValue := range triggerValues {
			for _, watcher := range c.watchers.valueWatchers[triggerValue] {
				c.watchersBuffer = append(c.watchersBuffer, watcher)
			}
		}
	} else {
		// supported equal to false generally means that trigger function
		// is not defined (or not aware of any indexes). In this case,
		// watchers filters should generally also don't generate any
		// trigger values, but can cause problems in case of some
		// misconfiguration. Thus we paranoidly leave this branch.

		// Iterate over watchers interested in exact values for all values.
		for _, watchers := range c.watchers.valueWatchers {
			for _, watcher := range watchers {
				c.watchersBuffer = append(c.watchersBuffer, watcher)
			}
		}
	}
}

// finishDispatching stops all the watchers that were supposed to be
// stopped in the meantime, but it was deferred to avoid closing input
// channels of watchers, as add() may still have writing to it.
// It also marks dispatching as false.
func (c *Cacher) finishDispatching() {
	c.Lock()
	defer c.Unlock()
	c.dispatching = false
	for _, watcher := range c.watchersToStop {
		watcher.stopLocked()
	}
	c.watchersToStop = c.watchersToStop[:0]

	for _, watcher := range c.expiredBookmarkWatchers {
		if watcher.stopped {
			continue
		}
		// requeue the watcher for the next bookmark if needed.
		c.bookmarkWatchers.addWatcher(watcher)
	}
	c.expiredBookmarkWatchers = c.expiredBookmarkWatchers[:0]
}

func (c *Cacher) terminateAllWatchers() {
	c.Lock()
	defer c.Unlock()
	c.watchers.terminateAll(c.groupResource, c.stopWatcherLocked)
}

func (c *Cacher) stopWatcherLocked(watcher *cacheWatcher) {
	if c.dispatching {
		c.watchersToStop = append(c.watchersToStop, watcher)
	} else {
		watcher.stopLocked()
	}
}

func (c *Cacher) isStopped() bool {
	c.stopLock.RLock()
	defer c.stopLock.RUnlock()
	return c.stopped
}

// Stop implements the graceful termination.
func (c *Cacher) Stop() {
	c.stopLock.Lock()
	if c.stopped {
		// avoid stopping twice (note: cachers are shared with subresources)
		c.stopLock.Unlock()
		return
	}
	c.stopped = true
	c.ready.stop()
	c.stopLock.Unlock()
	close(c.stopCh)
	c.stopWg.Wait()
}

func forgetWatcher(c *Cacher, w *cacheWatcher, index int, scope namespacedName, triggerValue string, triggerSupported bool) func(bool) {
	return func(drainWatcher bool) {
		c.Lock()
		defer c.Unlock()

		w.setDrainInputBufferLocked(drainWatcher)

		// It's possible that the watcher is already not in the structure (e.g. in case of
		// simultaneous Stop() and terminateAllWatchers(), but it is safe to call stopLocked()
		// on a watcher multiple times.
		c.watchers.deleteWatcher(index, scope, triggerValue, triggerSupported, c.stopWatcherLocked)
	}
}

func filterWithAttrsFunction(key string, p storage.SelectionPredicate) filterWithAttrsFunc {
	filterFunc := func(objKey string, label labels.Set, field fields.Set) bool {
		if !hasPathPrefix(objKey, key) {
			return false
		}
		return p.MatchesObjectAttributes(label, field)
	}
	return filterFunc
}

// LastSyncResourceVersion returns resource version to which the underlying cache is synced.
func (c *Cacher) LastSyncResourceVersion() (uint64, error) {
	if err := c.ready.wait(context.Background()); err != nil {
		return 0, errors.NewServiceUnavailable(err.Error())
	}

	resourceVersion := c.reflector.LastSyncResourceVersion()
	return c.versioner.ParseResourceVersion(resourceVersion)
}

// getCurrentResourceVersionFromStorage gets the current resource version from the underlying storage engine.
// this method issues an empty list request and reads only the ResourceVersion from the object metadata
func (c *Cacher) getCurrentResourceVersionFromStorage(ctx context.Context) (uint64, error) {
	if c.newListFunc == nil {
		return 0, fmt.Errorf("newListFunction wasn't provided for %v", c.objectType)
	}
	emptyList := c.newListFunc()
	pred := storage.SelectionPredicate{
		Label: labels.Everything(),
		Field: fields.Everything(),
		Limit: 1, // just in case we actually hit something
	}

	err := c.storage.GetList(ctx, c.resourcePrefix, storage.ListOptions{Predicate: pred}, emptyList)
	if err != nil {
		return 0, err
	}
	emptyListAccessor, err := meta.ListAccessor(emptyList)
	if err != nil {
		return 0, err
	}
	if emptyListAccessor == nil {
		return 0, fmt.Errorf("unable to extract a list accessor from %T", emptyList)
	}

	currentResourceVersion, err := strconv.Atoi(emptyListAccessor.GetResourceVersion())
	if err != nil {
		return 0, err
	}

	if currentResourceVersion == 0 {
		return 0, fmt.Errorf("the current resource version must be greater than 0")
	}
	return uint64(currentResourceVersion), nil
}

// getBookmarkAfterResourceVersionLockedFunc returns a function that
// spits a ResourceVersion after which the bookmark event will be delivered.
//
// The returned function must be called under the watchCache lock.
func (c *Cacher) getBookmarkAfterResourceVersionLockedFunc(ctx context.Context, parsedResourceVersion uint64, opts storage.ListOptions) (func() uint64, error) {
	if opts.SendInitialEvents == nil || *opts.SendInitialEvents == false || !opts.Predicate.AllowWatchBookmarks {
		return func() uint64 { return 0 }, nil
	}
	return c.getCommonResourceVersionLockedFunc(ctx, parsedResourceVersion, opts)
}

// getStartResourceVersionForWatchLockedFunc returns a function that
// spits a ResourceVersion the watch will be started from.
// Depending on the input parameters the semantics of the returned ResourceVersion are:
//   - start at Exact (return parsedWatchResourceVersion)
//   - start at Most Recent (return an RV from etcd)
//   - start at Any (return the current watchCache's RV)
//
// The returned function must be called under the watchCache lock.
func (c *Cacher) getStartResourceVersionForWatchLockedFunc(ctx context.Context, parsedWatchResourceVersion uint64, opts storage.ListOptions) (func() uint64, error) {
	if opts.SendInitialEvents == nil || *opts.SendInitialEvents == true {
		return func() uint64 { return parsedWatchResourceVersion }, nil
	}
	return c.getCommonResourceVersionLockedFunc(ctx, parsedWatchResourceVersion, opts)
}

// getCommonResourceVersionLockedFunc a helper that simply computes a ResourceVersion
// based on the input parameters. Please examine callers of this method to get more context.
//
// The returned function must be called under the watchCache lock.
func (c *Cacher) getCommonResourceVersionLockedFunc(ctx context.Context, parsedWatchResourceVersion uint64, opts storage.ListOptions) (func() uint64, error) {
	switch {
	case len(opts.ResourceVersion) == 0:
		rv, err := c.getCurrentResourceVersionFromStorage(ctx)
		if err != nil {
			return nil, err
		}
		return func() uint64 { return rv }, nil
	case parsedWatchResourceVersion == 0:
		// here we assume that watchCache locked is already held
		return func() uint64 { return c.watchCache.resourceVersion }, nil
	default:
		return func() uint64 { return parsedWatchResourceVersion }, nil
	}
}

// waitUntilWatchCacheFreshAndForceAllEvents waits until cache is at least
// as fresh as given requestedWatchRV if sendInitialEvents was requested.
// Additionally, it instructs the caller whether it should ask for
// all events from the cache (full state) or not.
func (c *Cacher) waitUntilWatchCacheFreshAndForceAllEvents(ctx context.Context, requestedWatchRV uint64, opts storage.ListOptions) (bool, error) {
	if opts.SendInitialEvents != nil && *opts.SendInitialEvents == true {
		err := c.watchCache.waitUntilFreshAndBlock(ctx, requestedWatchRV)
		defer c.watchCache.RUnlock()
		return err == nil, err
	}
	return false, nil
}

// cacherListerWatcher opaques storage.Interface to expose cache.ListerWatcher.
type cacherListerWatcher struct {
	storage        storage.Interface
	resourcePrefix string
	newListFunc    func() runtime.Object
}

// NewCacherListerWatcher returns a storage.Interface backed ListerWatcher.
func NewCacherListerWatcher(storage storage.Interface, resourcePrefix string, newListFunc func() runtime.Object) cache.ListerWatcher {
	return &cacherListerWatcher{
		storage:        storage,
		resourcePrefix: resourcePrefix,
		newListFunc:    newListFunc,
	}
}

// Implements cache.ListerWatcher interface.
func (lw *cacherListerWatcher) List(options metav1.ListOptions) (runtime.Object, error) {
	list := lw.newListFunc()
	pred := storage.SelectionPredicate{
		Label:    labels.Everything(),
		Field:    fields.Everything(),
		Limit:    options.Limit,
		Continue: options.Continue,
	}

	storageOpts := storage.ListOptions{
		ResourceVersionMatch: options.ResourceVersionMatch,
		Predicate:            pred,
		Recursive:            true,
	}
	if err := lw.storage.GetList(context.TODO(), lw.resourcePrefix, storageOpts, list); err != nil {
		return nil, err
	}
	return list, nil
}

// Implements cache.ListerWatcher interface.
func (lw *cacherListerWatcher) Watch(options metav1.ListOptions) (watch.Interface, error) {
	opts := storage.ListOptions{
		ResourceVersion: options.ResourceVersion,
		Predicate:       storage.Everything,
		Recursive:       true,
		ProgressNotify:  true,
	}
	return lw.storage.Watch(context.TODO(), lw.resourcePrefix, opts)
}

// errWatcher implements watch.Interface to return a single error
type errWatcher struct {
	result chan watch.Event
}

func newErrWatcher(err error) *errWatcher {
	// Create an error event
	errEvent := watch.Event{Type: watch.Error}
	switch err := err.(type) {
	case runtime.Object:
		errEvent.Object = err
	case *errors.StatusError:
		errEvent.Object = &err.ErrStatus
	default:
		errEvent.Object = &metav1.Status{
			Status:  metav1.StatusFailure,
			Message: err.Error(),
			Reason:  metav1.StatusReasonInternalError,
			Code:    http.StatusInternalServerError,
		}
	}

	// Create a watcher with room for a single event, populate it, and close the channel
	watcher := &errWatcher{result: make(chan watch.Event, 1)}
	watcher.result <- errEvent
	close(watcher.result)

	return watcher
}

// Implements watch.Interface.
func (c *errWatcher) ResultChan() <-chan watch.Event {
	return c.result
}

// Implements watch.Interface.
func (c *errWatcher) Stop() {
	// no-op
}

// immediateCloseWatcher implements watch.Interface that is immediately closed
type immediateCloseWatcher struct {
	result chan watch.Event
}

func newImmediateCloseWatcher() *immediateCloseWatcher {
	watcher := &immediateCloseWatcher{result: make(chan watch.Event)}
	close(watcher.result)
	return watcher
}

// Implements watch.Interface.
func (c *immediateCloseWatcher) ResultChan() <-chan watch.Event {
	return c.result
}

// Implements watch.Interface.
func (c *immediateCloseWatcher) Stop() {
	// no-op
}
