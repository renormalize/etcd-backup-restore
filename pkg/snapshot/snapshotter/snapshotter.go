// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package snapshotter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gardener/etcd-backup-restore/pkg/compressor"
	"github.com/gardener/etcd-backup-restore/pkg/errors"
	"github.com/gardener/etcd-backup-restore/pkg/etcdutil"
	"github.com/gardener/etcd-backup-restore/pkg/health/heartbeat"
	"github.com/gardener/etcd-backup-restore/pkg/metrics"
	"github.com/gardener/etcd-backup-restore/pkg/miscellaneous"
	"github.com/gardener/etcd-backup-restore/pkg/snapstore"
	brtypes "github.com/gardener/etcd-backup-restore/pkg/types"
	"github.com/gardener/etcd-backup-restore/pkg/wrappers"

	"github.com/prometheus/client_golang/prometheus"
	cron "github.com/robfig/cron/v3"
	"github.com/sirupsen/logrus"
	"go.etcd.io/etcd/clientv3"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	min                          = iota // Minutes field
	hour                                // Hours field
	dayOfMonth                          // Day of month field
	month                               // Month field
	dayOfWeek                           // Day of week field
	defaultFullSnapMaxTimeWindow = 24   // default full snapshot time window in hours
)

var (
	emptyStruct struct{}
)

// event is wrapper over etcd event to keep track of time of event
type event struct {
	EtcdEvent *clientv3.Event `json:"etcdEvent"`
	Time      time.Time       `json:"time"`
}

type result struct {
	Snapshot *brtypes.Snapshot `json:"snapshot"`
	Err      error             `json:"error"`
}

// NewSnapshotterConfig returns the snapshotter config.
func NewSnapshotterConfig() *brtypes.SnapshotterConfig {
	return &brtypes.SnapshotterConfig{
		FullSnapshotSchedule:     brtypes.DefaultFullSnapshotSchedule,
		DeltaSnapshotPeriod:      wrappers.Duration{Duration: brtypes.DefaultDeltaSnapshotInterval},
		DeltaSnapshotMemoryLimit: brtypes.DefaultDeltaSnapMemoryLimit,
		GarbageCollectionPeriod:  wrappers.Duration{Duration: brtypes.DefaultGarbageCollectionPeriod},
		GarbageCollectionPolicy:  brtypes.GarbageCollectionPolicyExponential,
		MaxBackups:               brtypes.DefaultMaxBackups,
	}
}

// Snapshotter is a struct for etcd snapshot taker
type Snapshotter struct {
	lastSecretModifiedTime       time.Time
	schedule                     cron.Schedule
	store                        brtypes.SnapStore
	K8sClientset                 client.Client
	FullSnapshotLeaseUpdateTimer *time.Timer
	fullSnapshotTimer            *time.Timer
	compressionConfig            *compressor.CompressionConfig
	PrevSnapshot                 *brtypes.Snapshot
	PrevFullSnapshot             *brtypes.Snapshot
	etcdConnectionConfig         *brtypes.EtcdConnectionConfig
	fullSnapshotReqCh            chan bool
	deltaSnapshotReqCh           chan struct{}
	fullSnapshotAckCh            chan result
	deltaSnapshotAckCh           chan result
	logger                       *logrus.Entry
	HealthConfig                 *brtypes.HealthConfig
	deltaSnapshotTimer           *time.Timer
	snapstoreConfig              *brtypes.SnapstoreConfig
	watchCh                      clientv3.WatchChan
	etcdWatchClient              *clientv3.Watcher
	cancelWatch                  context.CancelFunc
	SsrStateMutex                *sync.Mutex
	config                       *brtypes.SnapshotterConfig
	events                       []byte
	PrevDeltaSnapshots           brtypes.SnapList
	lastEventRevision            int64
	SsrState                     brtypes.SnapshotterState
	PrevFullSnapshotSucceeded    bool
}

// NewSnapshotter returns the snapshotter object.
func NewSnapshotter(logger *logrus.Entry, config *brtypes.SnapshotterConfig, store brtypes.SnapStore, etcdConnectionConfig *brtypes.EtcdConnectionConfig, compressionConfig *compressor.CompressionConfig, healthConfig *brtypes.HealthConfig, storeConfig *brtypes.SnapstoreConfig) (*Snapshotter, error) {
	sdl, err := cron.ParseStandard(config.FullSnapshotSchedule)
	if err != nil {
		// Ideally this should be validated before.
		return nil, fmt.Errorf("invalid full snapshot schedule provided %s : %v", config.FullSnapshotSchedule, err)
	}

	var prevSnapshot *brtypes.Snapshot
	fullSnap, deltaSnapList, err := miscellaneous.GetLatestFullSnapshotAndDeltaSnapList(store)
	if err != nil {
		return nil, err
	} else if fullSnap != nil && len(deltaSnapList) == 0 {
		prevSnapshot = fullSnap
		// setting timestamps of both full and delta to prev full snapshot's timestamp
		metrics.LatestSnapshotTimestamp.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindFull}).Set(float64(prevSnapshot.CreatedOn.Unix()))
		metrics.LatestSnapshotTimestamp.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindDelta}).Set(float64(prevSnapshot.CreatedOn.Unix()))
	} else if fullSnap != nil && len(deltaSnapList) != 0 {
		prevSnapshot = deltaSnapList[len(deltaSnapList)-1]
		metrics.LatestSnapshotTimestamp.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindFull}).Set(float64(fullSnap.CreatedOn.Unix()))
		metrics.LatestSnapshotTimestamp.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindDelta}).Set(float64(prevSnapshot.CreatedOn.Unix()))
	} else {
		// creating dummy previous snapshot since fullSnap == nil
		prevSnapshot = snapstore.NewSnapshot(brtypes.SnapshotKindFull, 0, 0, "", false)
	}

	metrics.LatestSnapshotRevision.With(prometheus.Labels{metrics.LabelKind: prevSnapshot.Kind}).Set(float64(prevSnapshot.LastRevision))

	//Attempt to create clientset only if `enable-snapshot-lease-renewal` flag of healthConfig is set
	var clientSet client.Client
	if healthConfig.SnapshotLeaseRenewalEnabled {
		clientSet, err = miscellaneous.GetKubernetesClientSetOrError()
		if err != nil {
			return nil, err
		}
	}

	return &Snapshotter{
		logger:                    logger.WithField("actor", "snapshotter"),
		store:                     store,
		config:                    config,
		etcdConnectionConfig:      etcdConnectionConfig,
		compressionConfig:         compressionConfig,
		HealthConfig:              healthConfig,
		schedule:                  sdl,
		PrevSnapshot:              prevSnapshot,
		PrevFullSnapshot:          fullSnap,
		PrevDeltaSnapshots:        deltaSnapList,
		SsrState:                  brtypes.SnapshotterInactive,
		SsrStateMutex:             &sync.Mutex{},
		fullSnapshotReqCh:         make(chan bool),
		deltaSnapshotReqCh:        make(chan struct{}),
		fullSnapshotAckCh:         make(chan result),
		deltaSnapshotAckCh:        make(chan result),
		cancelWatch:               func() {},
		K8sClientset:              clientSet,
		snapstoreConfig:           storeConfig,
		PrevFullSnapshotSucceeded: true,
	}, nil
}

// Run process loop for scheduled backup
// Setting startWithFullSnapshot to false will start the snapshotter without
// taking the first full snapshot.
func (ssr *Snapshotter) Run(stopCh <-chan struct{}, startWithFullSnapshot bool) error {
	fullSnapshotLeaseStopCh := make(chan struct{})
	defer ssr.stop(fullSnapshotLeaseStopCh)
	if startWithFullSnapshot {
		ssr.fullSnapshotTimer = time.NewTimer(0)
	} else {
		// for the case when snapshotter is run for the first time on
		// a fresh etcd with startWithFullSnapshot set to false, we need
		// to take the first delta snapshot(s) initially and then set
		// the full snapshot schedule
		if ssr.watchCh == nil {
			ssrStopped, err := ssr.CollectEventsSincePrevSnapshot(stopCh)
			if ssrStopped {
				return nil
			}
			if err != nil {
				return fmt.Errorf("failed to collect events for first delta snapshot(s): %v", err)
			}
		}
		if err := ssr.resetFullSnapshotTimer(); err != nil {
			return fmt.Errorf("failed to reset full snapshot timer: %v", err)
		}
	}
	if ssr.HealthConfig.SnapshotLeaseRenewalEnabled {
		go ssr.RenewFullSnapshotLeasePeriodically(fullSnapshotLeaseStopCh, brtypes.FullSnapshotLeaseUpdateInterval)
	}
	ssr.deltaSnapshotTimer = time.NewTimer(brtypes.DefaultDeltaSnapshotInterval)
	if ssr.config.DeltaSnapshotPeriod.Duration >= brtypes.DeltaSnapshotIntervalThreshold {
		ssr.deltaSnapshotTimer.Stop()
		ssr.deltaSnapshotTimer.Reset(ssr.config.DeltaSnapshotPeriod.Duration)
	}

	return ssr.snapshotEventHandler(stopCh)
}

// TriggerFullSnapshot sends the events to take full snapshot. This is to
// trigger full snapshot externally out of regular schedule.
func (ssr *Snapshotter) TriggerFullSnapshot(_ context.Context, isFinal bool) (*brtypes.Snapshot, error) {
	ssr.SsrStateMutex.Lock()
	defer ssr.SsrStateMutex.Unlock()

	if ssr.SsrState != brtypes.SnapshotterActive {
		return nil, fmt.Errorf("snapshotter is not active")
	}
	ssr.logger.Info("Triggering out of schedule full snapshot...")
	ssr.fullSnapshotReqCh <- isFinal
	res := <-ssr.fullSnapshotAckCh
	return res.Snapshot, res.Err
}

// TriggerDeltaSnapshot sends the events to take delta snapshot. This is to
// trigger delta snapshot externally out of regular schedule.
func (ssr *Snapshotter) TriggerDeltaSnapshot() (*brtypes.Snapshot, error) {
	ssr.SsrStateMutex.Lock()
	defer ssr.SsrStateMutex.Unlock()

	if ssr.SsrState != brtypes.SnapshotterActive {
		return nil, fmt.Errorf("snapshotter is not active")
	}
	if ssr.config.DeltaSnapshotPeriod.Duration < brtypes.DeltaSnapshotIntervalThreshold {
		return nil, fmt.Errorf("found delta snapshot interval %s less than %v. Delta snapshotting is disabled. ", ssr.config.DeltaSnapshotPeriod.Duration, time.Duration(brtypes.DeltaSnapshotIntervalThreshold))
	}
	ssr.logger.Info("Triggering out of schedule delta snapshot...")
	ssr.deltaSnapshotReqCh <- emptyStruct
	res := <-ssr.deltaSnapshotAckCh
	return res.Snapshot, res.Err
}

// stop stops the snapshotter. Once stopped any subsequent calls will
// not have any effect.
func (ssr *Snapshotter) stop(fullSnapshotLeaseStopCh chan struct{}) {
	ssr.logger.Info("Closing the Snapshotter...")

	if ssr.fullSnapshotTimer != nil {
		ssr.fullSnapshotTimer.Stop()
		ssr.fullSnapshotTimer = nil
	}
	if ssr.deltaSnapshotTimer != nil {
		ssr.deltaSnapshotTimer.Stop()
		ssr.deltaSnapshotTimer = nil
	}
	if ssr.HealthConfig.SnapshotLeaseRenewalEnabled {
		fullSnapshotLeaseStopCh <- emptyStruct
	}
	ssr.SetSnapshotterInactive()
	ssr.closeEtcdClient()
}

// SetSnapshotterInactive set the snapshotter state to Inactive.
func (ssr *Snapshotter) SetSnapshotterInactive() {
	ssr.SsrStateMutex.Lock()
	defer ssr.SsrStateMutex.Unlock()
	ssr.SsrState = brtypes.SnapshotterInactive
}

// SetSnapshotterActive set the snapshotter state to active.
func (ssr *Snapshotter) SetSnapshotterActive() {
	ssr.SsrStateMutex.Lock()
	defer ssr.SsrStateMutex.Unlock()
	ssr.SsrState = brtypes.SnapshotterActive
}

func (ssr *Snapshotter) closeEtcdClient() {
	if ssr.cancelWatch != nil {
		ssr.cancelWatch()
		ssr.cancelWatch = nil
	}
	if ssr.watchCh != nil {
		ssr.watchCh = nil
	}

	if ssr.etcdWatchClient != nil {
		if err := (*ssr.etcdWatchClient).Close(); err != nil {
			ssr.logger.Warnf("Error while closing etcd watch client connection, %v", err)
		}
		ssr.etcdWatchClient = nil
	}
}

// TakeFullSnapshotAndResetTimer takes a full snapshot and resets the full snapshot
// timer as per the schedule.
func (ssr *Snapshotter) TakeFullSnapshotAndResetTimer(isFinal bool) (*brtypes.Snapshot, error) {
	ssr.logger.Infof("Taking scheduled full snapshot for time: %s", time.Now().Local())
	s, err := ssr.takeFullSnapshot(isFinal)
	if err != nil {
		// As per design principle, in business critical service if backup is not working,
		// it's better to fail the process. So, we are quiting here.
		ssr.logger.Warnf("Taking scheduled full snapshot failed: %v", err)
		return nil, err
	}

	return s, ssr.resetFullSnapshotTimer()
}

// takeFullSnapshot will store full snapshot of etcd to brtypes.
// It basically will connect to etcd. Then ask for snapshot. And finally
// store it to underlying snapstore on the fly.
func (ssr *Snapshotter) takeFullSnapshot(isFinal bool) (*brtypes.Snapshot, error) {
	defer ssr.cleanupInMemoryEvents()
	// close previous watch and client.
	ssr.closeEtcdClient()

	// Update the snapstore object before taking a full snapshot if the credentials have changed
	// Refer: https://github.com/gardener/etcd-backup-restore/issues/449
	hasSecretUpdated, err := ssr.hasSnapStoreSecretUpdated()
	if err != nil {
		return nil, fmt.Errorf("error checking if the credentials were updated %v", err)
	}
	if hasSecretUpdated {
		var err error
		ssr.store, err = snapstore.GetSnapstore(ssr.snapstoreConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create snapstore from configured storage provider: %v", err)
		}
		ssr.logger.Info("Updated the snapstore object with new credentials")
	}

	clientFactory := etcdutil.NewFactory(*ssr.etcdConnectionConfig)
	clientKV, err := clientFactory.NewKV()
	if err != nil {
		return nil, &errors.EtcdError{
			Message: fmt.Sprintf("failed to create etcd KV client: %v", err),
		}
	}
	defer clientKV.Close()

	ctx, cancel := context.WithTimeout(context.TODO(), ssr.etcdConnectionConfig.ConnectionTimeout.Duration)
	// Note: Although Get and snapshot call are not atomic, so revision number in snapshot file
	// may be ahead of the revision found from GET call. But currently this is the only workaround available
	// Refer: https://github.com/coreos/etcd/issues/9037
	resp, err := clientKV.Get(ctx, "", clientv3.WithLastRev()...)
	cancel()
	if err != nil {
		return nil, &errors.EtcdError{
			Message: fmt.Sprintf("failed to get etcd latest revision: %v", err),
		}
	}
	lastRevision := resp.Header.Revision
	if isFinal && ssr.PrevSnapshot.IsFinal && ssr.PrevSnapshot.Kind == brtypes.SnapshotKindFull && ssr.PrevSnapshot.LastRevision == lastRevision {
		ssr.logger.Infof("There are no new updates since previous final full snapshot, skipping new final full snapshot.")
	} else {
		// Note: As FullSnapshot size can be very large, so to avoid context timeout use "SnapshotTimeout" in context.WithTimeout()
		ctx, cancel = context.WithTimeout(context.TODO(), ssr.etcdConnectionConfig.SnapshotTimeout.Duration)
		defer cancel()
		// compressionSuffix is useful in backward compatibility(restoring from uncompressed snapshots).
		// it is also helpful in inferring which compression Policy to be used to decompress the snapshot.
		compressionSuffix, err := compressor.GetCompressionSuffix(ssr.compressionConfig.Enabled, ssr.compressionConfig.CompressionPolicy)
		if err != nil {
			return nil, fmt.Errorf("failed to get compressionSuffix: %v", err)
		}

		clientMaintenance, err := clientFactory.NewMaintenance()
		if err != nil {
			return nil, fmt.Errorf("failed to build etcd maintenance client")
		}
		defer clientMaintenance.Close()

		s, err := etcdutil.TakeAndSaveFullSnapshot(ctx, clientMaintenance, ssr.store, ssr.snapstoreConfig.TempDir, lastRevision, ssr.compressionConfig, compressionSuffix, isFinal, ssr.logger)
		if err != nil {
			return nil, err
		}

		ssr.PrevSnapshot = s
		ssr.PrevFullSnapshot = s
		ssr.PrevDeltaSnapshots = nil

		metrics.LatestSnapshotRevision.With(prometheus.Labels{metrics.LabelKind: ssr.PrevSnapshot.Kind}).Set(float64(ssr.PrevSnapshot.LastRevision))
		metrics.LatestSnapshotTimestamp.With(prometheus.Labels{metrics.LabelKind: ssr.PrevSnapshot.Kind}).Set(float64(ssr.PrevSnapshot.CreatedOn.Unix()))
		metrics.SnapstoreLatestDeltasTotal.With(prometheus.Labels{}).Set(0)
		metrics.SnapstoreLatestDeltasRevisionsTotal.With(prometheus.Labels{}).Set(0)

		ssr.logger.Infof("Successfully saved full snapshot at: %s", path.Join(s.SnapDir, s.SnapName))
	}
	// setting `snapshotRequired` to 0 for both full and delta snapshot
	// for the following cases:
	// i.  Skipped full snapshot since no events were collected
	// ii. Successfully took a full snapshot
	metrics.SnapshotRequired.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindFull}).Set(0)
	metrics.SnapshotRequired.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindDelta}).Set(0)

	if ssr.config.DeltaSnapshotPeriod.Duration < time.Second {
		// return without creating a watch on events
		return ssr.PrevSnapshot, nil
	}

	ssrEtcdWatchClient, err := clientFactory.NewWatcher()
	if err != nil {
		return nil, &errors.EtcdError{
			Message: fmt.Sprintf("failed to create etcd watch client for snapshotter: %v", err),
		}
	}
	watchCtx, cancelWatch := context.WithCancel(context.TODO())
	ssr.cancelWatch = cancelWatch
	ssr.etcdWatchClient = &ssrEtcdWatchClient
	ssr.watchCh = ssrEtcdWatchClient.Watch(watchCtx, "", clientv3.WithPrefix(), clientv3.WithRev(ssr.PrevSnapshot.LastRevision+1))
	ssr.logger.Infof("Applied watch on etcd from revision: %d", ssr.PrevSnapshot.LastRevision+1)

	return ssr.PrevSnapshot, nil
}

func (ssr *Snapshotter) cleanupInMemoryEvents() {
	ssr.events = []byte{}
	ssr.lastEventRevision = -1
}

func (ssr *Snapshotter) takeDeltaSnapshotAndResetTimer() (*brtypes.Snapshot, error) {
	s, err := ssr.TakeDeltaSnapshot()
	if err != nil {
		// As per design principle, in business critical service if backup is not working,
		// it's better to fail the process. So, we are quiting here.
		ssr.logger.Warnf("Taking delta snapshot failed: %v", err)
		return nil, err
	}

	if ssr.deltaSnapshotTimer == nil {
		ssr.deltaSnapshotTimer = time.NewTimer(ssr.config.DeltaSnapshotPeriod.Duration)
	} else {
		ssr.logger.Infof("Stopping delta snapshot...")
		ssr.deltaSnapshotTimer.Stop()
		ssr.logger.Infof("Resetting delta snapshot to run after %s.", ssr.config.DeltaSnapshotPeriod.Duration.String())
		ssr.deltaSnapshotTimer.Reset(ssr.config.DeltaSnapshotPeriod.Duration)
	}
	return s, nil
}

// TakeDeltaSnapshot takes a delta snapshot that contains
// the etcd events collected up till now
func (ssr *Snapshotter) TakeDeltaSnapshot() (*brtypes.Snapshot, error) {
	defer ssr.cleanupInMemoryEvents()
	ssr.logger.Infof("Taking delta snapshot for time: %s", time.Now().Local())

	if len(ssr.events) == 0 {
		ssr.logger.Infof("No events received to save snapshot. Skipping delta snapshot.")
		metrics.SnapshotRequired.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindDelta}).Set(0)
		return nil, nil
	}
	ssr.events = append(ssr.events, byte(']'))

	// Update the snapstore object before taking a delta snapshot if the credentials have changed
	// Refer: https://github.com/gardener/etcd-backup-restore/issues/449
	hasSecretUpdated, err := ssr.hasSnapStoreSecretUpdated()
	if err != nil {
		return nil, fmt.Errorf("error checking if the credentials were updated %v", err)
	}
	if hasSecretUpdated {
		var err error
		ssr.store, err = snapstore.GetSnapstore(ssr.snapstoreConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create snapstore from configured storage provider: %v", err)
		}
		ssr.logger.Info("Updated the snapstore object with new credentials")
	}

	// compressionSuffix is useful in backward compatibility(restoring from uncompressed snapshots).
	// it is also helpful in inferring which compression Policy to be used to decompress the snapshot.
	compressionSuffix, err := compressor.GetCompressionSuffix(ssr.compressionConfig.Enabled, ssr.compressionConfig.CompressionPolicy)
	if err != nil {
		return nil, fmt.Errorf("failed to get compressionSuffix: %v", err)
	}
	snap := snapstore.NewSnapshot(brtypes.SnapshotKindDelta, ssr.PrevSnapshot.LastRevision+1, ssr.lastEventRevision, compressionSuffix, false)

	// compute hash
	hash := sha256.New()
	if _, err := hash.Write(ssr.events); err != nil {
		return nil, fmt.Errorf("failed to compute hash of events: %v", err)
	}
	ssr.events = hash.Sum(ssr.events)

	startTime := time.Now()
	rc := io.NopCloser(bytes.NewReader(ssr.events))

	// if compression is enabled
	//    then compress the snapshot.
	if ssr.compressionConfig.Enabled {
		ssr.logger.Info("start the Compression of delta snapshot")
		rc, err = compressor.CompressSnapshot(rc, ssr.compressionConfig.CompressionPolicy)
		if err != nil {
			return nil, fmt.Errorf("unable to compress delta snapshot: %v", err)
		}
	}
	defer rc.Close()

	if err := ssr.store.Save(*snap, rc); err != nil {
		timeTaken := time.Since(startTime).Seconds()
		metrics.SnapshotDurationSeconds.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindDelta, metrics.LabelSucceeded: metrics.ValueSucceededFalse}).Observe(timeTaken)
		ssr.logger.Errorf("Error saving delta snapshots. %v", err)
		return nil, err
	}
	timeTaken := time.Since(startTime).Seconds()
	metrics.SnapshotDurationSeconds.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindDelta, metrics.LabelSucceeded: metrics.ValueSucceededTrue}).Observe(timeTaken)
	logrus.Infof("Total time to save delta snapshot: %f seconds.", timeTaken)
	ssr.PrevSnapshot = snap
	ssr.PrevDeltaSnapshots = append(ssr.PrevDeltaSnapshots, snap)

	metrics.LatestSnapshotRevision.With(prometheus.Labels{metrics.LabelKind: ssr.PrevSnapshot.Kind}).Set(float64(ssr.PrevSnapshot.LastRevision))
	metrics.LatestSnapshotTimestamp.With(prometheus.Labels{metrics.LabelKind: ssr.PrevSnapshot.Kind}).Set(float64(ssr.PrevSnapshot.CreatedOn.Unix()))
	metrics.SnapshotRequired.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindDelta}).Set(0)
	metrics.SnapstoreLatestDeltasTotal.With(prometheus.Labels{}).Inc()
	metrics.SnapstoreLatestDeltasRevisionsTotal.With(prometheus.Labels{}).Add(float64(snap.LastRevision - snap.StartRevision))

	ssr.logger.Infof("Successfully saved delta snapshot at: %s", path.Join(snap.SnapDir, snap.SnapName))
	return snap, nil
}

// CollectEventsSincePrevSnapshot takes the first delta snapshot on etcd startup.
func (ssr *Snapshotter) CollectEventsSincePrevSnapshot(stopCh <-chan struct{}) (bool, error) {
	// close any previous watch and client.
	ssr.closeEtcdClient()

	clientFactory := etcdutil.NewFactory(*ssr.etcdConnectionConfig)
	clientKV, err := clientFactory.NewKV()
	if err != nil {
		return false, &errors.EtcdError{
			Message: fmt.Sprintf("failed to create etcd KV client: %v", err),
		}
	}
	defer clientKV.Close()

	ctx, cancel := context.WithTimeout(context.TODO(), ssr.etcdConnectionConfig.ConnectionTimeout.Duration)
	resp, err := clientKV.Get(ctx, "", clientv3.WithLastRev()...)
	cancel()
	if err != nil {
		return false, &errors.EtcdError{
			Message: fmt.Sprintf("failed to get etcd latest revision: %v", err),
		}
	}
	lastEtcdRevision := resp.Header.Revision

	metrics.SnapshotRequired.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindFull}).Set(0)
	metrics.SnapshotRequired.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindDelta}).Set(0)

	// if etcd revision newer than latest full snapshot revision,
	// set `required` metric for full snapshot to 1
	if ssr.PrevFullSnapshot == nil || ssr.PrevFullSnapshot.LastRevision != lastEtcdRevision {
		metrics.SnapshotRequired.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindFull}).Set(1)
	}

	ssrEtcdWatchClient, err := clientFactory.NewWatcher()
	if err != nil {
		return false, &errors.EtcdError{
			Message: fmt.Sprintf("failed to create etcd watch client for snapshotter: %v", err),
		}
	}
	// TODO: Use parent context. Passing parent context here directly requires some additional management of error handling.
	watchCtx, cancelWatch := context.WithCancel(context.TODO())
	ssr.cancelWatch = cancelWatch
	ssr.etcdWatchClient = &ssrEtcdWatchClient
	ssr.watchCh = ssrEtcdWatchClient.Watch(watchCtx, "", clientv3.WithPrefix(), clientv3.WithRev(ssr.PrevSnapshot.LastRevision+1))
	ssr.logger.Infof("Applied watch on etcd from revision: %d", ssr.PrevSnapshot.LastRevision+1)

	if ssr.PrevSnapshot.LastRevision == lastEtcdRevision {
		ssr.logger.Infof("No new events since last snapshot. Skipping initial delta snapshot.")
		return false, nil
	}

	// need to take a delta snapshot here, because etcd revision is
	// newer than latest snapshot revision. Also means, a subsequent
	// full snapshot will be required later
	metrics.SnapshotRequired.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindDelta}).Set(1)
	metrics.SnapshotRequired.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindFull}).Set(1)

	for {
		select {
		case wr, ok := <-ssr.watchCh:
			if !ok {
				return false, fmt.Errorf("watch channel closed")
			}
			if err := ssr.handleDeltaWatchEvents(wr); err != nil {
				return false, err
			}

			lastWatchRevision := wr.Events[len(wr.Events)-1].Kv.ModRevision
			if lastWatchRevision >= lastEtcdRevision {
				return false, nil
			}
		case <-stopCh:
			ssr.cleanupInMemoryEvents()
			return true, nil
		}
	}
}

func (ssr *Snapshotter) handleDeltaWatchEvents(wr clientv3.WatchResponse) error {
	if err := wr.Err(); err != nil {
		return err
	}
	// aggregate events
	for _, ev := range wr.Events {
		timedEvent := newEvent(ev)
		jsonByte, err := json.Marshal(timedEvent)
		if err != nil {
			return fmt.Errorf("failed to marshal events to json: %v", err)
		}
		if len(ssr.events) == 0 {
			ssr.events = append(ssr.events, byte('['))
		} else {
			ssr.events = append(ssr.events, byte(','))
		}
		ssr.events = append(ssr.events, jsonByte...)
		ssr.lastEventRevision = ev.Kv.ModRevision
		metrics.SnapshotRequired.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindFull}).Set(1)
		metrics.SnapshotRequired.With(prometheus.Labels{metrics.LabelKind: brtypes.SnapshotKindDelta}).Set(1)
	}
	ssr.logger.Debugf("Added events till revision: %d", ssr.lastEventRevision)
	// #nosec G115 -- validated for size to be lesser than MaxInt.
	if len(ssr.events) >= int(ssr.config.DeltaSnapshotMemoryLimit) {
		ssr.logger.Infof("Delta events memory crossed the memory limit: %d Bytes", len(ssr.events))
		_, err := ssr.takeDeltaSnapshotAndResetTimer()
		return err
	}
	return nil
}

func newEvent(e *clientv3.Event) *event {
	return &event{
		EtcdEvent: e,
		Time:      time.Now(),
	}
}

func (ssr *Snapshotter) snapshotEventHandler(stopCh <-chan struct{}) error {
	leaseUpdateCtx, leaseUpdateCancel := context.WithCancel(context.TODO())
	defer leaseUpdateCancel()
	ssr.logger.Info("Starting the Snapshot EventHandler.")
	for {
		select {
		case isFinal := <-ssr.fullSnapshotReqCh:
			s, err := ssr.TakeFullSnapshotAndResetTimer(isFinal)
			res := result{
				Snapshot: s,
				Err:      err,
			}
			ssr.fullSnapshotAckCh <- res
			if err != nil {
				ssr.PrevFullSnapshotSucceeded = false
				return err
			}
			ssr.PrevFullSnapshotSucceeded = true
			if ssr.HealthConfig.SnapshotLeaseRenewalEnabled {
				ssr.FullSnapshotLeaseUpdateTimer.Stop()
				ssr.FullSnapshotLeaseUpdateTimer.Reset(time.Nanosecond)
			}

		case <-ssr.deltaSnapshotReqCh:
			s, err := ssr.takeDeltaSnapshotAndResetTimer()
			res := result{
				Snapshot: s,
				Err:      err,
			}
			ssr.deltaSnapshotAckCh <- res
			if err != nil {
				return err
			}
			if ssr.HealthConfig.SnapshotLeaseRenewalEnabled {
				ctx, cancel := context.WithTimeout(leaseUpdateCtx, brtypes.LeaseUpdateTimeoutDuration)
				if err = heartbeat.DeltaSnapshotCaseLeaseUpdate(ctx, ssr.logger, ssr.K8sClientset, ssr.HealthConfig.DeltaSnapshotLeaseName, ssr.store); err != nil {
					ssr.logger.Warnf("Snapshot lease update failed : %v", err)
				}
				cancel()
			}

		case <-ssr.fullSnapshotTimer.C:
			if _, err := ssr.TakeFullSnapshotAndResetTimer(false); err != nil {
				ssr.PrevFullSnapshotSucceeded = false
				return err
			}
			ssr.PrevFullSnapshotSucceeded = true
			if ssr.HealthConfig.SnapshotLeaseRenewalEnabled {
				ssr.FullSnapshotLeaseUpdateTimer.Stop()
				ssr.FullSnapshotLeaseUpdateTimer.Reset(time.Nanosecond)
			}

		case <-ssr.deltaSnapshotTimer.C:
			if ssr.config.DeltaSnapshotPeriod.Duration >= time.Second {
				if _, err := ssr.takeDeltaSnapshotAndResetTimer(); err != nil {
					return err
				}
				if ssr.HealthConfig.SnapshotLeaseRenewalEnabled {
					ctx, cancel := context.WithTimeout(leaseUpdateCtx, brtypes.LeaseUpdateTimeoutDuration)
					if err := heartbeat.DeltaSnapshotCaseLeaseUpdate(ctx, ssr.logger, ssr.K8sClientset, ssr.HealthConfig.DeltaSnapshotLeaseName, ssr.store); err != nil {
						ssr.logger.Warnf("Snapshot lease update failed : %v", err)
					}
					cancel()
				}
			}

		case wr, ok := <-ssr.watchCh:
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			snapshots := len(ssr.PrevDeltaSnapshots)
			if err := ssr.handleDeltaWatchEvents(wr); err != nil {
				return err
			}
			if ssr.HealthConfig.SnapshotLeaseRenewalEnabled {
				//Call UpdateDeltaSnapshotLease only if new delta snapshot taken
				if snapshots < len(ssr.PrevDeltaSnapshots) {
					ctx, cancel := context.WithTimeout(leaseUpdateCtx, brtypes.LeaseUpdateTimeoutDuration)
					if err := heartbeat.DeltaSnapshotCaseLeaseUpdate(ctx, ssr.logger, ssr.K8sClientset, ssr.HealthConfig.DeltaSnapshotLeaseName, ssr.store); err != nil {
						ssr.logger.Warnf("Snapshot lease update failed : %v", err)
					}
					cancel()
				}
			}

		case <-stopCh:
			ssr.logger.Info("Closing the Snapshot EventHandler.")
			ssr.cleanupInMemoryEvents()
			return nil
		}
	}
}

func (ssr *Snapshotter) resetFullSnapshotTimer() error {
	now := time.Now()
	effective := ssr.schedule.Next(now)
	if effective.IsZero() {
		ssr.logger.Info("There are no backups scheduled for the future. Stopping now.")
		return fmt.Errorf("error in full snapshot schedule")
	}
	duration := effective.Sub(now)
	if ssr.fullSnapshotTimer == nil {
		ssr.fullSnapshotTimer = time.NewTimer(duration)
	} else {
		ssr.logger.Infof("Stopping full snapshot...")
		ssr.fullSnapshotTimer.Stop()
		ssr.logger.Infof("Resetting full snapshot to run after %s", duration)
		ssr.fullSnapshotTimer.Reset(duration)
	}
	ssr.logger.Infof("Will take next full snapshot at time: %s", effective)

	return nil
}

// hasSnapStoreSecretUpdated checks if the snapstore secret has been updated
func (ssr *Snapshotter) hasSnapStoreSecretUpdated() (bool, error) {
	ssr.logger.Debug("checking the timestamp of snapstore secret...")
	newSecretModifiedTime, err := snapstore.GetSnapstoreSecretModifiedTime(ssr.snapstoreConfig.Provider)
	if err != nil {
		return false, fmt.Errorf("error checking the modification time of the access credentials  %v", err)
	}

	// the secret has not been modified
	if !newSecretModifiedTime.After(ssr.lastSecretModifiedTime) {
		return false, nil
	}

	// update the previous modification time with the latest modification time
	ssr.lastSecretModifiedTime = newSecretModifiedTime
	return true, nil
}

// IsFullSnapshotRequiredAtStartup checks whether to take a full snapshot or not during the startup of backup-restore.
func (ssr *Snapshotter) IsFullSnapshotRequiredAtStartup(timeWindow float64) bool {
	if ssr.PrevFullSnapshot == nil || ssr.PrevFullSnapshot.IsFinal || time.Since(ssr.PrevFullSnapshot.CreatedOn).Hours() > timeWindow || !ssr.PrevFullSnapshotSucceeded {
		return true
	}

	if !ssr.WasScheduledFullSnapshotMissed(timeWindow) {
		return false
	}
	return ssr.IsNextFullSnapshotBeyondTimeWindow(timeWindow)
}

// WasScheduledFullSnapshotMissed determines whether the preceding full-snapshot was missed or not.
func (ssr *Snapshotter) WasScheduledFullSnapshotMissed(timeWindow float64) bool {
	now := time.Now()
	nextSnapSchedule := ssr.schedule.Next(now)

	if miscellaneous.GetPrevScheduledSnapTime(nextSnapSchedule, timeWindow) == ssr.PrevFullSnapshot.CreatedOn {
		ssr.logger.Info("previous full snapshot was taken at scheduled time, skipping the full snapshot at startup")
		return false
	}
	return true
}

// IsNextFullSnapshotBeyondTimeWindow determines whether the next scheduled full snapshot will exceed the given time window or not.
func (ssr *Snapshotter) IsNextFullSnapshotBeyondTimeWindow(timeWindow float64) bool {
	now := time.Now()
	nextSnapSchedule := ssr.schedule.Next(now)
	timeLeftToTakeNextSnap := nextSnapSchedule.Sub(now)

	return timeLeftToTakeNextSnap.Hours()+time.Since(ssr.PrevFullSnapshot.CreatedOn).Hours() > timeWindow
}

// GetFullSnapshotMaxTimeWindow returns the maximum time period in hours for which backup-restore must take atleast one full snapshot.
func (ssr *Snapshotter) GetFullSnapshotMaxTimeWindow(fullSnapScheduleSpec string) float64 {
	// Split on whitespace.
	schedule := strings.Fields(fullSnapScheduleSpec)
	if len(schedule) < 5 {
		return defaultFullSnapMaxTimeWindow
	}

	if schedule[dayOfWeek] != "*" {
		return defaultFullSnapMaxTimeWindow * 7
	}

	if schedule[dayOfMonth] == "*" && schedule[dayOfWeek] == "*" && strings.Contains(schedule[hour], "/") {
		if timeWindow, err := strconv.ParseFloat(schedule[hour][strings.Index(schedule[hour], "/")+1:], 64); err == nil {
			return timeWindow
		}
	}

	return defaultFullSnapMaxTimeWindow
}
