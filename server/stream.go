// Copyright 2019-2022 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/s2"
	"github.com/nats-io/nuid"
)

// StreamConfig will determine the name, subjects and retention policy
// for a given stream. If subjects is empty the name will be used.
type StreamConfig struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Subjects     []string        `json:"subjects,omitempty"`
	Retention    RetentionPolicy `json:"retention"`
	MaxConsumers int             `json:"max_consumers"`
	MaxMsgs      int64           `json:"max_msgs"`
	MaxBytes     int64           `json:"max_bytes"`
	MaxAge       time.Duration   `json:"max_age"`
	MaxMsgsPer   int64           `json:"max_msgs_per_subject"`
	MaxMsgSize   int32           `json:"max_msg_size,omitempty"`
	Discard      DiscardPolicy   `json:"discard"`
	Storage      StorageType     `json:"storage"`
	Replicas     int             `json:"num_replicas"`
	NoAck        bool            `json:"no_ack,omitempty"`
	Template     string          `json:"template_owner,omitempty"`
	Duplicates   time.Duration   `json:"duplicate_window,omitempty"`
	Placement    *Placement      `json:"placement,omitempty"`
	Mirror       *StreamSource   `json:"mirror,omitempty"`
	Sources      []*StreamSource `json:"sources,omitempty"`

	// Optional qualifiers. These can not be modified after set to true.

	// Sealed will seal a stream so no messages can get out or in.
	Sealed bool `json:"sealed"`
	// DenyDelete will restrict the ability to delete messages.
	DenyDelete bool `json:"deny_delete"`
	// DenyPurge will restrict the ability to purge messages.
	DenyPurge bool `json:"deny_purge"`
	// AllowRollup allows messages to be placed into the system and purge
	// all older messages using a special msg header.
	AllowRollup bool `json:"allow_rollup_hdrs"`
}

// JSPubAckResponse is a formal response to a publish operation.
type JSPubAckResponse struct {
	Error *ApiError `json:"error,omitempty"`
	*PubAck
}

// ToError checks if the response has a error and if it does converts it to an error
// avoiding the pitfalls described by https://yourbasic.org/golang/gotcha-why-nil-error-not-equal-nil/
func (r *JSPubAckResponse) ToError() error {
	if r.Error == nil {
		return nil
	}
	return r.Error
}

// PubAck is the detail you get back from a publish to a stream that was successful.
// e.g. +OK {"stream": "Orders", "seq": 22}
type PubAck struct {
	Stream    string `json:"stream"`
	Sequence  uint64 `json:"seq"`
	Domain    string `json:"domain,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// StreamInfo shows config and current state for this stream.
type StreamInfo struct {
	Config  StreamConfig        `json:"config"`
	Created time.Time           `json:"created"`
	State   StreamState         `json:"state"`
	Domain  string              `json:"domain,omitempty"`
	Cluster *ClusterInfo        `json:"cluster,omitempty"`
	Mirror  *StreamSourceInfo   `json:"mirror,omitempty"`
	Sources []*StreamSourceInfo `json:"sources,omitempty"`
}

// ClusterInfo shows information about the underlying set of servers
// that make up the stream or consumer.
type ClusterInfo struct {
	Name     string      `json:"name,omitempty"`
	Leader   string      `json:"leader,omitempty"`
	Replicas []*PeerInfo `json:"replicas,omitempty"`
}

// PeerInfo shows information about all the peers in the cluster that
// are supporting the stream or consumer.
type PeerInfo struct {
	Name    string        `json:"name"`
	Current bool          `json:"current"`
	Offline bool          `json:"offline,omitempty"`
	Active  time.Duration `json:"active"`
	Lag     uint64        `json:"lag,omitempty"`
}

// StreamSourceInfo shows information about an upstream stream source.
type StreamSourceInfo struct {
	Name     string          `json:"name"`
	External *ExternalStream `json:"external,omitempty"`
	Lag      uint64          `json:"lag"`
	Active   time.Duration   `json:"active"`
	Error    *ApiError       `json:"error,omitempty"`
}

// StreamSource dictates how streams can source from other streams.
type StreamSource struct {
	Name          string          `json:"name"`
	OptStartSeq   uint64          `json:"opt_start_seq,omitempty"`
	OptStartTime  *time.Time      `json:"opt_start_time,omitempty"`
	FilterSubject string          `json:"filter_subject,omitempty"`
	External      *ExternalStream `json:"external,omitempty"`

	// Internal
	iname string // For indexing when stream names are the same for multiple sources.
}

// ExternalStream allows you to qualify access to a stream source in another account.
type ExternalStream struct {
	ApiPrefix     string `json:"api"`
	DeliverPrefix string `json:"deliver"`
}

// Stream is a jetstream stream of messages. When we receive a message internally destined
// for a Stream we will direct link from the client to this structure.
type stream struct {
	mu        sync.RWMutex
	js        *jetStream
	jsa       *jsAccount
	acc       *Account
	srv       *Server
	client    *client
	sysc      *client
	sid       int
	pubAck    []byte
	outq      *jsOutQ
	msgs      *ipQueue // of *inMsg
	store     StreamStore
	ackq      *ipQueue // of uint64
	lseq      uint64
	lmsgId    string
	consumers map[string]*consumer
	numFilter int
	cfg       StreamConfig
	created   time.Time
	stype     StorageType
	tier      string
	ddmap     map[string]*ddentry
	ddarr     []*ddentry
	ddindex   int
	ddtmr     *time.Timer
	qch       chan struct{}
	active    bool
	ddloaded  bool

	// Mirror
	mirror *sourceInfo

	// Sources
	sources map[string]*sourceInfo

	// Indicates we have direct consumers.
	directs int

	// TODO(dlc) - Hide everything below behind two pointers.
	// Clustered mode.
	sa       *streamAssignment
	node     RaftNode
	catchup  bool
	syncSub  *subscription
	infoSub  *subscription
	clMu     sync.Mutex
	clseq    uint64
	clfs     uint64
	leader   string
	lqsent   time.Time
	catchups map[string]uint64
}

type sourceInfo struct {
	name  string
	iname string
	cname string
	sub   *subscription
	msgs  *ipQueue // of *inMsg
	sseq  uint64
	dseq  uint64
	lag   uint64
	err   *ApiError
	last  time.Time
	lreq  time.Time
	qch   chan struct{}
	grr   bool
}

// Headers for published messages.
const (
	JSMsgId               = "Nats-Msg-Id"
	JSExpectedStream      = "Nats-Expected-Stream"
	JSExpectedLastSeq     = "Nats-Expected-Last-Sequence"
	JSExpectedLastSubjSeq = "Nats-Expected-Last-Subject-Sequence"
	JSExpectedLastMsgId   = "Nats-Expected-Last-Msg-Id"
	JSStreamSource        = "Nats-Stream-Source"
	JSLastConsumerSeq     = "Nats-Last-Consumer"
	JSLastStreamSeq       = "Nats-Last-Stream"
	JSConsumerStalled     = "Nats-Consumer-Stalled"
	JSMsgRollup           = "Nats-Rollup"
	JSMsgSize             = "Nats-Msg-Size"
	JSResponseType        = "Nats-Response-Type"
)

// Rollups, can be subject only or all messages.
const (
	JSMsgRollupSubject = "sub"
	JSMsgRollupAll     = "all"
)

const (
	jsCreateResponse = "create"
)

// Dedupe entry
type ddentry struct {
	id  string
	seq uint64
	ts  int64
}

// Replicas Range
const (
	StreamMaxReplicas = 5
)

// AddStream adds a stream for the given account.
func (a *Account) addStream(config *StreamConfig) (*stream, error) {
	return a.addStreamWithAssignment(config, nil, nil)
}

// AddStreamWithStore adds a stream for the given account with custome store config options.
func (a *Account) addStreamWithStore(config *StreamConfig, fsConfig *FileStoreConfig) (*stream, error) {
	return a.addStreamWithAssignment(config, fsConfig, nil)
}

func (a *Account) addStreamWithAssignment(config *StreamConfig, fsConfig *FileStoreConfig, sa *streamAssignment) (*stream, error) {
	s, jsa, err := a.checkForJetStream()
	if err != nil {
		return nil, err
	}

	// If we do not have the stream currently assigned to us in cluster mode we will proceed but warn.
	// This can happen on startup with restored state where on meta replay we still do not have
	// the assignment. Running in single server mode this always returns true.
	if !jsa.streamAssigned(config.Name) {
		s.Debugf("Stream '%s > %s' does not seem to be assigned to this server", a.Name, config.Name)
	}

	// Sensible defaults.
	cfg, err := checkStreamCfg(config, &s.getOpts().JetStreamLimits)
	if err != nil {
		return nil, NewJSStreamInvalidConfigError(err, Unless(err))
	}

	// Can't create a stream with a sealed state.
	if cfg.Sealed {
		return nil, NewJSStreamInvalidConfigError(fmt.Errorf("stream configuration for create can not be sealed"))
	}

	singleServerMode := !s.JetStreamIsClustered() && s.standAloneMode()
	if singleServerMode && cfg.Replicas > 1 {
		return nil, ApiErrors[JSStreamReplicasNotSupportedErr]
	}

	js, isClustered := jsa.jetStreamAndClustered()
	jsa.mu.RLock()
	if mset, ok := jsa.streams[cfg.Name]; ok {
		jsa.mu.RUnlock()
		// Check to see if configs are same.
		ocfg := mset.config()
		if reflect.DeepEqual(ocfg, cfg) {
			if sa != nil {
				mset.setStreamAssignment(sa)
			}
			return mset, nil
		} else {
			return nil, ApiErrors[JSStreamNameExistErr]
		}
	}
	selected, tier, hasTier := jsa.selectLimits(&cfg)
	reserved := int64(0)
	if !isClustered {
		reserved = jsa.tieredReservation(tier, &cfg)
	}
	jsa.mu.RUnlock()
	if !hasTier {
		return nil, NewJSNoLimitsError()
	}
	js.mu.RLock()
	if isClustered {
		_, reserved = tieredStreamAndReservationCount(js.cluster.streams[a.Name], tier, &cfg)
	}
	if err := js.checkAllLimits(&selected, &cfg, reserved, 0); err != nil {
		js.mu.RUnlock()
		return nil, err
	}
	js.mu.RUnlock()
	jsa.mu.Lock()
	// Check for template ownership if present.
	if cfg.Template != _EMPTY_ && jsa.account != nil {
		if !jsa.checkTemplateOwnership(cfg.Template, cfg.Name) {
			jsa.mu.Unlock()
			return nil, fmt.Errorf("stream not owned by template")
		}
	}

	// Check for mirror designation.
	if cfg.Mirror != nil {
		// Can't have subjects.
		if len(cfg.Subjects) > 0 {
			jsa.mu.Unlock()
			return nil, fmt.Errorf("stream mirrors can not also contain subjects")
		}
		if len(cfg.Sources) > 0 {
			jsa.mu.Unlock()
			return nil, fmt.Errorf("stream mirrors can not also contain other sources")
		}
		if cfg.Mirror.FilterSubject != _EMPTY_ {
			jsa.mu.Unlock()
			return nil, fmt.Errorf("stream mirrors can not contain filtered subjects")
		}
		if cfg.Mirror.OptStartSeq > 0 && cfg.Mirror.OptStartTime != nil {
			jsa.mu.Unlock()
			return nil, fmt.Errorf("stream mirrors can not have both start seq and start time configured")
		}
	} else if len(cfg.Subjects) == 0 && len(cfg.Sources) == 0 {
		jsa.mu.Unlock()
		return nil, fmt.Errorf("stream needs at least one configured subject or mirror")
	}

	// Setup our internal indexed names here for sources.
	if len(cfg.Sources) > 0 {
		for _, ssi := range cfg.Sources {
			ssi.setIndexName()
		}
	}

	// Check for overlapping subjects. These are not allowed for now.
	if jsa.subjectsOverlap(cfg.Subjects) {
		jsa.mu.Unlock()
		return nil, fmt.Errorf("subjects overlap with an existing stream")
	}

	if !hasTier {
		jsa.mu.Unlock()
		return nil, fmt.Errorf("no applicable tier found")
	}

	// Setup the internal clients.
	c := s.createInternalJetStreamClient()
	ic := s.createInternalJetStreamClient()

	qpfx := fmt.Sprintf("[ACC:%s] stream '%s' ", a.Name, config.Name)
	mset := &stream{
		acc:       a,
		jsa:       jsa,
		cfg:       cfg,
		js:        js,
		srv:       s,
		client:    c,
		sysc:      ic,
		tier:      tier,
		stype:     cfg.Storage,
		consumers: make(map[string]*consumer),
		msgs:      s.newIPQueue(qpfx + "messages"), // of *inMsg
		qch:       make(chan struct{}),
	}

	// For no-ack consumers when we are interest retention.
	if cfg.Retention != LimitsPolicy {
		mset.ackq = s.newIPQueue(qpfx + "acks") // of uint64
	}

	jsa.streams[cfg.Name] = mset
	storeDir := filepath.Join(jsa.storeDir, streamsDir, cfg.Name)
	jsa.mu.Unlock()

	// Bind to the user account.
	c.registerWithAccount(a)
	// Bind to the system account.
	ic.registerWithAccount(s.SystemAccount())

	// Create the appropriate storage
	fsCfg := fsConfig
	if fsCfg == nil {
		fsCfg = &FileStoreConfig{}
		// If we are file based and not explicitly configured
		// we may be able to auto-tune based on max msgs or bytes.
		if cfg.Storage == FileStorage {
			mset.autoTuneFileStorageBlockSize(fsCfg)
		}
	}
	fsCfg.StoreDir = storeDir
	fsCfg.AsyncFlush = false
	fsCfg.SyncInterval = 2 * time.Minute

	if err := mset.setupStore(fsCfg); err != nil {
		mset.stop(true, false)
		return nil, err
	}

	// Create our pubAck template here. Better than json marshal each time on success.
	if domain := s.getOpts().JetStreamDomain; domain != _EMPTY_ {
		mset.pubAck = []byte(fmt.Sprintf("{%q:%q, %q:%q, %q:", "stream", cfg.Name, "domain", domain, "seq"))
	} else {
		mset.pubAck = []byte(fmt.Sprintf("{%q:%q, %q:", "stream", cfg.Name, "seq"))
	}
	end := len(mset.pubAck)
	mset.pubAck = mset.pubAck[:end:end]

	// Set our known last sequence.
	var state StreamState
	mset.store.FastState(&state)
	mset.lseq = state.LastSeq

	// If no msgs (new stream), set dedupe state loaded to true.
	if state.Msgs == 0 {
		mset.ddloaded = true
	}

	// Set our stream assignment if in clustered mode.
	if sa != nil {
		mset.setStreamAssignment(sa)
	}

	// Setup our internal send go routine.
	mset.setupSendCapabilities()

	// Reserve resources if MaxBytes present.
	mset.js.reserveStreamResources(&mset.cfg)

	// Call directly to set leader if not in clustered mode.
	// This can be called though before we actually setup clustering, so check both.
	if singleServerMode {
		if err := mset.setLeader(true); err != nil {
			mset.stop(true, false)
			return nil, err
		}
	}

	// This is always true in single server mode.
	mset.mu.RLock()
	isLeader := mset.isLeader()
	mset.mu.RUnlock()

	if isLeader {
		// Send advisory.
		var suppress bool
		if !s.standAloneMode() && sa == nil {
			if cfg.Replicas > 1 {
				suppress = true
			}
		} else if sa != nil {
			suppress = sa.responded
		}
		if !suppress {
			mset.sendCreateAdvisory()
		}
	}

	return mset, nil
}

// Sets the index name. Usually just the stream name but when the stream is external we will
// use additional information in case the stream names are the same.
func (ssi *StreamSource) setIndexName() {
	if ssi.External != nil {
		ssi.iname = ssi.Name + ":" + string(getHash(ssi.External.ApiPrefix))
	} else {
		ssi.iname = ssi.Name
	}
}

func (mset *stream) streamAssignment() *streamAssignment {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.sa
}

func (mset *stream) setStreamAssignment(sa *streamAssignment) {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	mset.sa = sa
	if sa == nil {
		return
	}

	// Set our node.
	mset.node = sa.Group.node

	// Setup our info sub here as well for all stream members. This is now by design.
	if mset.infoSub == nil {
		isubj := fmt.Sprintf(clusterStreamInfoT, mset.jsa.acc(), mset.cfg.Name)
		// Note below the way we subscribe here is so that we can send requests to ourselves.
		mset.infoSub, _ = mset.srv.systemSubscribe(isubj, _EMPTY_, false, mset.sysc, mset.handleClusterStreamInfoRequest)
	}
}

// IsLeader will return if we are the current leader.
func (mset *stream) IsLeader() bool {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	return mset.isLeader()
}

// Lock should be held.
func (mset *stream) isLeader() bool {
	if mset.isClustered() {
		return mset.node.Leader()
	}
	return true
}

// TODO(dlc) - Check to see if we can accept being the leader or we should should step down.
func (mset *stream) setLeader(isLeader bool) error {
	mset.mu.Lock()
	// If we are here we have a change in leader status.
	if isLeader {
		// Make sure we are listening for sync requests.
		// TODO(dlc) - Original design was that all in sync members of the group would do DQ.
		mset.startClusterSubs()
		// Setup subscriptions
		if err := mset.subscribeToStream(); err != nil {
			mset.mu.Unlock()
			return err
		}
		// Clear and fixup state we had for last state.
		mset.clfs = 0
	} else {
		// Stop responding to sync requests.
		mset.stopClusterSubs()
		// Unsubscribe from direct stream.
		mset.unsubscribeToStream()
		// Clear catchup state
		mset.clearAllCatchupPeers()
		// Check on any fixup state and optionally clear.
		if mset.isClustered() && mset.leader != _EMPTY_ && mset.leader != mset.node.GroupLeader() {
			mset.clfs = 0
		}
	}
	// Track group leader.
	if mset.isClustered() {
		mset.leader = mset.node.GroupLeader()
	} else {
		mset.leader = _EMPTY_
	}
	mset.mu.Unlock()
	return nil
}

// Lock should be held.
func (mset *stream) startClusterSubs() {
	if mset.isClustered() && mset.syncSub == nil {
		mset.syncSub, _ = mset.srv.systemSubscribe(mset.sa.Sync, _EMPTY_, false, mset.sysc, mset.handleClusterSyncRequest)
	}
}

// Lock should be held.
func (mset *stream) stopClusterSubs() {
	if mset.syncSub != nil {
		mset.srv.sysUnsubscribe(mset.syncSub)
		mset.syncSub = nil
	}
}

// account gets the account for this stream.
func (mset *stream) account() *Account {
	mset.mu.RLock()
	jsa := mset.jsa
	mset.mu.RUnlock()
	if jsa == nil {
		return nil
	}
	return jsa.acc()
}

// Helper to determine the max msg size for this stream if file based.
func (mset *stream) maxMsgSize() uint64 {
	maxMsgSize := mset.cfg.MaxMsgSize
	if maxMsgSize <= 0 {
		// Pull from the account.
		if mset.jsa != nil {
			if acc := mset.jsa.acc(); acc != nil {
				acc.mu.RLock()
				maxMsgSize = acc.mpay
				acc.mu.RUnlock()
			}
		}
		// If all else fails use default.
		if maxMsgSize <= 0 {
			maxMsgSize = MAX_PAYLOAD_SIZE
		}
	}
	// Now determine an estimation for the subjects etc.
	maxSubject := -1
	for _, subj := range mset.cfg.Subjects {
		if subjectIsLiteral(subj) {
			if len(subj) > maxSubject {
				maxSubject = len(subj)
			}
		}
	}
	if maxSubject < 0 {
		const defaultMaxSubject = 256
		maxSubject = defaultMaxSubject
	}
	// filestore will add in estimates for record headers, etc.
	return fileStoreMsgSizeEstimate(maxSubject, int(maxMsgSize))
}

// If we are file based and the file storage config was not explicitly set
// we can autotune block sizes to better match. Our target will be to store 125%
// of the theoretical limit. We will round up to nearest 100 bytes as well.
func (mset *stream) autoTuneFileStorageBlockSize(fsCfg *FileStoreConfig) {
	var totalEstSize uint64

	// MaxBytes will take precedence for now.
	if mset.cfg.MaxBytes > 0 {
		totalEstSize = uint64(mset.cfg.MaxBytes)
	} else if mset.cfg.MaxMsgs > 0 {
		// Determine max message size to estimate.
		totalEstSize = mset.maxMsgSize() * uint64(mset.cfg.MaxMsgs)
	} else if mset.cfg.MaxMsgsPer > 0 {
		fsCfg.BlockSize = uint64(defaultKVBlockSize)
		return
	} else {
		// If nothing set will let underlying filestore determine blkSize.
		return
	}

	blkSize := (totalEstSize / 4) + 1 // (25% overhead)
	// Round up to nearest 100
	if m := blkSize % 100; m != 0 {
		blkSize += 100 - m
	}
	if blkSize < FileStoreMinBlkSize {
		blkSize = FileStoreMinBlkSize
	}
	if blkSize > FileStoreMaxBlkSize {
		blkSize = FileStoreMaxBlkSize
	}
	fsCfg.BlockSize = uint64(blkSize)
}

// rebuildDedupe will rebuild any dedupe structures needed after recovery of a stream.
// Will be called lazily to avoid penalizing startup times.
// TODO(dlc) - Might be good to know if this should be checked at all for streams with no
// headers and msgId in them. Would need signaling from the storage layer.
// Lock should be held.
func (mset *stream) rebuildDedupe() {
	if mset.ddloaded {
		return
	}

	mset.ddloaded = true

	// We have some messages. Lookup starting sequence by duplicate time window.
	sseq := mset.store.GetSeqFromTime(time.Now().Add(-mset.cfg.Duplicates))
	if sseq == 0 {
		return
	}

	var smv StoreMsg
	state := mset.store.State()
	for seq := sseq; seq <= state.LastSeq; seq++ {
		sm, err := mset.store.LoadMsg(seq, &smv)
		if err != nil {
			continue
		}
		var msgId string
		if len(sm.hdr) > 0 {
			if msgId = getMsgId(sm.hdr); msgId != _EMPTY_ {
				mset.storeMsgIdLocked(&ddentry{msgId, sm.seq, sm.ts})
			}
		}
		if seq == state.LastSeq {
			mset.lmsgId = msgId
		}
	}
}

func (mset *stream) lastSeq() uint64 {
	mset.mu.RLock()
	lseq := mset.lseq
	mset.mu.RUnlock()
	return lseq
}

func (mset *stream) setLastSeq(lseq uint64) {
	mset.mu.Lock()
	mset.lseq = lseq
	mset.mu.Unlock()
}

func (mset *stream) sendCreateAdvisory() {
	mset.mu.Lock()
	name := mset.cfg.Name
	template := mset.cfg.Template
	outq := mset.outq
	srv := mset.srv
	mset.mu.Unlock()

	if outq == nil {
		return
	}

	// finally send an event that this stream was created
	m := JSStreamActionAdvisory{
		TypedEvent: TypedEvent{
			Type: JSStreamActionAdvisoryType,
			ID:   nuid.Next(),
			Time: time.Now().UTC(),
		},
		Stream:   name,
		Action:   CreateEvent,
		Template: template,
		Domain:   srv.getOpts().JetStreamDomain,
	}

	j, err := json.Marshal(m)
	if err != nil {
		return
	}

	subj := JSAdvisoryStreamCreatedPre + "." + name
	outq.sendMsg(subj, j)
}

func (mset *stream) sendDeleteAdvisoryLocked() {
	if mset.outq == nil {
		return
	}

	m := JSStreamActionAdvisory{
		TypedEvent: TypedEvent{
			Type: JSStreamActionAdvisoryType,
			ID:   nuid.Next(),
			Time: time.Now().UTC(),
		},
		Stream:   mset.cfg.Name,
		Action:   DeleteEvent,
		Template: mset.cfg.Template,
		Domain:   mset.srv.getOpts().JetStreamDomain,
	}

	j, err := json.Marshal(m)
	if err == nil {
		subj := JSAdvisoryStreamDeletedPre + "." + mset.cfg.Name
		mset.outq.sendMsg(subj, j)
	}
}

func (mset *stream) sendUpdateAdvisoryLocked() {
	if mset.outq == nil {
		return
	}

	m := JSStreamActionAdvisory{
		TypedEvent: TypedEvent{
			Type: JSStreamActionAdvisoryType,
			ID:   nuid.Next(),
			Time: time.Now().UTC(),
		},
		Stream: mset.cfg.Name,
		Action: ModifyEvent,
		Domain: mset.srv.getOpts().JetStreamDomain,
	}

	j, err := json.Marshal(m)
	if err == nil {
		subj := JSAdvisoryStreamUpdatedPre + "." + mset.cfg.Name
		mset.outq.sendMsg(subj, j)
	}
}

// Created returns created time.
func (mset *stream) createdTime() time.Time {
	mset.mu.RLock()
	created := mset.created
	mset.mu.RUnlock()
	return created
}

// Internal to allow creation time to be restored.
func (mset *stream) setCreatedTime(created time.Time) {
	mset.mu.Lock()
	mset.created = created
	mset.mu.Unlock()
}

// Check to see if these subjects overlap with existing subjects.
// Lock should be held.
func (jsa *jsAccount) subjectsOverlap(subjects []string) bool {
	for _, mset := range jsa.streams {
		for _, subj := range mset.cfg.Subjects {
			for _, tsubj := range subjects {
				if SubjectsCollide(tsubj, subj) {
					return true
				}
			}
		}
	}
	return false
}

// StreamDefaultDuplicatesWindow default duplicates window.
const StreamDefaultDuplicatesWindow = 2 * time.Minute

func checkStreamCfg(config *StreamConfig, lim *JSLimitOpts) (StreamConfig, error) {
	if config == nil {
		return StreamConfig{}, fmt.Errorf("stream configuration invalid")
	}
	if !isValidName(config.Name) {
		return StreamConfig{}, fmt.Errorf("stream name is required and can not contain '.', '*', '>'")
	}
	if len(config.Name) > JSMaxNameLen {
		return StreamConfig{}, fmt.Errorf("stream name is too long, maximum allowed is %d", JSMaxNameLen)
	}
	if len(config.Description) > JSMaxDescriptionLen {
		return StreamConfig{}, fmt.Errorf("stream description is too long, maximum allowed is %d", JSMaxDescriptionLen)
	}

	cfg := *config

	// Make file the default.
	if cfg.Storage == 0 {
		cfg.Storage = FileStorage
	}
	if cfg.Replicas == 0 {
		cfg.Replicas = 1
	}
	if cfg.Replicas > StreamMaxReplicas {
		return cfg, fmt.Errorf("maximum replicas is %d", StreamMaxReplicas)
	}
	if cfg.MaxMsgs == 0 {
		cfg.MaxMsgs = -1
	}
	if cfg.MaxMsgsPer == 0 {
		cfg.MaxMsgsPer = -1
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = -1
	}
	if cfg.MaxMsgSize == 0 {
		cfg.MaxMsgSize = -1
	}
	if cfg.MaxConsumers == 0 {
		cfg.MaxConsumers = -1
	}
	if cfg.Duplicates == 0 {
		maxWindow := StreamDefaultDuplicatesWindow
		if lim.Duplicates > 0 && maxWindow > lim.Duplicates {
			maxWindow = lim.Duplicates
		}
		if cfg.MaxAge != 0 && cfg.MaxAge < maxWindow {
			cfg.Duplicates = cfg.MaxAge
		} else {
			cfg.Duplicates = maxWindow
		}
	}
	if cfg.Duplicates < 0 {
		return StreamConfig{}, fmt.Errorf("duplicates window can not be negative")
	}
	// Check that duplicates is not larger then age if set.
	if cfg.MaxAge != 0 && cfg.Duplicates > cfg.MaxAge {
		return StreamConfig{}, fmt.Errorf("duplicates window can not be larger then max age")
	}
	if lim.Duplicates > 0 && cfg.Duplicates > lim.Duplicates {
		return StreamConfig{}, fmt.Errorf("duplicates window can not be larger then server limit of %v", lim.Duplicates.String())
	}

	if cfg.DenyPurge && cfg.AllowRollup {
		return StreamConfig{}, fmt.Errorf("roll-ups require the purge permission")
	}

	if len(cfg.Subjects) == 0 {
		if cfg.Mirror == nil && len(cfg.Sources) == 0 {
			cfg.Subjects = append(cfg.Subjects, cfg.Name)
		}
	} else {
		if cfg.Mirror != nil {
			return StreamConfig{}, fmt.Errorf("stream mirrors may not have subjects")
		}

		// We can allow overlaps, but don't allow direct duplicates.
		dset := make(map[string]struct{}, len(cfg.Subjects))
		for _, subj := range cfg.Subjects {
			if _, ok := dset[subj]; ok {
				return StreamConfig{}, fmt.Errorf("duplicate subjects detected")
			}
			// Also check to make sure we do not overlap with our $JS API subjects.
			if subjectIsSubsetMatch(subj, "$JS.API.>") {
				return StreamConfig{}, fmt.Errorf("subjects overlap with jetstream api")
			}
			// Make sure the subject is valid.
			if !IsValidSubject(subj) {
				return StreamConfig{}, fmt.Errorf("invalid subject")
			}
			// Mark for duplicate check.
			dset[subj] = struct{}{}
		}
	}
	return cfg, nil
}

// Config returns the stream's configuration.
func (mset *stream) config() StreamConfig {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.cfg
}

func (mset *stream) fileStoreConfig() (FileStoreConfig, error) {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	fs, ok := mset.store.(*fileStore)
	if !ok {
		return FileStoreConfig{}, ErrStoreWrongType
	}
	return fs.fileStoreConfig(), nil
}

// Do not hold jsAccount or jetStream lock
func (jsa *jsAccount) configUpdateCheck(old, new *StreamConfig, lim *JSLimitOpts) (*StreamConfig, error) {
	cfg, err := checkStreamCfg(new, lim)
	if err != nil {
		return nil, NewJSStreamInvalidConfigError(err, Unless(err))
	}

	// Name must match.
	if cfg.Name != old.Name {
		return nil, NewJSStreamInvalidConfigError(fmt.Errorf("stream configuration name must match original"))
	}
	// Can't change MaxConsumers for now.
	if cfg.MaxConsumers != old.MaxConsumers {
		return nil, NewJSStreamInvalidConfigError(fmt.Errorf("stream configuration update can not change MaxConsumers"))
	}
	// Can't change storage types.
	if cfg.Storage != old.Storage {
		return nil, NewJSStreamInvalidConfigError(fmt.Errorf("stream configuration update can not change storage type"))
	}
	// Can't change retention.
	if cfg.Retention != old.Retention {
		return nil, NewJSStreamInvalidConfigError(fmt.Errorf("stream configuration update can not change retention policy"))
	}
	// Can not have a template owner for now.
	if old.Template != _EMPTY_ {
		return nil, NewJSStreamInvalidConfigError(fmt.Errorf("stream configuration update not allowed on template owned stream"))
	}
	if cfg.Template != _EMPTY_ {
		return nil, NewJSStreamInvalidConfigError(fmt.Errorf("stream configuration update can not be owned by a template"))
	}
	// Can not change from true to false.
	if !cfg.Sealed && old.Sealed {
		return nil, NewJSStreamInvalidConfigError(fmt.Errorf("stream configuration update can not unseal a sealed stream"))
	}
	// Can not change from true to false.
	if !cfg.DenyDelete && old.DenyDelete {
		return nil, NewJSStreamInvalidConfigError(fmt.Errorf("stream configuration update can not cancel deny message deletes"))
	}
	// Can not change from true to false.
	if !cfg.DenyPurge && old.DenyPurge {
		return nil, NewJSStreamInvalidConfigError(fmt.Errorf("stream configuration update can not cancel deny purge"))
	}

	// Do some adjustments for being sealed.
	if cfg.Sealed {
		cfg.MaxAge = 0
		cfg.Discard = DiscardNew
		cfg.DenyDelete, cfg.DenyPurge = true, true
		cfg.AllowRollup = false
	}

	// Check limits. We need some extra handling to allow updating MaxBytes.

	// First, let's calculate the difference between the new and old MaxBytes.
	maxBytesDiff := cfg.MaxBytes - old.MaxBytes
	if maxBytesDiff < 0 {
		// If we're updating to a lower MaxBytes (maxBytesDiff is negative),
		// then set to zero so checkBytesLimits doesn't set addBytes to 1.
		maxBytesDiff = 0
	}
	// If maxBytesDiff == 0, then that means MaxBytes didn't change.
	// If maxBytesDiff > 0, then we want to reserve additional bytes.

	// Save the user configured MaxBytes.
	newMaxBytes := cfg.MaxBytes

	maxBytesOffset := int64(0)
	if old.MaxBytes > 0 {
		if excessRep := cfg.Replicas - old.Replicas; excessRep > 0 {
			maxBytesOffset = old.MaxBytes * int64(excessRep)
		}
	}

	// We temporarily set cfg.MaxBytes to maxBytesDiff because checkAllLimits
	// adds cfg.MaxBytes to the current reserved limit and checks if we've gone
	// over. However, we don't want an addition cfg.MaxBytes, we only want to
	// reserve the difference between the new and the old values.
	cfg.MaxBytes = maxBytesDiff

	// Check limits.
	js, isClustered := jsa.jetStreamAndClustered()
	jsa.mu.RLock()
	acc := jsa.account
	selected, tier, hasTier := jsa.selectLimits(&cfg)
	if !hasTier && old.Replicas != cfg.Replicas {
		selected, tier, hasTier = jsa.selectLimits(old)
	}
	reserved := int64(0)
	if !isClustered {
		reserved = jsa.tieredReservation(tier, &cfg)
	}
	jsa.mu.RUnlock()
	if !hasTier {
		return nil, NewJSNoLimitsError()
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	if isClustered {
		_, reserved = tieredStreamAndReservationCount(js.cluster.streams[acc.Name], tier, &cfg)
	}
	// reservation does not account for this stream, hence add the old value
	reserved += int64(old.Replicas) * old.MaxBytes
	if err := js.checkAllLimits(&selected, &cfg, reserved, maxBytesOffset); err != nil {
		return nil, err
	}
	// Restore the user configured MaxBytes.
	cfg.MaxBytes = newMaxBytes
	return &cfg, nil
}

// Update will allow certain configuration properties of an existing stream to be updated.
func (mset *stream) update(config *StreamConfig) error {
	ocfg := mset.config()
	cfg, err := mset.jsa.configUpdateCheck(&ocfg, config, &mset.srv.getOpts().JetStreamLimits)
	if err != nil {
		return NewJSStreamInvalidConfigError(err, Unless(err))
	}

	mset.mu.Lock()
	if mset.isLeader() {
		// Now check for subject interest differences.
		current := make(map[string]struct{}, len(ocfg.Subjects))
		for _, s := range ocfg.Subjects {
			current[s] = struct{}{}
		}
		// Update config with new values. The store update will enforce any stricter limits.

		// Now walk new subjects. All of these need to be added, but we will check
		// the originals first, since if it is in there we can skip, already added.
		for _, s := range cfg.Subjects {
			if _, ok := current[s]; !ok {
				if _, err := mset.subscribeInternal(s, mset.processInboundJetStreamMsg); err != nil {
					mset.mu.Unlock()
					return err
				}
			}
			delete(current, s)
		}
		// What is left in current needs to be deleted.
		for s := range current {
			if err := mset.unsubscribeInternal(s); err != nil {
				mset.mu.Unlock()
				return err
			}
		}

		// Check for the Duplicates
		if cfg.Duplicates != ocfg.Duplicates && mset.ddtmr != nil {
			// Let it fire right away, it will adjust properly on purge.
			mset.ddtmr.Reset(time.Microsecond)
		}

		// Check for Sources.
		if len(cfg.Sources) > 0 || len(ocfg.Sources) > 0 {
			current := make(map[string]struct{})
			for _, s := range ocfg.Sources {
				current[s.iname] = struct{}{}
			}
			for _, s := range cfg.Sources {
				s.setIndexName()
				if _, ok := current[s.iname]; !ok {
					if mset.sources == nil {
						mset.sources = make(map[string]*sourceInfo)
					}
					mset.cfg.Sources = append(mset.cfg.Sources, s)
					qname := fmt.Sprintf("[ACC:%s] stream source '%s' from '%s' msgs", mset.acc.Name, mset.cfg.Name, s.Name)
					si := &sourceInfo{name: s.Name, iname: s.iname, msgs: mset.srv.newIPQueue(qname) /* of *inMsg */}
					mset.sources[s.iname] = si
					mset.setStartingSequenceForSource(s.iname)
					mset.setSourceConsumer(s.iname, si.sseq+1)
				}
				delete(current, s.Name)
			}
			// What is left in current needs to be deleted.
			for iname := range current {
				mset.cancelSourceConsumer(iname)
				delete(mset.sources, iname)
			}
		}
	}

	js := mset.js

	if targetTier := tierName(cfg); mset.tier != targetTier {
		// In cases such as R1->R3, only one update is needed
		if _, ok := mset.jsa.limits[targetTier]; ok {
			// error never set
			_, reported, _ := mset.store.Utilization()
			mset.jsa.updateUsage(mset.tier, mset.stype, -int64(reported))
			mset.jsa.updateUsage(targetTier, mset.stype, int64(reported))
			mset.tier = targetTier
		}
		// else in case the new tier does not exist (say on move), keep the old tier around
		// a subsequent update to an existing tier will then move from existing past tier to existing new tier
	}

	// Now update config and store's version of our config.
	mset.cfg = *cfg

	// If we are the leader never suppres update advisory, simply send.
	if mset.isLeader() {
		mset.sendUpdateAdvisoryLocked()
	}
	mset.mu.Unlock()

	if js != nil {
		maxBytesDiff := cfg.MaxBytes - ocfg.MaxBytes
		if maxBytesDiff > 0 {
			// Reserve the difference
			js.reserveStreamResources(&StreamConfig{
				MaxBytes: maxBytesDiff,
				Storage:  cfg.Storage,
			})
		} else if maxBytesDiff < 0 {
			// Release the difference
			js.releaseStreamResources(&StreamConfig{
				MaxBytes: -maxBytesDiff,
				Storage:  ocfg.Storage,
			})
		}
	}

	mset.store.UpdateConfig(cfg)

	return nil
}

// Purge will remove all messages from the stream and underlying store based on the request.
func (mset *stream) purge(preq *JSApiStreamPurgeRequest) (purged uint64, err error) {
	mset.mu.Lock()
	if mset.client == nil {
		mset.mu.Unlock()
		return 0, errors.New("invalid stream")
	}
	if mset.cfg.Sealed {
		return 0, errors.New("sealed stream")
	}
	var _obs [4]*consumer
	obs := _obs[:0]
	for _, o := range mset.consumers {
		if preq != nil && !o.isFilteredMatch(preq.Subject) {
			continue
		}
		obs = append(obs, o)
	}
	mset.mu.Unlock()

	if preq != nil {
		purged, err = mset.store.PurgeEx(preq.Subject, preq.Sequence, preq.Keep)
	} else {
		purged, err = mset.store.Purge()
	}
	if err != nil {
		return purged, err
	}

	// Purge consumers.
	var state StreamState
	mset.store.FastState(&state)
	fseq := state.FirstSeq
	lseq := state.LastSeq

	// Check for filtered purge.
	if preq != nil && preq.Subject != _EMPTY_ {
		ss := mset.store.FilteredState(state.FirstSeq, preq.Subject)
		fseq = ss.First
	}

	for _, o := range obs {
		o.purge(fseq, lseq)
	}
	return purged, nil
}

// RemoveMsg will remove a message from a stream.
// FIXME(dlc) - Should pick one and be consistent.
func (mset *stream) removeMsg(seq uint64) (bool, error) {
	return mset.deleteMsg(seq)
}

// DeleteMsg will remove a message from a stream.
func (mset *stream) deleteMsg(seq uint64) (bool, error) {
	mset.mu.RLock()
	if mset.client == nil {
		mset.mu.RUnlock()
		return false, fmt.Errorf("invalid stream")
	}
	mset.mu.RUnlock()
	return mset.store.RemoveMsg(seq)
}

// EraseMsg will securely remove a message and rewrite the data with random data.
func (mset *stream) eraseMsg(seq uint64) (bool, error) {
	mset.mu.RLock()
	if mset.client == nil {
		mset.mu.RUnlock()
		return false, fmt.Errorf("invalid stream")
	}
	mset.mu.RUnlock()
	return mset.store.EraseMsg(seq)
}

// Are we a mirror?
func (mset *stream) isMirror() bool {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.cfg.Mirror != nil
}

func (mset *stream) hasSources() bool {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return len(mset.sources) > 0
}

func (mset *stream) sourcesInfo() (sis []*StreamSourceInfo) {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	for _, si := range mset.sources {
		sis = append(sis, mset.sourceInfo(si))
	}
	return sis
}

func allSubjects(cfg *StreamConfig, acc *Account) ([]string, bool) {
	subjects := copyStrings(cfg.Subjects)
	var hasExt bool
	var seen map[string]bool

	if cfg.Mirror != nil {
		var subjs []string
		seen = make(map[string]bool)
		subjs, hasExt = acc.streamSourceSubjects(cfg.Mirror, seen)
		if len(subjs) > 0 {
			subjects = append(subjects, subjs...)
		}
	} else if len(cfg.Sources) > 0 {
		var subjs []string
		seen = make(map[string]bool)
		for _, si := range cfg.Sources {
			subjs, hasExt = acc.streamSourceSubjects(si, seen)
			if len(subjs) > 0 {
				subjects = append(subjects, subjs...)
			}
		}
	}

	return subjects, hasExt
}

// Return the subjects for a stream source.
func (a *Account) streamSourceSubjects(ss *StreamSource, seen map[string]bool) (subjects []string, hasExt bool) {
	if ss != nil && ss.External != nil {
		return nil, true
	}

	s, js, _ := a.getJetStreamFromAccount()

	if !s.JetStreamIsClustered() {
		return a.streamSourceSubjectsNotClustered(ss.Name, seen)
	} else {
		return js.streamSourceSubjectsClustered(a.Name, ss.Name, seen)
	}
}

func (js *jetStream) streamSourceSubjectsClustered(accountName, streamName string, seen map[string]bool) (subjects []string, hasExt bool) {
	if seen[streamName] {
		return nil, false
	}

	// We are clustered here so need to work through stream assignments.
	sa := js.streamAssignment(accountName, streamName)
	if sa == nil {
		return nil, false
	}
	seen[streamName] = true

	js.mu.RLock()
	cfg := sa.Config
	if len(cfg.Subjects) > 0 {
		subjects = append(subjects, cfg.Subjects...)
	}

	// Check if we need to keep going.
	var sources []*StreamSource
	if cfg.Mirror != nil {
		sources = append(sources, cfg.Mirror)
	} else if len(cfg.Sources) > 0 {
		sources = append(sources, cfg.Sources...)
	}
	js.mu.RUnlock()

	if len(sources) > 0 {
		var subjs []string
		if acc, err := js.srv.lookupAccount(accountName); err == nil {
			for _, ss := range sources {
				subjs, hasExt = acc.streamSourceSubjects(ss, seen)
				if len(subjs) > 0 {
					subjects = append(subjects, subjs...)
				}
				if hasExt {
					break
				}
			}
		}
	}

	return subjects, hasExt
}

func (a *Account) streamSourceSubjectsNotClustered(streamName string, seen map[string]bool) (subjects []string, hasExt bool) {
	if seen[streamName] {
		return nil, false
	}

	mset, err := a.lookupStream(streamName)
	if err != nil {
		return nil, false
	}
	seen[streamName] = true

	cfg := mset.config()
	if len(cfg.Subjects) > 0 {
		subjects = append(subjects, cfg.Subjects...)
	}

	var subjs []string
	if cfg.Mirror != nil {
		subjs, hasExt = a.streamSourceSubjects(cfg.Mirror, seen)
		if len(subjs) > 0 {
			subjects = append(subjects, subjs...)
		}
	} else if len(cfg.Sources) > 0 {
		for _, si := range cfg.Sources {
			subjs, hasExt = a.streamSourceSubjects(si, seen)
			if len(subjs) > 0 {
				subjects = append(subjects, subjs...)
			}
			if hasExt {
				break
			}
		}
	}
	return subjects, hasExt
}

// Lock should be held
func (mset *stream) sourceInfo(si *sourceInfo) *StreamSourceInfo {
	if si == nil {
		return nil
	}

	ssi := &StreamSourceInfo{Name: si.name, Lag: si.lag, Error: si.err}
	// If we have not heard from the source, set Active to -1.
	if si.last.IsZero() {
		ssi.Active = -1
	} else {
		ssi.Active = time.Since(si.last)
	}

	var ext *ExternalStream
	if mset.cfg.Mirror != nil {
		ext = mset.cfg.Mirror.External
	} else if ss := mset.streamSource(si.iname); ss != nil && ss.External != nil {
		ext = ss.External
	}
	if ext != nil {
		ssi.External = &ExternalStream{
			ApiPrefix:     ext.ApiPrefix,
			DeliverPrefix: ext.DeliverPrefix,
		}
	}
	return ssi
}

// Return our source info for our mirror.
func (mset *stream) mirrorInfo() *StreamSourceInfo {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.sourceInfo(mset.mirror)
}

const sourceHealthCheckInterval = 2 * time.Second

// Will run as a Go routine to process mirror consumer messages.
func (mset *stream) processMirrorMsgs() {
	s := mset.srv
	defer s.grWG.Done()
	defer func() {
		mset.mu.Lock()
		if mset.mirror != nil {
			mset.mirror.grr = false
			if mset.mirror.qch != nil {
				close(mset.mirror.qch)
				mset.mirror.qch = nil
			}
		}
		mset.mu.Unlock()
	}()

	// Grab stream quit channel.
	mset.mu.Lock()
	if mset.mirror == nil {
		mset.mu.Unlock()
		return
	}
	msgs, qch, siqch := mset.mirror.msgs, mset.qch, mset.mirror.qch
	// Set the last seen as now so that we don't fail at the first check.
	mset.mirror.last = time.Now()
	mset.mu.Unlock()

	t := time.NewTicker(sourceHealthCheckInterval)
	defer t.Stop()

	for {
		select {
		case <-s.quitCh:
			return
		case <-qch:
			return
		case <-siqch:
			return
		case <-msgs.ch:
			ims := msgs.pop()
			for _, imi := range ims {
				im := imi.(*inMsg)
				if !mset.processInboundMirrorMsg(im) {
					break
				}
			}
			msgs.recycle(&ims)
		case <-t.C:
			mset.mu.RLock()
			isLeader := mset.isLeader()
			stalled := mset.mirror != nil && time.Since(mset.mirror.last) > 3*sourceHealthCheckInterval
			mset.mu.RUnlock()
			// No longer leader.
			if !isLeader {
				mset.cancelMirrorConsumer()
				return
			}
			// We are stalled.
			if stalled {
				mset.retryMirrorConsumer()
			}
		}
	}
}

// Checks that the message is from our current direct consumer. We can not depend on sub comparison
// since cross account imports break.
func (si *sourceInfo) isCurrentSub(reply string) bool {
	return si.cname != _EMPTY_ && strings.HasPrefix(reply, jsAckPre) && si.cname == tokenAt(reply, 4)
}

// processInboundMirrorMsg handles processing messages bound for a stream.
func (mset *stream) processInboundMirrorMsg(m *inMsg) bool {
	mset.mu.Lock()
	if mset.mirror == nil {
		mset.mu.Unlock()
		return false
	}
	if !mset.isLeader() {
		mset.mu.Unlock()
		mset.cancelMirrorConsumer()
		return false
	}

	isControl := m.isControlMsg()

	// Ignore from old subscriptions.
	// The reason we can not just compare subs is that on cross account imports they will not match.
	if !mset.mirror.isCurrentSub(m.rply) && !isControl {
		mset.mu.Unlock()
		return false
	}

	mset.mirror.last = time.Now()
	node := mset.node

	// Check for heartbeats and flow control messages.
	if isControl {
		var needsRetry bool
		// Flow controls have reply subjects.
		if m.rply != _EMPTY_ {
			mset.handleFlowControl(mset.mirror, m)
		} else {
			// For idle heartbeats make sure we did not miss anything and check if we are considered stalled.
			if ldseq := parseInt64(getHeader(JSLastConsumerSeq, m.hdr)); ldseq > 0 && uint64(ldseq) != mset.mirror.dseq {
				needsRetry = true
			} else if fcReply := getHeader(JSConsumerStalled, m.hdr); len(fcReply) > 0 {
				// Other side thinks we are stalled, so send flow control reply.
				mset.outq.sendMsg(string(fcReply), nil)
			}
		}
		mset.mu.Unlock()
		if needsRetry {
			mset.retryMirrorConsumer()
		}
		return !needsRetry
	}

	sseq, dseq, dc, ts, pending := replyInfo(m.rply)

	if dc > 1 {
		mset.mu.Unlock()
		return false
	}

	// Mirror info tracking.
	olag, osseq, odseq := mset.mirror.lag, mset.mirror.sseq, mset.mirror.dseq
	if sseq == mset.mirror.sseq+1 {
		mset.mirror.dseq = dseq
		mset.mirror.sseq++
	} else if sseq <= mset.mirror.sseq {
		// Ignore older messages.
		mset.mu.Unlock()
		return true
	} else if mset.mirror.cname == _EMPTY_ {
		mset.mirror.cname = tokenAt(m.rply, 4)
		mset.mirror.dseq, mset.mirror.sseq = dseq, sseq
	} else {
		// If the deliver sequence matches then the upstream stream has expired or deleted messages.
		if dseq == mset.mirror.dseq+1 {
			mset.skipMsgs(mset.mirror.sseq+1, sseq-1)
			mset.mirror.dseq++
			mset.mirror.sseq = sseq
		} else {
			mset.mu.Unlock()
			mset.retryMirrorConsumer()
			return false
		}
	}

	if pending == 0 {
		mset.mirror.lag = 0
	} else {
		mset.mirror.lag = pending - 1
	}

	js, stype := mset.js, mset.cfg.Storage
	mset.mu.Unlock()

	s := mset.srv
	var err error
	if node != nil {
		if js.limitsExceeded(stype) {
			s.resourcesExeededError()
			err = ApiErrors[JSInsufficientResourcesErr]
		} else {
			err = node.Propose(encodeStreamMsg(m.subj, _EMPTY_, m.hdr, m.msg, sseq-1, ts))
		}
	} else {
		err = mset.processJetStreamMsg(m.subj, _EMPTY_, m.hdr, m.msg, sseq-1, ts)
	}
	if err != nil {
		if err == errLastSeqMismatch {
			// We may have missed messages, restart.
			if sseq <= mset.lastSeq() {
				mset.mu.Lock()
				mset.mirror.lag = olag
				mset.mirror.sseq = osseq
				mset.mirror.dseq = odseq
				mset.mu.Unlock()
				return false
			} else {
				mset.mu.Lock()
				mset.mirror.dseq = odseq
				mset.mirror.sseq = osseq
				mset.mu.Unlock()
				mset.retryMirrorConsumer()
			}
		} else {
			s.Warnf("Got error processing JetStream mirror msg: %v", err)
		}
		if strings.Contains(err.Error(), "no space left") {
			s.Errorf("JetStream out of space, will be DISABLED")
			s.DisableJetStream()
		}
	}
	return err == nil
}

func (mset *stream) setMirrorErr(err *ApiError) {
	mset.mu.Lock()
	if mset.mirror != nil {
		mset.mirror.err = err
	}
	mset.mu.Unlock()
}

func (mset *stream) cancelMirrorConsumer() {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	if mset.mirror == nil {
		return
	}
	if mset.mirror.sub != nil {
		mset.unsubscribe(mset.mirror.sub)
		mset.mirror.sub = nil
	}
	mset.removeInternalConsumer(mset.mirror)
	// If the go routine is still running close the quit chan.
	if mset.mirror.qch != nil {
		close(mset.mirror.qch)
		mset.mirror.qch = nil
	}
}

func (mset *stream) retryMirrorConsumer() error {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	mset.srv.Debugf("Retrying mirror consumer for '%s > %s'", mset.acc.Name, mset.cfg.Name)
	return mset.setupMirrorConsumer()
}

// Lock should be held.
func (mset *stream) skipMsgs(start, end uint64) {
	node, store := mset.node, mset.store
	var entries []*Entry
	for seq := start; seq <= end; seq++ {
		if node != nil {
			entries = append(entries, &Entry{EntryNormal, encodeStreamMsg(_EMPTY_, _EMPTY_, nil, nil, seq-1, 0)})
			// So a single message does not get too big.
			if len(entries) > 10_000 {
				node.ProposeDirect(entries)
				// We need to re-craete `entries` because there is a reference
				// to it in the node's pae map.
				entries = entries[:0]
			}
		} else {
			mset.lseq = store.SkipMsg()
		}
	}
	// Send all at once.
	if node != nil && len(entries) > 0 {
		node.ProposeDirect(entries)
	}
}

// Setup our mirror consumer.
// Lock should be held.
func (mset *stream) setupMirrorConsumer() error {
	if mset.outq == nil {
		return errors.New("outq required")
	}

	isReset := mset.mirror != nil

	// Reset
	if isReset {
		if mset.mirror.sub != nil {
			mset.unsubscribe(mset.mirror.sub)
			mset.mirror.sub = nil
			mset.mirror.dseq = 0
			mset.mirror.sseq = mset.lseq
		}
		// Make sure to delete any prior consumers if we know about them.
		mset.removeInternalConsumer(mset.mirror)

		// If we are no longer the leader stop trying.
		if !mset.isLeader() {
			return nil
		}
	}

	// Determine subjects etc.
	var deliverSubject string
	ext := mset.cfg.Mirror.External

	if ext != nil && ext.DeliverPrefix != _EMPTY_ {
		deliverSubject = strings.ReplaceAll(ext.DeliverPrefix+syncSubject(".M"), "..", ".")
	} else {
		deliverSubject = syncSubject("$JS.M")
	}

	if !isReset {
		qname := fmt.Sprintf("[ACC:%s] stream mirror '%s' of '%s' msgs", mset.acc.Name, mset.cfg.Name, mset.cfg.Mirror.Name)
		mset.mirror = &sourceInfo{name: mset.cfg.Mirror.Name, msgs: mset.srv.newIPQueue(qname) /* of *inMsg */}
	}

	if !mset.mirror.grr {
		mset.mirror.grr = true
		mset.mirror.qch = make(chan struct{})
		mset.srv.startGoRoutine(func() { mset.processMirrorMsgs() })
	}

	// We want to throttle here in terms of how fast we request new consumers.
	if time.Since(mset.mirror.lreq) < 2*time.Second {
		return nil
	}
	mset.mirror.lreq = time.Now()

	// Now send off request to create/update our consumer. This will be all API based even in single server mode.
	// We calculate durable names apriori so we do not need to save them off.

	var state StreamState
	mset.store.FastState(&state)

	req := &CreateConsumerRequest{
		Stream: mset.cfg.Mirror.Name,
		Config: ConsumerConfig{
			DeliverSubject: deliverSubject,
			DeliverPolicy:  DeliverByStartSequence,
			OptStartSeq:    state.LastSeq + 1,
			AckPolicy:      AckNone,
			AckWait:        22 * time.Hour,
			MaxDeliver:     1,
			Heartbeat:      sourceHealthCheckInterval,
			FlowControl:    true,
			Direct:         true,
		},
	}

	// Only use start optionals on first time.
	if state.Msgs == 0 && state.FirstSeq == 0 {
		req.Config.OptStartSeq = 0
		if mset.cfg.Mirror.OptStartSeq > 0 {
			req.Config.OptStartSeq = mset.cfg.Mirror.OptStartSeq
		} else if mset.cfg.Mirror.OptStartTime != nil {
			req.Config.OptStartTime = mset.cfg.Mirror.OptStartTime
			req.Config.DeliverPolicy = DeliverByStartTime
		}
	}
	if req.Config.OptStartSeq == 0 && req.Config.OptStartTime == nil {
		// If starting out and lastSeq is 0.
		req.Config.DeliverPolicy = DeliverAll
	}

	respCh := make(chan *JSApiConsumerCreateResponse, 1)
	reply := infoReplySubject()
	crSub, _ := mset.subscribeInternal(reply, func(sub *subscription, c *client, _ *Account, subject, reply string, rmsg []byte) {
		mset.unsubscribeUnlocked(sub)
		_, msg := c.msgParts(rmsg)

		var ccr JSApiConsumerCreateResponse
		if err := json.Unmarshal(msg, &ccr); err != nil {
			c.Warnf("JetStream bad mirror consumer create response: %q", msg)
			mset.cancelMirrorConsumer()
			mset.setMirrorErr(ApiErrors[JSInvalidJSONErr])
			return
		}
		respCh <- &ccr
	})

	b, _ := json.Marshal(req)
	subject := fmt.Sprintf(JSApiConsumerCreateT, mset.cfg.Mirror.Name)
	if ext != nil {
		subject = strings.Replace(subject, JSApiPrefix, ext.ApiPrefix, 1)
		subject = strings.ReplaceAll(subject, "..", ".")
	}

	mset.outq.send(newJSPubMsg(subject, _EMPTY_, reply, nil, b, nil, 0))

	go func() {
		select {
		case ccr := <-respCh:
			if ccr.Error != nil || ccr.ConsumerInfo == nil {
				mset.cancelMirrorConsumer()
			} else {
				mset.mu.Lock()
				// Mirror config has been removed.
				if mset.mirror == nil {
					mset.mu.Unlock()
					mset.cancelMirrorConsumer()
					return
				}

				// When an upstream stream expires messages or in general has messages that we want
				// that are no longer available we need to adjust here.
				var state StreamState
				mset.store.FastState(&state)

				// Check if we need to skip messages.
				if state.LastSeq != ccr.ConsumerInfo.Delivered.Stream {
					mset.skipMsgs(state.LastSeq+1, ccr.ConsumerInfo.Delivered.Stream)
				}

				// Capture consumer name.
				mset.mirror.cname = ccr.ConsumerInfo.Name
				msgs := mset.mirror.msgs

				// Process inbound mirror messages from the wire.
				sub, err := mset.subscribeInternal(deliverSubject, func(sub *subscription, c *client, _ *Account, subject, reply string, rmsg []byte) {
					hdr, msg := c.msgParts(copyBytes(rmsg)) // Need to copy.
					mset.queueInbound(msgs, subject, reply, hdr, msg)
				})
				if err != nil {
					mset.mirror.err = NewJSMirrorConsumerSetupFailedError(err, Unless(err))
					mset.mirror.sub = nil
					mset.mirror.cname = _EMPTY_
				} else {
					mset.mirror.err = nil
					mset.mirror.sub = sub
					mset.mirror.last = time.Now()
					mset.mirror.dseq = 0
					mset.mirror.sseq = ccr.ConsumerInfo.Delivered.Stream
				}
				mset.mu.Unlock()
			}
			mset.setMirrorErr(ccr.Error)
		case <-time.After(10 * time.Second):
			mset.unsubscribeUnlocked(crSub)
			return
		}
	}()

	return nil
}

func (mset *stream) streamSource(iname string) *StreamSource {
	for _, ssi := range mset.cfg.Sources {
		if ssi.iname == iname {
			return ssi
		}
	}
	return nil
}

func (mset *stream) retrySourceConsumer(sname string) {
	mset.mu.Lock()
	defer mset.mu.Unlock()

	si := mset.sources[sname]
	if si == nil {
		return
	}
	mset.setStartingSequenceForSource(sname)
	mset.retrySourceConsumerAtSeq(sname, si.sseq+1)
}

// Lock should be held.
func (mset *stream) retrySourceConsumerAtSeq(sname string, seq uint64) {
	if mset.client == nil {
		return
	}
	s := mset.srv

	s.Debugf("Retrying source consumer for '%s > %s'", mset.acc.Name, mset.cfg.Name)

	// No longer configured.
	if si := mset.sources[sname]; si == nil {
		return
	}
	mset.setSourceConsumer(sname, seq)
}

// Lock should be held.
func (mset *stream) cancelSourceConsumer(sname string) {
	if si := mset.sources[sname]; si != nil && si.sub != nil {
		mset.unsubscribe(si.sub)
		si.sub = nil
		si.sseq, si.dseq = 0, 0
		mset.removeInternalConsumer(si)
		// If the go routine is still running close the quit chan.
		if si.qch != nil {
			close(si.qch)
			si.qch = nil
		}
	}
}

// Lock should be held.
func (mset *stream) setSourceConsumer(iname string, seq uint64) {
	si := mset.sources[iname]
	if si == nil {
		return
	}
	if si.sub != nil {
		mset.unsubscribe(si.sub)
		si.sub = nil
	}
	// Need to delete the old one.
	mset.removeInternalConsumer(si)

	si.sseq, si.dseq = seq, 0
	si.last = time.Now()
	ssi := mset.streamSource(iname)
	if ssi == nil {
		return
	}

	// Determine subjects etc.
	var deliverSubject string
	ext := ssi.External

	if ext != nil && ext.DeliverPrefix != _EMPTY_ {
		deliverSubject = strings.ReplaceAll(ext.DeliverPrefix+syncSubject(".S"), "..", ".")
	} else {
		deliverSubject = syncSubject("$JS.S")
	}

	if !si.grr {
		si.grr = true
		si.qch = make(chan struct{})
		mset.srv.startGoRoutine(func() { mset.processSourceMsgs(si) })
	}

	// We want to throttle here in terms of how fast we request new consumers.
	if time.Since(si.lreq) < 2*time.Second {
		return
	}
	si.lreq = time.Now()

	req := &CreateConsumerRequest{
		Stream: si.name,
		Config: ConsumerConfig{
			DeliverSubject: deliverSubject,
			AckPolicy:      AckNone,
			AckWait:        22 * time.Hour,
			MaxDeliver:     1,
			Heartbeat:      sourceHealthCheckInterval,
			FlowControl:    true,
			Direct:         true,
		},
	}
	// If starting, check any configs.
	if seq <= 1 {
		if ssi.OptStartSeq > 0 {
			req.Config.OptStartSeq = ssi.OptStartSeq
			req.Config.DeliverPolicy = DeliverByStartSequence
		} else if ssi.OptStartTime != nil {
			req.Config.OptStartTime = ssi.OptStartTime
			req.Config.DeliverPolicy = DeliverByStartTime
		}
	} else {
		req.Config.OptStartSeq = seq
		req.Config.DeliverPolicy = DeliverByStartSequence
	}
	// Filters
	if ssi.FilterSubject != _EMPTY_ {
		req.Config.FilterSubject = ssi.FilterSubject
	}

	respCh := make(chan *JSApiConsumerCreateResponse, 1)
	reply := infoReplySubject()
	crSub, _ := mset.subscribeInternal(reply, func(sub *subscription, c *client, _ *Account, subject, reply string, rmsg []byte) {
		mset.unsubscribeUnlocked(sub)
		_, msg := c.msgParts(rmsg)
		var ccr JSApiConsumerCreateResponse
		if err := json.Unmarshal(msg, &ccr); err != nil {
			c.Warnf("JetStream bad source consumer create response: %q", msg)
			return
		}
		respCh <- &ccr
	})

	b, _ := json.Marshal(req)
	subject := fmt.Sprintf(JSApiConsumerCreateT, si.name)
	if ext != nil {
		subject = strings.Replace(subject, JSApiPrefix, ext.ApiPrefix, 1)
		subject = strings.ReplaceAll(subject, "..", ".")
	}

	mset.outq.send(newJSPubMsg(subject, _EMPTY_, reply, nil, b, nil, 0))

	go func() {
		select {
		case ccr := <-respCh:
			mset.mu.Lock()
			if si := mset.sources[iname]; si != nil {
				si.err = nil
				if ccr.Error != nil || ccr.ConsumerInfo == nil {
					mset.srv.Warnf("JetStream error response for create source consumer: %+v", ccr.Error)
					si.err = ccr.Error
					// We will retry every 10 seconds or so
					mset.cancelSourceConsumer(iname)
				} else {
					if si.sseq != ccr.ConsumerInfo.Delivered.Stream {
						si.sseq = ccr.ConsumerInfo.Delivered.Stream + 1
					}

					// Capture consumer name.
					si.cname = ccr.ConsumerInfo.Name
					// Now create sub to receive messages.
					sub, err := mset.subscribeInternal(deliverSubject, func(sub *subscription, c *client, _ *Account, subject, reply string, rmsg []byte) {
						hdr, msg := c.msgParts(copyBytes(rmsg)) // Need to copy.
						mset.queueInbound(si.msgs, subject, reply, hdr, msg)
					})
					if err != nil {
						si.err = NewJSSourceConsumerSetupFailedError(err, Unless(err))
						si.sub = nil
					} else {
						si.err = nil
						si.sub = sub
						si.last = time.Now()
					}
				}
			}
			mset.mu.Unlock()
		case <-time.After(10 * time.Second):
			mset.unsubscribeUnlocked(crSub)
			return
		}
	}()
}

func (mset *stream) processSourceMsgs(si *sourceInfo) {
	s := mset.srv
	defer s.grWG.Done()

	if si == nil {
		return
	}

	defer func() {
		mset.mu.Lock()
		si.grr = false
		if si.qch != nil {
			close(si.qch)
			si.qch = nil
		}
		mset.mu.Unlock()
	}()

	// Grab stream quit channel.
	mset.mu.Lock()
	msgs, qch, siqch := si.msgs, mset.qch, si.qch
	// Set the last seen as now so that we don't fail at the first check.
	si.last = time.Now()
	mset.mu.Unlock()

	t := time.NewTicker(sourceHealthCheckInterval)
	defer t.Stop()

	for {
		select {
		case <-s.quitCh:
			return
		case <-qch:
			return
		case <-siqch:
			return
		case <-msgs.ch:
			ims := msgs.pop()
			for _, imi := range ims {
				im := imi.(*inMsg)
				if !mset.processInboundSourceMsg(si, im) {
					break
				}
			}
			msgs.recycle(&ims)
		case <-t.C:
			mset.mu.RLock()
			iname, isLeader := si.iname, mset.isLeader()
			stalled := time.Since(si.last) > 3*sourceHealthCheckInterval
			mset.mu.RUnlock()
			// No longer leader.
			if !isLeader {
				mset.mu.Lock()
				mset.cancelSourceConsumer(iname)
				mset.mu.Unlock()
				return
			}
			// We are stalled.
			if stalled {
				mset.retrySourceConsumer(iname)
			}
		}
	}
}

// isControlMsg determines if this is a control message.
func (m *inMsg) isControlMsg() bool {
	return len(m.msg) == 0 && len(m.hdr) > 0 && bytes.HasPrefix(m.hdr, []byte("NATS/1.0 100 "))
}

// Sends a reply to a flow control request.
func (mset *stream) sendFlowControlReply(reply string) {
	mset.mu.Lock()
	if mset.isLeader() && mset.outq != nil {
		mset.outq.sendMsg(reply, nil)
	}
	mset.mu.Unlock()
}

// handleFlowControl will properly handle flow control messages for both R==1 and R>1.
// Lock should be held.
func (mset *stream) handleFlowControl(si *sourceInfo, m *inMsg) {
	// If we are clustered we will send the flow control message through the replication stack.
	if mset.isClustered() {
		mset.node.Propose(encodeStreamMsg(_EMPTY_, m.rply, m.hdr, nil, 0, 0))
	} else {
		mset.outq.sendMsg(m.rply, nil)
	}
}

// processInboundSourceMsg handles processing other stream messages bound for this stream.
func (mset *stream) processInboundSourceMsg(si *sourceInfo, m *inMsg) bool {
	mset.mu.Lock()

	// If we are no longer the leader cancel this subscriber.
	if !mset.isLeader() {
		mset.mu.Unlock()
		mset.cancelSourceConsumer(si.name)
		return false
	}

	isControl := m.isControlMsg()

	// Ignore from old subscriptions.
	if !si.isCurrentSub(m.rply) && !isControl {
		mset.mu.Unlock()
		return false
	}

	si.last = time.Now()
	node := mset.node

	// Check for heartbeats and flow control messages.
	if isControl {
		var needsRetry bool
		// Flow controls have reply subjects.
		if m.rply != _EMPTY_ {
			mset.handleFlowControl(si, m)
		} else {
			// For idle heartbeats make sure we did not miss anything.
			if ldseq := parseInt64(getHeader(JSLastConsumerSeq, m.hdr)); ldseq > 0 && uint64(ldseq) != si.dseq {
				needsRetry = true
				mset.retrySourceConsumerAtSeq(si.iname, si.sseq+1)
			} else if fcReply := getHeader(JSConsumerStalled, m.hdr); len(fcReply) > 0 {
				// Other side thinks we are stalled, so send flow control reply.
				mset.outq.sendMsg(string(fcReply), nil)
			}
		}
		mset.mu.Unlock()
		return !needsRetry
	}

	sseq, dseq, dc, _, pending := replyInfo(m.rply)

	if dc > 1 {
		mset.mu.Unlock()
		return false
	}

	// Tracking is done here.
	if dseq == si.dseq+1 {
		si.dseq++
		si.sseq = sseq
	} else if dseq > si.dseq {
		if si.cname == _EMPTY_ {
			si.cname = tokenAt(m.rply, 4)
			si.dseq, si.sseq = dseq, sseq
		} else {
			mset.retrySourceConsumerAtSeq(si.iname, si.sseq+1)
			mset.mu.Unlock()
			return false
		}
	} else {
		mset.mu.Unlock()
		return false
	}

	if pending == 0 {
		si.lag = 0
	} else {
		si.lag = pending - 1
	}
	mset.mu.Unlock()

	hdr, msg := m.hdr, m.msg

	// If we are daisy chained here make sure to remove the original one.
	if len(hdr) > 0 {
		hdr = removeHeaderIfPresent(hdr, JSStreamSource)
	}
	// Hold onto the origin reply which has all the metadata.
	hdr = genHeader(hdr, JSStreamSource, si.genSourceHeader(m.rply))

	var err error
	// If we are clustered we need to propose this message to the underlying raft group.
	if node != nil {
		err = mset.processClusteredInboundMsg(m.subj, _EMPTY_, hdr, msg)
	} else {
		err = mset.processJetStreamMsg(m.subj, _EMPTY_, hdr, msg, 0, 0)
	}

	if err != nil {
		s := mset.srv
		if err == errLastSeqMismatch {
			mset.cancelSourceConsumer(si.iname)
			mset.retrySourceConsumer(si.iname)
		} else {
			s.Warnf("JetStream got an error processing inbound source msg: %v", err)
		}
		if strings.Contains(err.Error(), "no space left") {
			s.Errorf("JetStream out of space, will be DISABLED")
			s.DisableJetStream()
		}
	}

	return true
}

// Generate a new style source header.
func (si *sourceInfo) genSourceHeader(reply string) string {
	var b strings.Builder
	b.WriteString(si.iname)
	b.WriteByte(' ')
	// Grab sequence as text here from reply subject.
	var tsa [expectedNumReplyTokens]string
	start, tokens := 0, tsa[:0]
	for i := 0; i < len(reply); i++ {
		if reply[i] == btsep {
			tokens, start = append(tokens, reply[start:i]), i+1
		}
	}
	tokens = append(tokens, reply[start:])
	seq := "1" // Default
	if len(tokens) == expectedNumReplyTokens && tokens[0] == "$JS" && tokens[1] == "ACK" {
		seq = tokens[5]
	}
	b.WriteString(seq)
	return b.String()
}

// Original version of header that stored ack reply direct.
func streamAndSeqFromAckReply(reply string) (string, uint64) {
	tsa := [expectedNumReplyTokens]string{}
	start, tokens := 0, tsa[:0]
	for i := 0; i < len(reply); i++ {
		if reply[i] == btsep {
			tokens, start = append(tokens, reply[start:i]), i+1
		}
	}
	tokens = append(tokens, reply[start:])
	if len(tokens) != expectedNumReplyTokens || tokens[0] != "$JS" || tokens[1] != "ACK" {
		return _EMPTY_, 0
	}
	return tokens[2], uint64(parseAckReplyNum(tokens[5]))
}

// Extract the stream (indexed name) and sequence from the source header.
func streamAndSeq(shdr string) (string, uint64) {
	if strings.HasPrefix(shdr, jsAckPre) {
		return streamAndSeqFromAckReply(shdr)
	}
	// New version which is stream index name <SPC> sequence
	fields := strings.Fields(shdr)
	if len(fields) != 2 {
		return _EMPTY_, 0
	}
	return fields[0], uint64(parseAckReplyNum(fields[1]))
}

// Lock should be held.
func (mset *stream) setStartingSequenceForSource(sname string) {
	si := mset.sources[sname]
	if si == nil {
		return
	}

	var state StreamState
	mset.store.FastState(&state)

	// Do not reset sseq here so we can remember when purge/expiration happens.
	if state.Msgs == 0 {
		si.dseq = 0
		return
	}

	var smv StoreMsg
	for seq := state.LastSeq; seq >= state.FirstSeq; seq-- {
		sm, err := mset.store.LoadMsg(seq, &smv)
		if err != nil || len(sm.hdr) == 0 {
			continue
		}
		ss := getHeader(JSStreamSource, sm.hdr)
		if len(ss) == 0 {
			continue
		}
		iname, sseq := streamAndSeq(string(ss))
		if iname == sname {
			si.sseq = sseq
			si.dseq = 0
			return
		}
	}
}

// Lock should be held.
// This will do a reverse scan on startup or leader election
// searching for the starting sequence number.
// This can be slow in degenerative cases.
// Lock should be held.
func (mset *stream) startingSequenceForSources() {
	if len(mset.cfg.Sources) == 0 {
		return
	}
	// Always reset here.
	mset.sources = make(map[string]*sourceInfo)

	for _, ssi := range mset.cfg.Sources {
		if ssi.iname == _EMPTY_ {
			ssi.setIndexName()
		}
		qname := fmt.Sprintf("[ACC:%s] stream source '%s' from '%s' msgs", mset.acc.Name, mset.cfg.Name, ssi.Name)
		si := &sourceInfo{name: ssi.Name, iname: ssi.iname, msgs: mset.srv.newIPQueue(qname) /* of *inMsg */}
		mset.sources[ssi.iname] = si
	}

	var state StreamState
	mset.store.FastState(&state)
	if state.Msgs == 0 {
		return
	}
	// For short circuiting return.
	expected := len(mset.cfg.Sources)
	seqs := make(map[string]uint64)

	// Stamp our si seq records on the way out.
	defer func() {
		for sname, seq := range seqs {
			// Ignore if not set.
			if seq == 0 {
				continue
			}
			if si := mset.sources[sname]; si != nil {
				si.sseq = seq
				si.dseq = 0
			}
		}
	}()

	var smv StoreMsg
	for seq := state.LastSeq; seq >= state.FirstSeq; seq-- {
		sm, err := mset.store.LoadMsg(seq, &smv)
		if err != nil || sm == nil || len(sm.hdr) == 0 {
			continue
		}
		ss := getHeader(JSStreamSource, sm.hdr)
		if len(ss) == 0 {
			continue
		}
		name, sseq := streamAndSeq(string(ss))
		// Only update active in case we have older ones in here that got configured out.
		if si := mset.sources[name]; si != nil {
			if _, ok := seqs[name]; !ok {
				seqs[name] = sseq
				if len(seqs) == expected {
					return
				}
			}
		}
	}
}

// Setup our source consumers.
// Lock should be held.
func (mset *stream) setupSourceConsumers() error {
	if mset.outq == nil {
		return errors.New("outq required")
	}
	// Reset if needed.
	for _, si := range mset.sources {
		if si.sub != nil {
			mset.cancelSourceConsumer(si.name)
		}
	}

	mset.startingSequenceForSources()

	// Setup our consumers at the proper starting position.
	for _, ssi := range mset.cfg.Sources {
		if si := mset.sources[ssi.iname]; si != nil {
			mset.setSourceConsumer(ssi.iname, si.sseq+1)
		}
	}

	return nil
}

// Will create internal subscriptions for the stream.
// Lock should be held.
func (mset *stream) subscribeToStream() error {
	if mset.active {
		return nil
	}
	for _, subject := range mset.cfg.Subjects {
		if _, err := mset.subscribeInternal(subject, mset.processInboundJetStreamMsg); err != nil {
			return err
		}
	}
	// Check if we need to setup mirroring.
	if mset.cfg.Mirror != nil {
		if err := mset.setupMirrorConsumer(); err != nil {
			return err
		}
	} else if len(mset.cfg.Sources) > 0 {
		if err := mset.setupSourceConsumers(); err != nil {
			return err
		}
	}

	mset.active = true
	return nil
}

// Stop our source consumers.
// Lock should be held.
func (mset *stream) stopSourceConsumers() {
	for _, si := range mset.sources {
		if si.sub != nil {
			mset.unsubscribe(si.sub)
		}
		// Need to delete the old one.
		mset.removeInternalConsumer(si)
		// If the go routine is still running close the quit chan.
		if si.qch != nil {
			close(si.qch)
			si.qch = nil
		}
		si.msgs.unregister()
	}
}

// Lock should be held.
func (mset *stream) removeInternalConsumer(si *sourceInfo) {
	if si == nil || si.cname == _EMPTY_ {
		return
	}
	si.cname = _EMPTY_
}

// Will unsubscribe from the stream.
// Lock should be held.
func (mset *stream) unsubscribeToStream() error {
	for _, subject := range mset.cfg.Subjects {
		mset.unsubscribeInternal(subject)
	}
	if mset.mirror != nil {
		if mset.mirror.sub != nil {
			mset.unsubscribe(mset.mirror.sub)
		}
		mset.removeInternalConsumer(mset.mirror)
		// If the go routine is still running close the quit chan.
		if mset.mirror.qch != nil {
			close(mset.mirror.qch)
		}
		mset.mirror.msgs.unregister()
		mset.mirror = nil
	}

	if len(mset.cfg.Sources) > 0 {
		mset.stopSourceConsumers()
	}

	mset.active = false
	return nil
}

// Lock should be held.
func (mset *stream) subscribeInternal(subject string, cb msgHandler) (*subscription, error) {
	c := mset.client
	if c == nil {
		return nil, fmt.Errorf("invalid stream")
	}
	if cb == nil {
		return nil, fmt.Errorf("undefined message handler")
	}

	mset.sid++

	// Now create the subscription
	return c.processSub([]byte(subject), nil, []byte(strconv.Itoa(mset.sid)), cb, false)
}

// Helper for unlocked stream.
func (mset *stream) subscribeInternalUnlocked(subject string, cb msgHandler) (*subscription, error) {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	return mset.subscribeInternal(subject, cb)
}

// This will unsubscribe us from the exact subject given.
// We do not currently track the subs so do not have the sid.
// This should be called only on an update.
// Lock should be held.
func (mset *stream) unsubscribeInternal(subject string) error {
	c := mset.client
	if c == nil {
		return fmt.Errorf("invalid stream")
	}

	var sid []byte
	c.mu.Lock()
	for _, sub := range c.subs {
		if subject == string(sub.subject) {
			sid = sub.sid
			break
		}
	}
	c.mu.Unlock()

	if sid != nil {
		return c.processUnsub(sid)
	}
	return nil
}

// Lock should be held.
func (mset *stream) unsubscribe(sub *subscription) {
	if sub == nil || mset.client == nil {
		return
	}
	mset.client.processUnsub(sub.sid)
}

func (mset *stream) unsubscribeUnlocked(sub *subscription) {
	mset.mu.Lock()
	mset.unsubscribe(sub)
	mset.mu.Unlock()
}

func (mset *stream) setupStore(fsCfg *FileStoreConfig) error {
	mset.mu.Lock()
	mset.created = time.Now().UTC()

	switch mset.cfg.Storage {
	case MemoryStorage:
		ms, err := newMemStore(&mset.cfg)
		if err != nil {
			mset.mu.Unlock()
			return err
		}
		mset.store = ms
	case FileStorage:
		s := mset.srv
		fs, err := newFileStoreWithCreated(*fsCfg, mset.cfg, mset.created, s.jsKeyGen(mset.acc.Name))
		if err != nil {
			mset.mu.Unlock()
			return err
		}
		mset.store = fs
	}
	mset.mu.Unlock()

	mset.store.RegisterStorageUpdates(mset.storeUpdates)

	return nil
}

// Called for any updates to the underlying stream. We pass through the bytes to the
// jetstream account. We do local processing for stream pending for consumers, but only
// for removals.
// Lock should not be held.
func (mset *stream) storeUpdates(md, bd int64, seq uint64, subj string) {
	// If we have a single negative update then we will process our consumers for stream pending.
	// Purge and Store handled separately inside individual calls.
	if md == -1 && seq > 0 {
		// We need to pull these out here and release the lock, even and RLock. RLocks are allowed to
		// be reentrant, however once anyone signals interest in a write lock any subsequent RLocks
		// will block. decStreamPending can try to re-acquire the RLock for this stream.
		var _cl [8]*consumer
		cl := _cl[:0]

		mset.mu.RLock()
		for _, o := range mset.consumers {
			cl = append(cl, o)
		}
		mset.mu.RUnlock()

		for _, o := range cl {
			o.decStreamPending(seq, subj)
		}
	}

	if mset.jsa != nil {
		mset.jsa.updateUsage(mset.tier, mset.stype, bd)
	}
}

// NumMsgIds returns the number of message ids being tracked for duplicate suppression.
func (mset *stream) numMsgIds() int {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	if !mset.ddloaded {
		mset.rebuildDedupe()
	}
	return len(mset.ddmap)
}

// checkMsgId will process and check for duplicates.
// Lock should be held.
func (mset *stream) checkMsgId(id string) *ddentry {
	if !mset.ddloaded {
		mset.rebuildDedupe()
	}
	if id == _EMPTY_ || len(mset.ddmap) == 0 {
		return nil
	}
	return mset.ddmap[id]
}

// Will purge the entries that are past the window.
// Should be called from a timer.
func (mset *stream) purgeMsgIds() {
	mset.mu.Lock()
	defer mset.mu.Unlock()

	now := time.Now().UnixNano()
	tmrNext := mset.cfg.Duplicates
	window := int64(tmrNext)

	for i, dde := range mset.ddarr[mset.ddindex:] {
		if now-dde.ts >= window {
			delete(mset.ddmap, dde.id)
		} else {
			mset.ddindex += i
			// Check if we should garbage collect here if we are 1/3 total size.
			if cap(mset.ddarr) > 3*(len(mset.ddarr)-mset.ddindex) {
				mset.ddarr = append([]*ddentry(nil), mset.ddarr[mset.ddindex:]...)
				mset.ddindex = 0
			}
			tmrNext = time.Duration(window - (now - dde.ts))
			break
		}
	}
	if len(mset.ddmap) > 0 {
		// Make sure to not fire too quick
		const minFire = 50 * time.Millisecond
		if tmrNext < minFire {
			tmrNext = minFire
		}
		if mset.ddtmr != nil {
			mset.ddtmr.Reset(tmrNext)
		} else {
			mset.ddtmr = time.AfterFunc(tmrNext, mset.purgeMsgIds)
		}
	} else {
		if mset.ddtmr != nil {
			mset.ddtmr.Stop()
			mset.ddtmr = nil
		}
		mset.ddmap = nil
		mset.ddarr = nil
		mset.ddindex = 0
	}
}

// storeMsgId will store the message id for duplicate detection.
func (mset *stream) storeMsgId(dde *ddentry) {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	mset.storeMsgIdLocked(dde)
}

// storeMsgIdLocked will store the message id for duplicate detection.
// Lock should he held.
func (mset *stream) storeMsgIdLocked(dde *ddentry) {
	if mset.ddmap == nil {
		mset.ddmap = make(map[string]*ddentry)
	}
	mset.ddmap[dde.id] = dde
	mset.ddarr = append(mset.ddarr, dde)
	if mset.ddtmr == nil {
		mset.ddtmr = time.AfterFunc(mset.cfg.Duplicates, mset.purgeMsgIds)
	}
}

// Fast lookup of msgId.
func getMsgId(hdr []byte) string {
	return string(getHeader(JSMsgId, hdr))
}

// Fast lookup of expected last msgId.
func getExpectedLastMsgId(hdr []byte) string {
	return string(getHeader(JSExpectedLastMsgId, hdr))
}

// Fast lookup of expected stream.
func getExpectedStream(hdr []byte) string {
	return string(getHeader(JSExpectedStream, hdr))
}

// Fast lookup of expected stream.
func getExpectedLastSeq(hdr []byte) uint64 {
	bseq := getHeader(JSExpectedLastSeq, hdr)
	if len(bseq) == 0 {
		return 0
	}
	return uint64(parseInt64(bseq))
}

// Fast lookup of rollups.
func getRollup(hdr []byte) string {
	r := getHeader(JSMsgRollup, hdr)
	if len(r) == 0 {
		return _EMPTY_
	}
	return strings.ToLower(string(r))
}

// Fast lookup of expected stream sequence per subject.
func getExpectedLastSeqPerSubject(hdr []byte) (uint64, bool) {
	bseq := getHeader(JSExpectedLastSubjSeq, hdr)
	if len(bseq) == 0 {
		return 0, false
	}
	return uint64(parseInt64(bseq)), true
}

// Lock should be held.
func (mset *stream) isClustered() bool {
	return mset.node != nil
}

// Used if we have to queue things internally to avoid the route/gw path.
type inMsg struct {
	subj string
	rply string
	hdr  []byte
	msg  []byte
}

func (mset *stream) queueInbound(ib *ipQueue, subj, rply string, hdr, msg []byte) {
	ib.push(&inMsg{subj, rply, hdr, msg})
}

func (mset *stream) queueInboundMsg(subj, rply string, hdr, msg []byte) {
	// Copy these.
	if len(hdr) > 0 {
		hdr = copyBytes(hdr)
	}
	if len(msg) > 0 {
		msg = copyBytes(msg)
	}
	mset.queueInbound(mset.msgs, subj, rply, hdr, msg)
}

// processInboundJetStreamMsg handles processing messages bound for a stream.
func (mset *stream) processInboundJetStreamMsg(_ *subscription, c *client, _ *Account, subject, reply string, rmsg []byte) {
	mset.mu.RLock()
	isLeader, isClustered, isSealed := mset.isLeader(), mset.node != nil, mset.cfg.Sealed
	mset.mu.RUnlock()

	// If we are not the leader just ignore.
	if !isLeader {
		return
	}

	if isSealed {
		var resp = JSPubAckResponse{
			PubAck: &PubAck{Stream: mset.name()},
			Error:  NewJSStreamSealedError(),
		}
		b, _ := json.Marshal(resp)
		mset.outq.sendMsg(reply, b)
		return
	}

	hdr, msg := c.msgParts(rmsg)

	// If we are not receiving directly from a client we should move this to another Go routine.
	if c.kind != CLIENT {
		mset.queueInboundMsg(subject, reply, hdr, msg)
		return
	}

	// If we are clustered we need to propose this message to the underlying raft group.
	if isClustered {
		mset.processClusteredInboundMsg(subject, reply, hdr, msg)
	} else {
		mset.processJetStreamMsg(subject, reply, hdr, msg, 0, 0)
	}
}

var (
	errLastSeqMismatch = errors.New("last sequence mismatch")
	errMsgIdDuplicate  = errors.New("msgid is duplicate")
)

// processJetStreamMsg is where we try to actually process the stream msg.
func (mset *stream) processJetStreamMsg(subject, reply string, hdr, msg []byte, lseq uint64, ts int64) error {
	mset.mu.Lock()
	store := mset.store
	c, s := mset.client, mset.srv
	if c == nil {
		mset.mu.Unlock()
		return nil
	}

	var accName string
	if mset.acc != nil {
		accName = mset.acc.Name
	}

	js, jsa, doAck := mset.js, mset.jsa, !mset.cfg.NoAck
	name, stype := mset.cfg.Name, mset.cfg.Storage
	maxMsgSize := int(mset.cfg.MaxMsgSize)
	numConsumers := len(mset.consumers)
	interestRetention := mset.cfg.Retention == InterestPolicy
	// Snapshot if we are the leader and if we can respond.
	isLeader := mset.isLeader()
	canRespond := doAck && len(reply) > 0 && isLeader

	var resp = &JSPubAckResponse{}

	var buf [256]byte
	pubAck := append(buf[:0], mset.pubAck...)

	// For clustering the lower layers will pass our expected lseq. If it is present check for that here.
	if lseq > 0 && lseq != (mset.lseq+mset.clfs) {
		isMisMatch := true
		// We may be able to recover here if we have no state whatsoever, or we are a mirror.
		// See if we have to adjust our starting sequence.
		if mset.lseq == 0 || mset.cfg.Mirror != nil {
			var state StreamState
			mset.store.FastState(&state)
			if state.FirstSeq == 0 {
				mset.store.Compact(lseq + 1)
				mset.lseq = lseq
				isMisMatch = false
			}
		}
		// Really is a mismatch.
		if isMisMatch {
			outq := mset.outq
			mset.mu.Unlock()
			if canRespond && outq != nil {
				resp.PubAck = &PubAck{Stream: name}
				resp.Error = ApiErrors[JSStreamSequenceNotMatchErr]
				b, _ := json.Marshal(resp)
				outq.sendMsg(reply, b)
			}
			return errLastSeqMismatch
		}
	}

	// If we have received this message across an account we may have request information attached.
	// For now remove. TODO(dlc) - Should this be opt-in or opt-out?
	if len(hdr) > 0 {
		hdr = removeHeaderIfPresent(hdr, ClientInfoHdr)
	}

	// Process additional msg headers if still present.
	var msgId string
	var rollupSub, rollupAll bool

	if len(hdr) > 0 {
		outq := mset.outq

		// Dedupe detection.
		if msgId = getMsgId(hdr); msgId != _EMPTY_ {
			if dde := mset.checkMsgId(msgId); dde != nil {
				mset.clfs++
				mset.mu.Unlock()
				if canRespond {
					response := append(pubAck, strconv.FormatUint(dde.seq, 10)...)
					response = append(response, ",\"duplicate\": true}"...)
					outq.sendMsg(reply, response)
				}
				return errMsgIdDuplicate
			}
		}

		// Expected stream.
		if sname := getExpectedStream(hdr); sname != _EMPTY_ && sname != name {
			mset.clfs++
			mset.mu.Unlock()
			if canRespond {
				resp.PubAck = &PubAck{Stream: name}
				resp.Error = NewJSStreamNotMatchError()
				b, _ := json.Marshal(resp)
				outq.sendMsg(reply, b)
			}
			return errors.New("expected stream does not match")
		}
		// Expected last sequence.
		if seq := getExpectedLastSeq(hdr); seq > 0 && seq != mset.lseq {
			mlseq := mset.lseq
			mset.clfs++
			mset.mu.Unlock()
			if canRespond {
				resp.PubAck = &PubAck{Stream: name}
				resp.Error = NewJSStreamWrongLastSequenceError(mlseq)
				b, _ := json.Marshal(resp)
				outq.sendMsg(reply, b)
			}
			return fmt.Errorf("last sequence mismatch: %d vs %d", seq, mlseq)
		}
		// Expected last msgId.
		if lmsgId := getExpectedLastMsgId(hdr); lmsgId != _EMPTY_ {
			if mset.lmsgId == _EMPTY_ && !mset.ddloaded {
				mset.rebuildDedupe()
			}
			if lmsgId != mset.lmsgId {
				last := mset.lmsgId
				mset.clfs++
				mset.mu.Unlock()
				if canRespond {
					resp.PubAck = &PubAck{Stream: name}
					resp.Error = NewJSStreamWrongLastMsgIDError(last)
					b, _ := json.Marshal(resp)
					outq.sendMsg(reply, b)
				}
				return fmt.Errorf("last msgid mismatch: %q vs %q", lmsgId, last)
			}
		}
		// Expected last sequence per subject.
		if seq, exists := getExpectedLastSeqPerSubject(hdr); exists {
			// TODO(dlc) - We could make a new store func that does this all in one.
			var smv StoreMsg
			var fseq uint64
			sm, err := mset.store.LoadLastMsg(subject, &smv)
			if sm != nil {
				fseq = sm.seq
			}
			// If seq passed in is zero that signals we expect no msg to be present.
			if err == ErrStoreMsgNotFound && seq == 0 {
				fseq, err = 0, nil
			}
			if err != nil || fseq != seq {
				mset.clfs++
				mset.mu.Unlock()
				if canRespond {
					resp.PubAck = &PubAck{Stream: name}
					resp.Error = NewJSStreamWrongLastSequenceError(fseq)
					b, _ := json.Marshal(resp)
					outq.sendMsg(reply, b)
				}
				return fmt.Errorf("last sequence by subject mismatch: %d vs %d", seq, fseq)
			}
		}
		// Check for any rollups.
		if rollup := getRollup(hdr); rollup != _EMPTY_ {
			if !mset.cfg.AllowRollup || mset.cfg.DenyPurge {
				mset.clfs++
				mset.mu.Unlock()
				if canRespond {
					resp.PubAck = &PubAck{Stream: name}
					resp.Error = NewJSStreamRollupFailedError(errors.New("rollup not permitted"))
					b, _ := json.Marshal(resp)
					outq.sendMsg(reply, b)
				}
				return errors.New("rollup not permitted")
			}
			switch rollup {
			case JSMsgRollupSubject:
				rollupSub = true
			case JSMsgRollupAll:
				rollupAll = true
			default:
				mset.mu.Unlock()
				return fmt.Errorf("rollup value invalid: %q", rollup)
			}
		}
	}

	// Response Ack.
	var (
		response []byte
		seq      uint64
		err      error
	)

	// Check to see if we are over the max msg size.
	if maxMsgSize >= 0 && (len(hdr)+len(msg)) > maxMsgSize {
		mset.clfs++
		mset.mu.Unlock()
		if canRespond {
			resp.PubAck = &PubAck{Stream: name}
			resp.Error = NewJSStreamMessageExceedsMaximumError()
			b, _ := json.Marshal(resp)
			mset.outq.sendMsg(reply, b)
		}
		return ErrMaxPayload
	}

	if len(hdr) > math.MaxUint16 {
		mset.clfs++
		mset.mu.Unlock()
		if canRespond {
			resp.PubAck = &PubAck{Stream: name}
			resp.Error = NewJSStreamHeaderExceedsMaximumError()
			b, _ := json.Marshal(resp)
			mset.outq.sendMsg(reply, b)
		}
		return ErrMaxPayload
	}

	// Check to see if we have exceeded our limits.
	if js.limitsExceeded(stype) {
		s.resourcesExeededError()
		mset.clfs++
		mset.mu.Unlock()
		if canRespond {
			resp.PubAck = &PubAck{Stream: name}
			resp.Error = NewJSInsufficientResourcesError()
			b, _ := json.Marshal(resp)
			mset.outq.sendMsg(reply, b)
		}
		// Stepdown regardless.
		if node := mset.raftNode(); node != nil {
			node.StepDown()
		}
		return NewJSInsufficientResourcesError()
	}

	var noInterest bool

	// If we are interest based retention and have no consumers then we can skip.
	if interestRetention {
		if numConsumers == 0 {
			noInterest = true
		} else if mset.numFilter > 0 {
			// Assume no interest and check to disqualify.
			noInterest = true
			for _, o := range mset.consumers {
				if o.cfg.FilterSubject == _EMPTY_ || subjectIsSubsetMatch(subject, o.cfg.FilterSubject) {
					noInterest = false
					break
				}
			}
		}
	}

	// Grab timestamp if not already set.
	if ts == 0 && lseq > 0 {
		ts = time.Now().UnixNano()
	}

	// Skip msg here.
	if noInterest {
		mset.lseq = store.SkipMsg()
		mset.lmsgId = msgId
		mset.mu.Unlock()

		if canRespond {
			response = append(pubAck, strconv.FormatUint(mset.lseq, 10)...)
			response = append(response, '}')
			mset.outq.sendMsg(reply, response)
		}
		// If we have a msgId make sure to save.
		if msgId != _EMPTY_ {
			mset.storeMsgId(&ddentry{msgId, seq, ts})
		}
		return nil
	}

	// If here we will attempt to store the message.
	// Assume this will succeed.
	olmsgId := mset.lmsgId
	mset.lmsgId = msgId
	clfs := mset.clfs
	mset.lseq++
	tierName := mset.tier
	// We hold the lock to this point to make sure nothing gets between us since we check for pre-conditions.
	// Currently can not hold while calling store b/c we have inline storage update calls that may need the lock.
	// Note that upstream that sets seq/ts should be serialized as much as possible.
	mset.mu.Unlock()

	// Store actual msg.
	if lseq == 0 && ts == 0 {
		seq, ts, err = store.StoreMsg(subject, hdr, msg)
	} else {
		// Make sure to take into account any message assignments that we had to skip (clfs).
		seq = lseq + 1 - clfs
		err = store.StoreRawMsg(subject, hdr, msg, seq, ts)
	}

	if err != nil {
		// If we did not succeed put those values back and increment clfs in case we are clustered.
		mset.mu.Lock()
		var state StreamState
		mset.store.FastState(&state)
		mset.lseq = state.LastSeq
		mset.lmsgId = olmsgId
		mset.clfs++
		mset.mu.Unlock()

		switch err {
		case ErrMaxMsgs, ErrMaxBytes, ErrMaxMsgsPerSubject, ErrMsgTooLarge:
			s.Debugf("JetStream failed to store a msg on stream '%s > %s': %v", accName, name, err)
		case ErrStoreClosed:
		default:
			s.Errorf("JetStream failed to store a msg on stream '%s > %s': %v", accName, name, err)
		}

		if canRespond {
			resp.PubAck = &PubAck{Stream: name}
			resp.Error = NewJSStreamStoreFailedError(err, Unless(err))
			response, _ = json.Marshal(resp)
		}
	} else if jsa.limitsExceeded(stype, tierName) {
		s.Warnf("JetStream resource limits exceeded for account: %q", accName)
		if canRespond {
			resp.PubAck = &PubAck{Stream: name}
			resp.Error = NewJSAccountResourcesExceededError()
			response, _ = json.Marshal(resp)
		}
		// If we did not succeed put those values back.
		mset.mu.Lock()
		var state StreamState
		mset.store.FastState(&state)
		mset.lseq = state.LastSeq
		mset.lmsgId = olmsgId
		mset.mu.Unlock()
		store.RemoveMsg(seq)
		seq = 0
	} else {
		// No errors, this is the normal path.
		// If we have a msgId make sure to save.
		if msgId != _EMPTY_ {
			mset.storeMsgId(&ddentry{msgId, seq, ts})
		}
		if rollupSub {
			mset.purge(&JSApiStreamPurgeRequest{Subject: subject, Keep: 1})
		} else if rollupAll {
			mset.purge(&JSApiStreamPurgeRequest{Keep: 1})
		}
		if canRespond {
			response = append(pubAck, strconv.FormatUint(seq, 10)...)
			response = append(response, '}')
		}
	}

	// Send response here.
	if canRespond {
		mset.outq.sendMsg(reply, response)
	}

	if err == nil && seq > 0 && numConsumers > 0 {
		mset.mu.Lock()
		for _, o := range mset.consumers {
			o.mu.Lock()
			if o.isLeader() {
				if seq > o.lsgap && o.isFilteredMatch(subject) {
					o.sgap++
				}
				o.signalNewMessages()
			}
			o.mu.Unlock()
		}
		mset.mu.Unlock()
	}

	return err
}

// Internal message for use by jetstream subsystem.
type jsPubMsg struct {
	dsubj string // Subject to send to, e.g. _INBOX.xxx
	reply string
	StoreMsg
	o *consumer
}

var jsPubMsgPool sync.Pool

func newJSPubMsg(dsubj, subj, reply string, hdr, msg []byte, o *consumer, seq uint64) *jsPubMsg {
	var m *jsPubMsg
	var buf []byte
	pm := jsPubMsgPool.Get()
	if pm != nil {
		m = pm.(*jsPubMsg)
		buf = m.buf[:0]
	} else {
		m = new(jsPubMsg)
	}
	// When getting something from a pool it is criticical that all fields are
	// initialized. Doing this way guarantees that if someone adds a field to
	// the structure, the compiler will fail the build if this line is not updated.
	(*m) = jsPubMsg{dsubj, reply, StoreMsg{subj, hdr, msg, buf, seq, 0}, o}

	return m
}

// Gets a jsPubMsg from the pool.
func getJSPubMsgFromPool() *jsPubMsg {
	pm := jsPubMsgPool.Get()
	if pm != nil {
		return pm.(*jsPubMsg)
	}
	return new(jsPubMsg)
}

func (pm *jsPubMsg) returnToPool() {
	if pm == nil {
		return
	}
	pm.subj, pm.dsubj, pm.reply, pm.hdr, pm.msg, pm.o = _EMPTY_, _EMPTY_, _EMPTY_, nil, nil, nil
	if len(pm.buf) > 0 {
		pm.buf = pm.buf[:0]
	}
	jsPubMsgPool.Put(pm)
}

func (pm *jsPubMsg) size() int {
	if pm == nil {
		return 0
	}
	return len(pm.dsubj) + len(pm.reply) + len(pm.hdr) + len(pm.msg)
}

// Queue of *jsPubMsg for sending internal system messages.
type jsOutQ struct {
	*ipQueue
}

func (q *jsOutQ) sendMsg(subj string, msg []byte) {
	if q != nil {
		q.send(newJSPubMsg(subj, _EMPTY_, _EMPTY_, nil, msg, nil, 0))
	}
}

func (q *jsOutQ) send(msg *jsPubMsg) {
	if q == nil || msg == nil {
		return
	}
	q.push(msg)
}

func (q *jsOutQ) unregister() {
	if q == nil {
		return
	}
	q.ipQueue.unregister()
}

// StoredMsg is for raw access to messages in a stream.
type StoredMsg struct {
	Subject  string    `json:"subject"`
	Sequence uint64    `json:"seq"`
	Header   []byte    `json:"hdrs,omitempty"`
	Data     []byte    `json:"data,omitempty"`
	Time     time.Time `json:"time"`
}

// This is similar to system semantics but did not want to overload the single system sendq,
// or require system account when doing simple setup with jetstream.
func (mset *stream) setupSendCapabilities() {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	if mset.outq != nil {
		return
	}
	qname := fmt.Sprintf("[ACC:%s] stream '%s' sendQ", mset.acc.Name, mset.cfg.Name)
	mset.outq = &jsOutQ{mset.srv.newIPQueue(qname)} // of *jsPubMsg
	go mset.internalLoop()
}

// Returns the associated account name.
func (mset *stream) accName() string {
	if mset == nil {
		return _EMPTY_
	}
	mset.mu.RLock()
	acc := mset.acc
	mset.mu.RUnlock()
	return acc.Name
}

// Name returns the stream name.
func (mset *stream) name() string {
	if mset == nil {
		return _EMPTY_
	}
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.cfg.Name
}

func (mset *stream) internalLoop() {
	mset.mu.RLock()
	s := mset.srv
	c := s.createInternalJetStreamClient()
	c.registerWithAccount(mset.acc)
	defer c.closeConnection(ClientClosed)
	outq, qch, msgs := mset.outq, mset.qch, mset.msgs

	// For the ack msgs queue for interest retention.
	var (
		amch chan struct{}
		ackq *ipQueue // of uint64
	)
	if mset.ackq != nil {
		ackq, amch = mset.ackq, mset.ackq.ch
	}
	mset.mu.RUnlock()

	// Raw scratch buffer.
	// This should be rarely used now so can be smaller.
	var _r [1024]byte

	for {
		select {
		case <-outq.ch:
			pms := outq.pop()
			for _, pmi := range pms {
				pm := pmi.(*jsPubMsg)
				c.pa.subject = []byte(pm.dsubj)
				c.pa.deliver = []byte(pm.subj)
				c.pa.size = len(pm.msg) + len(pm.hdr)
				c.pa.szb = []byte(strconv.Itoa(c.pa.size))
				c.pa.reply = []byte(pm.reply)

				// If we have an underlying buf that is the wire contents for hdr + msg, else construct on the fly.
				var msg []byte
				if len(pm.buf) > 0 {
					msg = pm.buf
				} else {
					if len(pm.hdr) > 0 {
						msg = pm.hdr
						if len(pm.msg) > 0 {
							msg = _r[:0]
							msg = append(msg, pm.hdr...)
							msg = append(msg, pm.msg...)
						}
					} else if len(pm.msg) > 0 {
						// We own this now from a low level buffer perspective so can use directly here.
						msg = pm.msg
					}
				}

				if len(pm.hdr) > 0 {
					c.pa.hdr = len(pm.hdr)
					c.pa.hdb = []byte(strconv.Itoa(c.pa.hdr))
				} else {
					c.pa.hdr = -1
					c.pa.hdb = nil
				}

				msg = append(msg, _CRLF_...)

				didDeliver, _ := c.processInboundClientMsg(msg)
				c.pa.szb, c.pa.subject, c.pa.deliver = nil, nil, nil

				// Check to see if this is a delivery for a consumer and
				// we failed to deliver the message. If so alert the consumer.
				if pm.o != nil && pm.seq > 0 && !didDeliver {
					pm.o.didNotDeliver(pm.seq)
				}
				pm.returnToPool()
			}
			// TODO: Move in the for-loop?
			c.flushClients(0)
			outq.recycle(&pms)
		case <-msgs.ch:
			// This can possibly change now so needs to be checked here.
			mset.mu.RLock()
			isClustered := mset.node != nil
			mset.mu.RUnlock()

			ims := msgs.pop()
			for _, imi := range ims {
				im := imi.(*inMsg)
				// If we are clustered we need to propose this message to the underlying raft group.
				if isClustered {
					mset.processClusteredInboundMsg(im.subj, im.rply, im.hdr, im.msg)
				} else {
					mset.processJetStreamMsg(im.subj, im.rply, im.hdr, im.msg, 0, 0)
				}
			}
			msgs.recycle(&ims)
		case <-amch:
			seqs := ackq.pop()
			for _, seq := range seqs {
				mset.ackMsg(nil, seq.(uint64))
			}
			ackq.recycle(&seqs)
		case <-qch:
			return
		case <-s.quitCh:
			return
		}
	}
}

// Internal function to delete a stream.
func (mset *stream) delete() error {
	if mset == nil {
		return nil
	}
	return mset.stop(true, true)
}

// Internal function to stop or delete the stream.
func (mset *stream) stop(deleteFlag, advisory bool) error {
	mset.mu.RLock()
	js, jsa := mset.js, mset.jsa
	mset.mu.RUnlock()

	if jsa == nil {
		return NewJSNotEnabledForAccountError()
	}

	// Remove from our account map.
	jsa.mu.Lock()
	delete(jsa.streams, mset.cfg.Name)
	jsa.mu.Unlock()

	// Clean up consumers.
	mset.mu.Lock()
	var obs []*consumer
	for _, o := range mset.consumers {
		obs = append(obs, o)
	}

	// Check if we are a mirror.
	if mset.mirror != nil && mset.mirror.sub != nil {
		mset.unsubscribe(mset.mirror.sub)
		mset.mirror.sub = nil
		mset.removeInternalConsumer(mset.mirror)
	}
	// Now check for sources.
	if len(mset.sources) > 0 {
		for _, si := range mset.sources {
			mset.cancelSourceConsumer(si.iname)
		}
	}

	mset.mu.Unlock()
	for _, o := range obs {
		// Third flag says do not broadcast a signal.
		// TODO(dlc) - If we have an err here we don't want to stop
		// but should we log?
		o.stopWithFlags(deleteFlag, deleteFlag, false, advisory)
	}
	mset.mu.Lock()

	// Stop responding to sync requests.
	mset.stopClusterSubs()
	// Unsubscribe from direct stream.
	mset.unsubscribeToStream()

	// Our info sub if we spun it up.
	if mset.infoSub != nil {
		mset.srv.sysUnsubscribe(mset.infoSub)
		mset.infoSub = nil
	}

	// Cluster cleanup
	if n := mset.node; n != nil {
		if deleteFlag {
			n.Delete()
		} else {
			n.Stop()
		}
	}

	// Send stream delete advisory after the consumers.
	if deleteFlag && advisory {
		mset.sendDeleteAdvisoryLocked()
	}

	// Quit channel, do this after sending the delete advisory
	if mset.qch != nil {
		close(mset.qch)
		mset.qch = nil
	}

	c := mset.client
	mset.client = nil
	if c == nil {
		mset.mu.Unlock()
		return nil
	}

	// Cleanup duplicate timer if running.
	if mset.ddtmr != nil {
		mset.ddtmr.Stop()
		mset.ddtmr = nil
		mset.ddmap = nil
		mset.ddarr = nil
		mset.ddindex = 0
	}

	sysc := mset.sysc
	mset.sysc = nil

	if deleteFlag {
		// Unregistering ipQueues do not prevent them from push/pop
		// just will remove them from the central monitoring map
		mset.msgs.unregister()
		mset.ackq.unregister()
		mset.outq.unregister()
	}

	// Clustered cleanup.
	mset.mu.Unlock()

	c.closeConnection(ClientClosed)

	if sysc != nil {
		sysc.closeConnection(ClientClosed)
	}

	if mset.store == nil {
		return nil
	}

	if deleteFlag {
		if err := mset.store.Delete(); err != nil {
			return err
		}
		js.releaseStreamResources(&mset.cfg)
	} else if err := mset.store.Stop(); err != nil {
		return err
	}

	return nil
}

func (mset *stream) getMsg(seq uint64) (*StoredMsg, error) {
	var smv StoreMsg
	sm, err := mset.store.LoadMsg(seq, &smv)
	if err != nil {
		return nil, err
	}
	// This only used in tests directly so no need to pool etc.
	return &StoredMsg{
		Subject:  sm.subj,
		Sequence: sm.seq,
		Header:   sm.hdr,
		Data:     sm.msg,
		Time:     time.Unix(0, sm.ts).UTC(),
	}, nil
}

// getConsumers will return all the current consumers for this stream.
func (mset *stream) getConsumers() []*consumer {
	mset.mu.RLock()
	defer mset.mu.RUnlock()

	var obs []*consumer
	for _, o := range mset.consumers {
		obs = append(obs, o)
	}
	return obs
}

// Lock should be held for this one.
func (mset *stream) numPublicConsumers() int {
	return len(mset.consumers) - mset.directs
}

// This returns all consumers that are not DIRECT.
func (mset *stream) getPublicConsumers() []*consumer {
	mset.mu.RLock()
	defer mset.mu.RUnlock()

	var obs []*consumer
	for _, o := range mset.consumers {
		if !o.cfg.Direct {
			obs = append(obs, o)
		}
	}
	return obs
}

// NumConsumers reports on number of active consumers for this stream.
func (mset *stream) numConsumers() int {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return len(mset.consumers)
}

func (mset *stream) setConsumer(o *consumer) {
	mset.consumers[o.name] = o
	if o.cfg.FilterSubject != _EMPTY_ {
		mset.numFilter++
	}
	if o.cfg.Direct {
		mset.directs++
	}
}

func (mset *stream) removeConsumer(o *consumer) {
	if o.cfg.FilterSubject != _EMPTY_ && mset.numFilter > 0 {
		mset.numFilter--
	}
	if o.cfg.Direct && mset.directs > 0 {
		mset.directs--
	}
	delete(mset.consumers, o.name)
}

// lookupConsumer will retrieve a consumer by name.
func (mset *stream) lookupConsumer(name string) *consumer {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	return mset.consumers[name]
}

func (mset *stream) numDirectConsumers() (num int) {
	mset.mu.RLock()
	defer mset.mu.RUnlock()

	// Consumers that are direct are not recorded at the store level.
	for _, o := range mset.consumers {
		o.mu.RLock()
		if o.cfg.Direct {
			num++
		}
		o.mu.RUnlock()
	}
	return num
}

// State will return the current state for this stream.
func (mset *stream) state() StreamState {
	return mset.stateWithDetail(false)
}

func (mset *stream) stateWithDetail(details bool) StreamState {
	mset.mu.RLock()
	c, store := mset.client, mset.store
	mset.mu.RUnlock()
	if c == nil || store == nil {
		return StreamState{}
	}
	// Currently rely on store.
	if details {
		return store.State()
	}
	// Here we do the fast version.
	var state StreamState
	mset.store.FastState(&state)
	return state
}

func (mset *stream) Store() StreamStore {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.store
}

// Determines if the new proposed partition is unique amongst all consumers.
// Lock should be held.
func (mset *stream) partitionUnique(partition string) bool {
	for _, o := range mset.consumers {
		if o.cfg.FilterSubject == _EMPTY_ {
			return false
		}
		if subjectIsSubsetMatch(partition, o.cfg.FilterSubject) {
			return false
		}
	}
	return true
}

// Lock should be held.
func (mset *stream) checkInterest(seq uint64, obs *consumer) bool {
	for _, o := range mset.consumers {
		if o != obs && o.needAck(seq) {
			return true
		}
	}
	return false
}

// ackMsg is called into from a consumer when we have a WorkQueue or Interest Retention Policy.
func (mset *stream) ackMsg(o *consumer, seq uint64) {
	var shouldRemove bool

	switch mset.cfg.Retention {
	case LimitsPolicy:
		return
	case WorkQueuePolicy:
		// Normally we just remove a message when its ack'd here but if we have direct consumers
		// from sources and/or mirrors we need to make sure they have delivered the msg.
		mset.mu.RLock()
		shouldRemove = mset.directs <= 0 || !mset.checkInterest(seq, o)
		mset.mu.RUnlock()
	case InterestPolicy:
		mset.mu.RLock()
		shouldRemove = !mset.checkInterest(seq, o)
		mset.mu.RUnlock()
	}

	if shouldRemove {
		if _, err := mset.store.RemoveMsg(seq); err == ErrStoreEOF {
			// This should be rare but I have seen it.
			// The ack reached us before the actual msg with AckNone and InterestPolicy.
			if n := mset.raftNode(); n != nil {
				md := streamMsgDelete{Seq: seq, NoErase: true, Stream: mset.cfg.Name}
				n.ForwardProposal(encodeMsgDelete(&md))
			}
		}
	}
}

// Snapshot creates a snapshot for the stream and possibly consumers.
func (mset *stream) snapshot(deadline time.Duration, checkMsgs, includeConsumers bool) (*SnapshotResult, error) {
	mset.mu.RLock()
	if mset.client == nil || mset.store == nil {
		mset.mu.RUnlock()
		return nil, errors.New("invalid stream")
	}
	store := mset.store
	mset.mu.RUnlock()

	return store.Snapshot(deadline, checkMsgs, includeConsumers)
}

const snapsDir = "__snapshots__"

// RestoreStream will restore a stream from a snapshot.
func (a *Account) RestoreStream(ncfg *StreamConfig, r io.Reader) (*stream, error) {
	if ncfg == nil {
		return nil, errors.New("nil config on stream restore")
	}

	cfg, err := checkStreamCfg(ncfg, &a.srv.getOpts().JetStreamLimits)
	if err != nil {
		return nil, NewJSStreamNotFoundError(Unless(err))
	}

	_, jsa, err := a.checkForJetStream()
	if err != nil {
		return nil, err
	}

	sd := filepath.Join(jsa.storeDir, snapsDir)
	if _, err := os.Stat(sd); os.IsNotExist(err) {
		if err := os.MkdirAll(sd, defaultDirPerms); err != nil {
			return nil, fmt.Errorf("could not create snapshots directory - %v", err)
		}
	}
	sdir, err := ioutil.TempDir(sd, "snap-")
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(sdir); os.IsNotExist(err) {
		if err := os.MkdirAll(sdir, defaultDirPerms); err != nil {
			return nil, fmt.Errorf("could not create snapshots directory - %v", err)
		}
	}
	defer os.RemoveAll(sdir)

	logAndReturnError := func() error {
		a.mu.RLock()
		err := fmt.Errorf("unexpected content (account=%s)", a.Name)
		if a.srv != nil {
			a.srv.Errorf("Stream restore failed due to %v", err)
		}
		a.mu.RUnlock()
		return err
	}
	sdirCheck := filepath.Clean(sdir) + string(os.PathSeparator)

	tr := tar.NewReader(s2.NewReader(r))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break // End of snapshot
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return nil, logAndReturnError()
		}
		fpath := filepath.Join(sdir, filepath.Clean(hdr.Name))
		if !strings.HasPrefix(fpath, sdirCheck) {
			return nil, logAndReturnError()
		}
		os.MkdirAll(filepath.Dir(fpath), defaultDirPerms)
		fd, err := os.OpenFile(fpath, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(fd, tr)
		fd.Close()
		if err != nil {
			return nil, err
		}
	}

	// Check metadata.
	// The cfg passed in will be the new identity for the stream.
	var fcfg FileStreamInfo
	b, err := ioutil.ReadFile(filepath.Join(sdir, JetStreamMetaFile))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &fcfg); err != nil {
		return nil, err
	}

	// Check to make sure names match.
	if fcfg.Name != cfg.Name {
		return nil, errors.New("stream names do not match")
	}

	// See if this stream already exists.
	if _, err := a.lookupStream(cfg.Name); err == nil {
		return nil, NewJSStreamNameExistError()
	}
	// Move into the correct place here.
	ndir := filepath.Join(jsa.storeDir, streamsDir, cfg.Name)
	// Remove old one if for some reason it is still here.
	if _, err := os.Stat(ndir); err == nil {
		os.RemoveAll(ndir)
	}
	// Make sure our destination streams directory exists.
	if err := os.MkdirAll(filepath.Join(jsa.storeDir, streamsDir), defaultDirPerms); err != nil {
		return nil, err
	}
	// Move into new location.
	if err := os.Rename(sdir, ndir); err != nil {
		return nil, err
	}
	if cfg.Template != _EMPTY_ {
		if err := jsa.addStreamNameToTemplate(cfg.Template, cfg.Name); err != nil {
			return nil, err
		}
	}
	mset, err := a.addStream(&cfg)
	if err != nil {
		return nil, err
	}
	if !fcfg.Created.IsZero() {
		mset.setCreatedTime(fcfg.Created)
	}
	lseq := mset.lastSeq()

	// Now do consumers.
	odir := filepath.Join(ndir, consumerDir)
	ofis, _ := ioutil.ReadDir(odir)
	for _, ofi := range ofis {
		metafile := filepath.Join(odir, ofi.Name(), JetStreamMetaFile)
		metasum := filepath.Join(odir, ofi.Name(), JetStreamMetaFileSum)
		if _, err := os.Stat(metafile); os.IsNotExist(err) {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
		buf, err := ioutil.ReadFile(metafile)
		if err != nil {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
		if _, err := os.Stat(metasum); os.IsNotExist(err) {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
		var cfg FileConsumerInfo
		if err := json.Unmarshal(buf, &cfg); err != nil {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
		isEphemeral := !isDurableConsumer(&cfg.ConsumerConfig)
		if isEphemeral {
			// This is an ephermal consumer and this could fail on restart until
			// the consumer can reconnect. We will create it as a durable and switch it.
			cfg.ConsumerConfig.Durable = ofi.Name()
		}
		obs, err := mset.addConsumer(&cfg.ConsumerConfig)
		if err != nil {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
		if isEphemeral {
			obs.switchToEphemeral()
		}
		if !cfg.Created.IsZero() {
			obs.setCreatedTime(cfg.Created)
		}
		obs.mu.Lock()
		err = obs.readStoredState(lseq)
		obs.mu.Unlock()
		if err != nil {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
	}
	return mset, nil
}
