// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package leaderelection_test

import (
	"context"
	"fmt"
	"time"

	brtypes "github.com/gardener/etcd-backup-restore/pkg/types"

	"github.com/sirupsen/logrus"

	. "github.com/gardener/etcd-backup-restore/pkg/leaderelection"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Etcd Cluster", func() {
	var (
		le                    *LeaderElector
		config                *brtypes.Config
		etcdConnectionConfig  *brtypes.EtcdConnectionConfig
		startLeaseRenewal     int
		stopLeaseRenewal      int
		startSnapshotterCount int
		stopSnapshotterCount  int
		promoteLearnerCount   int
		learnerToVotingMember int
	)

	BeforeEach(func() {
		etcdConnectionConfig = brtypes.NewEtcdConnectionConfig()

		leaderCallbacks := &brtypes.LeaderCallbacks{
			OnStartedLeading: func(_ context.Context) {
				logger.Info("starting snapshotter...")
				startSnapshotterCount++
			},
			OnStoppedLeading: func() {
				logger.Info("stopping snapshotter...")
				stopSnapshotterCount++
			},
		}

		memberLeaseCallbacks := &brtypes.MemberLeaseCallbacks{
			StartLeaseRenewal: func() {
				logger.Info("started lease Renewal...")
				startLeaseRenewal++
			},
			StopLeaseRenewal: func() {
				logger.Info("stopped lease Renewal...")
				stopLeaseRenewal++
			},
		}

		promoteCallback := &brtypes.PromoteLearnerCallback{
			Promote: func(_ context.Context, logger *logrus.Entry) {
				logger.Info("promote a learner to voting member...")
				learnerToVotingMember++
				promoteLearnerCount++
			},
		}

		config = brtypes.NewLeaderElectionConfig()
		config.ReelectionPeriod = reelectionPeriod
		config.EtcdConnectionTimeout = etcdConnectionTimeout
		le, _ = NewLeaderElector(logger, etcdConnectionConfig, config, leaderCallbacks, memberLeaseCallbacks, nil, promoteCallback)
	})

	Describe("LeaderElection", func() {
		BeforeEach(func() {
			startLeaseRenewal = 0
			stopLeaseRenewal = 0
			startSnapshotterCount = 0
			stopSnapshotterCount = 0
		})

		Context("When Etcd is not running", func() {
			It("should moved to UnknownState from Follower State and stop lease renewal", func() {
				minCount := 1
				ctx, cancel := context.WithTimeout(testCtx, mockTimeout)
				defer cancel()

				le.CheckMemberStatus = func(_ context.Context, _ *brtypes.EtcdConnectionConfig, _ time.Duration, _ *logrus.Entry) (bool, bool, error) {
					return false, false, fmt.Errorf("unable to connect to etcd")
				}

				err := le.Run(ctx)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(le.CurrentState).Should(Equal(StateUnknown))
				Expect(stopLeaseRenewal).Should(Equal(minCount))
			})
		})

		Context("Etcd is Running as a Leader etcd", func() {
			It("should becomes the leading sidecar and moved to Leader State from Follower State", func() {
				minCount := 1
				ctx, cancel := context.WithTimeout(testCtx, mockTimeout)
				defer cancel()

				le.CheckMemberStatus = func(_ context.Context, _ *brtypes.EtcdConnectionConfig, _ time.Duration, _ *logrus.Entry) (bool, bool, error) {
					return true, false, nil
				}

				err := le.Run(ctx)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(le.CurrentState).Should(Equal(StateLeader))
				Expect(startSnapshotterCount).Should(Equal(minCount))
			})
		})

		Context("Etcd is Running as a Follower etcd", func() {
			It("should becomes the follower sidecar, so no change in State", func() {
				ctx, cancel := context.WithTimeout(testCtx, mockTimeout)
				defer cancel()

				le.CheckMemberStatus = func(_ context.Context, _ *brtypes.EtcdConnectionConfig, _ time.Duration, _ *logrus.Entry) (bool, bool, error) {
					return false, false, nil
				}

				err := le.Run(ctx)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(le.CurrentState).Should(Equal(StateFollower))
			})
		})

		Context("Etcd Lost the leader-election", func() {
			It("Should stop the snapshotter as backup-restore becomes follower sidecar from leading sidecar", func() {
				minCount := 1

				ctx, cancel := context.WithTimeout(testCtx, mockTimeout)
				defer cancel()

				le.CheckMemberStatus = func(_ context.Context, _ *brtypes.EtcdConnectionConfig, _ time.Duration, _ *logrus.Entry) (bool, bool, error) {
					if startSnapshotterCount == 0 {
						return true, false, nil
					} else {
						return false, false, nil
					}
				}

				err := le.Run(ctx)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(le.CurrentState).Should(Equal(StateFollower))
				Expect(startSnapshotterCount).Should(Equal(minCount))
				Expect(stopSnapshotterCount).Should(Equal(minCount))
			})
		})

		Context("Etcd lost the Quorum", func() {
			It("Should stop the snapshotter and leaseRenewal as backup-restore moves to UnkownState from Leader", func() {
				minCount := 1
				ctx, cancel := context.WithTimeout(testCtx, mockTimeout)
				defer cancel()

				le.CheckMemberStatus = func(_ context.Context, _ *brtypes.EtcdConnectionConfig, _ time.Duration, _ *logrus.Entry) (bool, bool, error) {
					if startSnapshotterCount == 0 {
						return true, false, nil
					} else {
						return false, false, fmt.Errorf("currently there is no etcd leader present may be due to etcd quorum loss")
					}
				}

				err := le.Run(ctx)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(le.CurrentState).Should(Equal(StateUnknown))
				Expect(startSnapshotterCount).Should(Equal(minCount))
				Expect(stopSnapshotterCount).Should(Equal(minCount))
				Expect(stopLeaseRenewal).Should(Equal(minCount))
			})
		})

		Context("Etcd member is learner", func() {
			It("Should promote the learner(non-voting) member to a voting member", func() {
				minCount := 1

				ctx, cancel := context.WithTimeout(testCtx, mockTimeout)
				defer cancel()

				le.CheckMemberStatus = func(_ context.Context, _ *brtypes.EtcdConnectionConfig, _ time.Duration, _ *logrus.Entry) (bool, bool, error) {
					if learnerToVotingMember == 0 {
						return false, true, nil
					} else {
						return false, false, nil
					}
				}

				err := le.Run(ctx)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(le.CurrentState).Should(Equal(StateFollower))
				Expect(promoteLearnerCount).Should(Equal(minCount))
			})
		})
	})
})
