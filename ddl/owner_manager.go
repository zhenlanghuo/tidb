// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/juju/errors"
	"github.com/ngaut/log"
	goctx "golang.org/x/net/context"
)

// OwnerManager is used to campaign the owner and manage the owner information.
type OwnerManager interface {
	// ID returns the ID of DDL.
	ID() string
	// IsOwner returns whether the ownerManager is the DDL owner.
	IsOwner() bool
	// SetOwner sets whether the ownerManager is the DDL owner.
	SetOwner(isOwner bool)
	// IsOwner returns whether the ownerManager is the background owner.
	IsBgOwner() bool
	// SetOwner sets whether the ownerManager is the background owner.
	SetBgOwner(isOwner bool)
	// CampaignOwners campaigns the DDL owner and the background owner.
	CampaignOwners(ctx goctx.Context, wg *sync.WaitGroup) error
	// Cancel cancels this etcd ownerManager campaign.
	Cancel()
}

const (
	ddlOwnerKey               = "/tidb/ddl/owner"
	bgOwnerKey                = "/tidb/ddl/bg/owner"
	newSessionDefaultRetryCnt = 3
	newSessionRetryUnlimited  = math.MaxInt64
)

// ownerManager represents the structure which is used for electing owner.
type ownerManager struct {
	ddlOwner    int32
	bgOwner     int32
	ddlID       string // id is the ID of DDL.
	etcdCli     *clientv3.Client
	etcdSession *concurrency.Session
	cancel      goctx.CancelFunc
}

// NewOwnerManager creates a new OwnerManager.
func NewOwnerManager(etcdCli *clientv3.Client, id string, cancel goctx.CancelFunc) OwnerManager {
	return &ownerManager{
		etcdCli: etcdCli,
		ddlID:   id,
		cancel:  cancel,
	}
}

// ID implements OwnerManager.ID interface.
func (m *ownerManager) ID() string {
	return m.ddlID
}

// IsOwner implements OwnerManager.IsOwner interface.
func (m *ownerManager) IsOwner() bool {
	return atomic.LoadInt32(&m.ddlOwner) == 1
}

// SetOwner implements OwnerManager.SetOwner interface.
func (m *ownerManager) SetOwner(isOwner bool) {
	if isOwner {
		atomic.StoreInt32(&m.ddlOwner, 1)
	} else {
		atomic.StoreInt32(&m.ddlOwner, 0)
	}
}

// Cancel implements OwnerManager.Cancel interface.
func (m *ownerManager) Cancel() {
	m.cancel()
}

// IsBgOwner implements OwnerManager.IsBgOwner interface.
func (m *ownerManager) IsBgOwner() bool {
	return atomic.LoadInt32(&m.bgOwner) == 1
}

// SetBgOwner implements OwnerManager.SetBgOwner interface.
func (m *ownerManager) SetBgOwner(isOwner bool) {
	if isOwner {
		atomic.StoreInt32(&m.bgOwner, 1)
	} else {
		atomic.StoreInt32(&m.bgOwner, 0)
	}
}

// newSessionTTL is the etcd session's TTL in seconds.
const newSessionTTL = 10

func (m *ownerManager) newSession(ctx goctx.Context, retryCnt int) error {
	var err error
	for i := 0; i < retryCnt; i++ {
		m.etcdSession, err = concurrency.NewSession(m.etcdCli,
			concurrency.WithTTL(newSessionTTL), concurrency.WithContext(ctx))
		if err == nil {
			break
		}
		log.Warnf("[ddl] failed to new session, err %v", err)
		if isContextFinished(err) {
			break
		}
		time.Sleep(200 * time.Millisecond)
		continue
	}
	return errors.Trace(err)
}

// CampaignOwners implements OwnerManager.CampaignOwners interface.
func (m *ownerManager) CampaignOwners(ctx goctx.Context, wg *sync.WaitGroup) error {
	err := m.newSession(ctx, newSessionDefaultRetryCnt)
	if err != nil {
		return errors.Trace(err)
	}

	wg.Add(2)
	ddlCtx, _ := goctx.WithCancel(ctx)
	go m.campaignLoop(ddlCtx, ddlOwnerKey, wg)

	bgCtx, _ := goctx.WithCancel(ctx)
	go m.campaignLoop(bgCtx, bgOwnerKey, wg)
	return nil
}

func (m *ownerManager) campaignLoop(ctx goctx.Context, key string, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-m.etcdSession.Done():
			log.Info("[ddl] etcd session is done, creates a new one")
			err := m.newSession(ctx, newSessionRetryUnlimited)
			if err != nil {
				log.Infof("[ddl] break %s campaign loop, err %v", key, err)
				return
			}
		case <-ctx.Done():
			log.Infof("[ddl] break %s campaign loop", key)
			return
		default:
		}

		elec := concurrency.NewElection(m.etcdSession, key)
		err := elec.Campaign(ctx, m.ddlID)
		if err != nil {
			log.Infof("[ddl] %s ownerManager %s failed to campaign, err %v", key, m.ddlID, err)
			if isContextFinished(err) {
				log.Warnf("[ddl] break %s campaign loop, err %v", key, err)
				return
			}
			continue
		}

		// Get owner information.
		resp, err := elec.Leader(ctx)
		if err != nil {
			// If no leader elected currently, it returns ErrElectionNoLeader.
			log.Infof("[ddl] failed to get leader, err %v", err)
			continue
		}
		leader := string(resp.Kvs[0].Value)
		log.Infof("[ddl] %s ownerManager is %s, owner is %v", key, m.ddlID, leader)
		if leader == m.ddlID {
			m.setOwnerVal(key, true)
		} else {
			log.Warnf("[ddl] ownerManager %s isn't the owner", m.ddlID)
			continue
		}

		m.watchOwner(ctx, string(resp.Kvs[0].Key))
		m.setOwnerVal(key, false)
	}
}

func (m *ownerManager) setOwnerVal(key string, val bool) {
	if key == ddlOwnerKey {
		m.SetOwner(val)
	} else {
		m.SetBgOwner(val)
	}
}

func (m *ownerManager) watchOwner(ctx goctx.Context, key string) {
	log.Debugf("[ddl] ownerManager %s watch owner key %v", m.ddlID, key)
	watchCh := m.etcdCli.Watch(ctx, key)
	for {
		select {
		case resp := <-watchCh:
			if resp.Canceled {
				log.Infof("[ddl] ownerManager %s watch owner key %v failed, no owner",
					m.ddlID, key)
				return
			}

			for _, ev := range resp.Events {
				if ev.Type == mvccpb.DELETE {
					log.Infof("[ddl] ownerManager %s watch owner key %v failed, owner is deleted", m.ddlID, key)
					return
				}
			}
		case <-m.etcdSession.Done():
			return
		case <-ctx.Done():
			return
		}
	}
}
