// Copyright 2016 The etcd-operator Authors
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

package cluster

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/etcd-operator/pkg/backup/s3/s3config"
	"github.com/coreos/etcd-operator/pkg/spec"
	"github.com/coreos/etcd-operator/pkg/util/constants"
	"github.com/coreos/etcd-operator/pkg/util/etcdutil"
	"github.com/coreos/etcd-operator/pkg/util/k8sutil"
	"github.com/coreos/etcd-operator/pkg/util/retryutil"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/clientv3"
	"github.com/pborman/uuid"
	"golang.org/x/net/context"
	k8sapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/labels"
)

type clusterEventType string

const (
	eventDeleteCluster clusterEventType = "Delete"
	eventModifyCluster clusterEventType = "Modify"
)

type clusterEvent struct {
	typ     clusterEventType
	cluster *spec.EtcdCluster
}

type Config struct {
	PVProvisioner string
	s3config.S3Context

	MasterHost string
	KubeCli    *unversioned.Client
}

type Cluster struct {
	logger *logrus.Entry

	config Config

	cluster *spec.EtcdCluster

	// in memory state of the cluster
	status    *spec.ClusterStatus
	idCounter int

	eventCh chan *clusterEvent
	stopCh  chan struct{}

	// members repsersents the members in the etcd cluster.
	// the name of the member is the the name of the pod the member
	// process runs in.
	members etcdutil.MemberSet

	bm        *backupManager
	backupDir string
}

func New(c Config, e *spec.EtcdCluster, stopC <-chan struct{}, wg *sync.WaitGroup) (*Cluster, error) {
	return new(c, e, stopC, wg, true)
}

func Restore(c Config, e *spec.EtcdCluster, stopC <-chan struct{}, wg *sync.WaitGroup) (*Cluster, error) {
	return new(c, e, stopC, wg, false)
}

func new(config Config, e *spec.EtcdCluster, stopC <-chan struct{}, wg *sync.WaitGroup, isNewCluster bool) (*Cluster, error) {
	err := e.Spec.Validate()
	if err != nil {
		return nil, fmt.Errorf("invalid cluster spec: %v", err)
	}

	lg := logrus.WithField("pkg", "cluster").WithField("cluster-name", e.Name)
	var bm *backupManager

	if b := e.Spec.Backup; b != nil && b.MaxBackups > 0 {
		bm, err = newBackupManager(config, e, lg, isNewCluster)
		if err != nil {
			return nil, err
		}
	}
	c := &Cluster{
		logger:  lg,
		config:  config,
		cluster: e,
		eventCh: make(chan *clusterEvent, 100),
		stopCh:  make(chan struct{}),
		status:  &spec.ClusterStatus{},
		bm:      bm,
	}

	if isNewCluster {
		if c.bm != nil {
			if err := c.bm.setup(); err != nil {
				return nil, err
			}
		}

		if c.cluster.Spec.Restore == nil {
			// Note: For restore case, we don't need to create seed member,
			// and will go through reconcile loop and disaster recovery.
			if err := c.prepareSeedMember(); err != nil {
				return nil, err
			}
		}

		if err := c.createClientServiceLB(); err != nil {
			return nil, fmt.Errorf("fail to create client service LB: %v", err)
		}
	}

	wg.Add(1)
	go c.run(stopC, wg)

	return c, nil
}

func (c *Cluster) prepareSeedMember() error {
	var err error
	if sh := c.cluster.Spec.SelfHosted; sh != nil {
		if len(sh.BootMemberClientEndpoint) == 0 {
			err = c.newSelfHostedSeedMember()
		} else {
			err = c.migrateBootMember()
		}
	} else {
		err = c.newSeedMember()
	}
	return err
}

func (c *Cluster) Delete() {
	c.send(&clusterEvent{typ: eventDeleteCluster})
}

func (c *Cluster) send(ev *clusterEvent) {
	select {
	case c.eventCh <- ev:
	case <-c.stopCh:
	default:
		panic("TODO: too many events queued...")
	}
}

func (c *Cluster) run(stopC <-chan struct{}, wg *sync.WaitGroup) {
	needDeleteCluster := true

	defer func() {
		if needDeleteCluster {
			c.logger.Infof("deleting cluster")
			c.delete()
		}
		close(c.stopCh)
		wg.Done()

		c.status.SetPhaseFailed()
		// best effort to mark the cluster as failed

		f := func() (bool, error) {
			for {
				err := c.updateStatus()
				if err != nil {
					c.logger.Warningf("cluster delete: failed to update TPR status: %v", err)
					return false, nil
				}
				return true, nil
			}
		}

		retryutil.Retry(5*time.Second, math.MaxInt64, f)

	}()

	c.status.SetPhaseRunning()

	for {
		select {
		case <-stopC:
			needDeleteCluster = false
			return
		case event := <-c.eventCh:
			switch event.typ {
			case eventModifyCluster:
				// TODO: we can't handle another upgrade while an upgrade is in progress
				c.logger.Infof("spec update: from: %v to: %v", c.cluster.Spec, event.cluster.Spec)
				c.cluster = event.cluster
			case eventDeleteCluster:
				return
			}
		case <-time.After(5 * time.Second):
			if c.cluster.Spec.Paused {
				c.status.PauseControl()
				c.logger.Infof("control is paused, skipping reconcilation")
				continue
			} else {
				c.status.Control()
			}

			running, pending, err := c.pollPods()
			if err != nil {
				c.logger.Errorf("fail to poll pods: %v", err)
				continue
			}
			if len(pending) > 0 {
				c.logger.Infof("skip reconciliation: running (%v), pending (%v)", k8sutil.GetPodNames(running), k8sutil.GetPodNames(pending))
				continue
			}
			if len(running) == 0 {
				c.logger.Warningf("all etcd pods are dead. Trying to recover from a previous backup")
				err := c.disasterRecovery(nil)
				if err != nil {
					if err == errNoBackupExist {
						c.logger.Error("cluster cannot be recovered: all members are dead and there is no backup")
						c.status.SetReason(spec.FailedReasonNoBackup)
						return
					}
					c.logger.Errorf("fail to recover. Will retry later: %v", err)
				}
				continue // Back-off, either on normal recovery or error.
			}

			if err := c.reconcile(running); err != nil {
				c.logger.Errorf("fail to reconcile: %v", err)
				if isFatalError(err) {
					c.logger.Errorf("exiting for fatal error: %v", err)
					return
				}
			}

			err = c.updateStatus()
			if err != nil {
				c.logger.Warningf("failed to update TPR status: %v", err)
			}
		}
	}
}

func isFatalError(err error) bool {
	switch err {
	case errNoBackupExist:
		return true
	default:
		return false
	}
}

func (c *Cluster) makeSeedMember() *etcdutil.Member {
	etcdName := fmt.Sprintf("%s-%04d", c.cluster.Name, c.idCounter)
	return &etcdutil.Member{Name: etcdName}
}

func (c *Cluster) startSeedMember(recoverFromBackup bool) error {
	m := c.makeSeedMember()
	if err := c.createPodAndService(etcdutil.NewMemberSet(m), m, "new", recoverFromBackup); err != nil {
		c.logger.Errorf("failed to create seed member (%s): %v", m.Name, err)
		return err
	}
	c.idCounter++
	c.logger.Infof("cluster created with seed member (%s)", m.Name)
	return nil
}

func (c *Cluster) newSeedMember() error {
	return c.startSeedMember(false)
}

func (c *Cluster) restoreSeedMember() error {
	return c.startSeedMember(true)
}

func (c *Cluster) Update(e *spec.EtcdCluster) {
	anyInterestedChange := false
	s1, s2 := e.Spec, c.cluster.Spec
	switch {
	case s1.Size != s2.Size, s1.Paused != s2.Paused, s1.Version != s2.Version:
		anyInterestedChange = true
	}
	if anyInterestedChange {
		c.send(&clusterEvent{
			typ:     eventModifyCluster,
			cluster: e,
		})
	}
}

func (c *Cluster) updateMembers(etcdcli *clientv3.Client) error {
	ctx, _ := context.WithTimeout(context.Background(), constants.DefaultRequestTimeout)
	resp, err := etcdcli.MemberList(ctx)
	if err != nil {
		return err
	}
	c.members = etcdutil.MemberSet{}
	for _, m := range resp.Members {
		if len(m.Name) == 0 {
			c.members = nil
			return fmt.Errorf("the name of member (%x) is empty. Not ready yet. Will retry later", m.ID)
		}
		id := findID(m.Name)
		if id+1 > c.idCounter {
			c.idCounter = id + 1
		}

		c.members[m.Name] = &etcdutil.Member{
			Name:       m.Name,
			ID:         m.ID,
			ClientURLs: m.ClientURLs,
			PeerURLs:   m.PeerURLs,
		}
	}
	return nil
}

func findID(name string) int {
	i := strings.LastIndex(name, "-")
	id, err := strconv.Atoi(name[i+1:])
	if err != nil {
		// TODO: do not panic for single cluster error
		panic(fmt.Sprintf("TODO: fail to extract valid ID from name (%s): %v", name, err))
	}
	return id
}

func (c *Cluster) delete() {
	option := k8sapi.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			"etcd_cluster": c.cluster.Name,
			"app":          "etcd",
		}),
	}

	pods, err := c.config.KubeCli.Pods(c.cluster.Namespace).List(option)
	if err != nil {
		c.logger.Errorf("cluster deletion: cannot delete any pod due to failure to list: %v", err)
	} else {
		for i := range pods.Items {
			if err := c.removePodAndService(pods.Items[i].Name); err != nil {
				c.logger.Errorf("cluster deletion: fail to delete (%s)'s pod and svc: %v", pods.Items[i].Name, err)
			}
		}
	}

	err = c.deleteClientServiceLB()
	if err != nil {
		c.logger.Errorf("cluster deletion: fail to delete client service LB: %v", err)
	}

	if c.bm != nil {
		err := c.bm.cleanup()
		if err != nil {
			c.logger.Errorf("cluster deletion: backup manager failed to cleanup: %v", err)
		}
	}
}

func (c *Cluster) createClientServiceLB() error {
	if _, err := k8sutil.CreateEtcdService(c.config.KubeCli, c.cluster.Name, c.cluster.Namespace, c.cluster.AsOwner()); err != nil {
		if !k8sutil.IsKubernetesResourceAlreadyExistError(err) {
			return err
		}
	}
	return nil
}

func (c *Cluster) deleteClientServiceLB() error {
	err := c.config.KubeCli.Services(c.cluster.Namespace).Delete(c.cluster.Name)
	if err != nil {
		if !k8sutil.IsKubernetesResourceNotFoundError(err) {
			return err
		}
	}
	return nil
}

func (c *Cluster) createPodAndService(members etcdutil.MemberSet, m *etcdutil.Member, state string, needRecovery bool) error {
	// TODO: remove garbage service. Because we will fail after service created before pods created.
	svc := k8sutil.MakeEtcdMemberService(m.Name, c.cluster.Name, c.cluster.AsOwner())
	if _, err := k8sutil.CreateEtcdMemberService(c.config.KubeCli, c.cluster.Namespace, svc); err != nil {
		if !k8sutil.IsKubernetesResourceAlreadyExistError(err) {
			return err
		}
	}
	token := ""
	if state == "new" {
		token = uuid.New()
	}

	pod := k8sutil.MakeEtcdPod(m, members.PeerURLPairs(), c.cluster.Name, state, token, c.cluster.Spec, c.cluster.AsOwner())
	if needRecovery {
		k8sutil.AddRecoveryToPod(pod, c.cluster.Name, m.Name, token, c.cluster.Spec)
	}
	_, err := c.config.KubeCli.Pods(c.cluster.Namespace).Create(pod)
	return err
}

func (c *Cluster) removePodAndService(name string) error {
	err := c.config.KubeCli.Services(c.cluster.Namespace).Delete(name)
	if err != nil {
		if !k8sutil.IsKubernetesResourceNotFoundError(err) {
			return err
		}
	}
	err = c.config.KubeCli.Pods(c.cluster.Namespace).Delete(name, k8sapi.NewDeleteOptions(0))
	if err != nil {
		if !k8sutil.IsKubernetesResourceNotFoundError(err) {
			return err
		}
	}
	return nil
}

func (c *Cluster) pollPods() ([]*k8sapi.Pod, []*k8sapi.Pod, error) {
	podList, err := c.config.KubeCli.Pods(c.cluster.Namespace).List(k8sutil.EtcdPodListOpt(c.cluster.Name))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list running pods: %v", err)
	}

	var running []*k8sapi.Pod
	var pending []*k8sapi.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		switch pod.Status.Phase {
		case k8sapi.PodRunning:
			running = append(running, pod)
		case k8sapi.PodPending:
			pending = append(pending, pod)
		}
	}

	return running, pending, nil
}

func (c *Cluster) updateStatus() error {
	if reflect.DeepEqual(c.cluster.Status, c.status) {
		return nil
	}

	newCluster := c.cluster
	newCluster.Status = c.status
	newCluster, err := k8sutil.UpdateClusterTPRObject(c.config.KubeCli, c.config.MasterHost, c.cluster.GetNamespace(), newCluster)
	if err != nil {
		return err
	}

	c.cluster = newCluster
	return nil
}
