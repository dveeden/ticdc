// Copyright 2024 PingCAP, Inc.
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

package coordinator

import (
	"context"
	"sync"
	"time"

	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/coordinator/changefeed"
	"github.com/pingcap/ticdc/coordinator/operator"
	"github.com/pingcap/ticdc/heartbeatpb"
	"github.com/pingcap/ticdc/pkg/bootstrap"
	"github.com/pingcap/ticdc/pkg/common"
	appcontext "github.com/pingcap/ticdc/pkg/common/context"
	"github.com/pingcap/ticdc/pkg/config"
	"github.com/pingcap/ticdc/pkg/errors"
	"github.com/pingcap/ticdc/pkg/messaging"
	"github.com/pingcap/ticdc/pkg/metrics"
	"github.com/pingcap/ticdc/pkg/node"
	"github.com/pingcap/ticdc/pkg/scheduler"
	"github.com/pingcap/ticdc/server/watcher"
	"github.com/pingcap/ticdc/utils/chann"
	"github.com/pingcap/ticdc/utils/threadpool"
	"github.com/pingcap/tiflow/cdc/model"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

// Controller schedules and balance changefeeds, there are 3 main components:
//  1. scheduler: generate operators for handling different scheduling tasks.
//  2. operatorController: manage all operators and execute them periodically.
//  3. changefeedDB: store all changefeeds info and their status in memory.
//  4. backend: the durable storage for storing changefeed metadata.
type Controller struct {
	version int64

	scheduler          *scheduler.Controller
	operatorController *operator.Controller
	changefeedDB       *changefeed.ChangefeedDB
	backend            changefeed.Backend
	eventCh            *chann.DrainableChann[*Event]

	bootstrapped *atomic.Bool
	bootstrapper *bootstrap.Bootstrapper[heartbeatpb.CoordinatorBootstrapResponse]

	mutex       sync.Mutex // protect nodeChanged and do onNodeChanged()
	nodeChanged bool
	nodeManager *watcher.NodeManager

	taskScheduler    threadpool.ThreadPool
	taskHandlerMutex sync.Mutex // protect taskHandlers
	taskHandlers     []*threadpool.TaskHandle
	messageCenter    messaging.MessageCenter

	updatedChangefeedCh chan map[common.ChangeFeedID]*changefeed.Changefeed
	stateChangedCh      chan *ChangefeedStateChangeEvent

	lastPrintStatusTime time.Time

	apiLock sync.RWMutex
}

type ChangefeedStateChangeEvent struct {
	ChangefeedID common.ChangeFeedID
	State        model.FeedState
	err          *model.RunningError
}

func NewController(
	version int64,
	selfNode *node.Info,
	updatedChangefeedCh chan map[common.ChangeFeedID]*changefeed.Changefeed,
	stateChangedCh chan *ChangefeedStateChangeEvent,
	backend changefeed.Backend,
	eventCh *chann.DrainableChann[*Event],
	taskScheduler threadpool.ThreadPool,
	batchSize int, balanceInterval time.Duration,
) *Controller {
	mc := appcontext.GetService[messaging.MessageCenter](appcontext.MessageCenter)
	changefeedDB := changefeed.NewChangefeedDB(version)

	nodeManager := appcontext.GetService[*watcher.NodeManager](watcher.NodeManagerName)
	oc := operator.NewOperatorController(mc, selfNode, changefeedDB, backend, nodeManager, batchSize)
	c := &Controller{
		version:      version,
		bootstrapped: atomic.NewBool(false),
		scheduler: scheduler.NewController(map[string]scheduler.Scheduler{
			scheduler.BasicScheduler:   scheduler.NewBasicScheduler(selfNode.ID.String(), batchSize, oc, changefeedDB, nodeManager, oc.NewAddMaintainerOperator),
			scheduler.BalanceScheduler: scheduler.NewBalanceScheduler(selfNode.ID.String(), batchSize, oc, changefeedDB, nodeManager, balanceInterval, oc.NewMoveMaintainerOperator),
		}),
		eventCh:             eventCh,
		operatorController:  oc,
		messageCenter:       mc,
		changefeedDB:        changefeedDB,
		nodeManager:         nodeManager,
		taskScheduler:       taskScheduler,
		backend:             backend,
		nodeChanged:         false,
		updatedChangefeedCh: updatedChangefeedCh,
		stateChangedCh:      stateChangedCh,
		lastPrintStatusTime: time.Now(),
	}
	c.bootstrapper = bootstrap.NewBootstrapper[heartbeatpb.CoordinatorBootstrapResponse]("coordinator", c.newBootstrapMessage)
	// init bootstrapper nodes
	nodes := c.nodeManager.GetAliveNodes()
	// detect the capture changes
	c.nodeManager.RegisterNodeChangeHandler("coordinator-controller", func(allNodes map[node.ID]*node.Info) {
		c.mutex.Lock()
		c.nodeChanged = true
		c.mutex.Unlock()
	})

	log.Info("coordinator bootstrap initial nodes",
		zap.Int("nodes", len(nodes)))
	newNodes := make([]*node.Info, 0, len(nodes))
	for _, n := range nodes {
		newNodes = append(newNodes, n)
	}
	for _, msg := range c.bootstrapper.HandleNewNodes(newNodes) {
		_ = c.messageCenter.SendCommand(msg)
	}
	c.submitPeriodTask()
	return c
}

// HandleEvent implements the event-driven process mode
func (c *Controller) HandleEvent(event *Event) bool {
	if event == nil {
		return false
	}

	start := time.Now()
	defer func() {
		duration := time.Since(start)
		if duration > time.Second {
			log.Info("coordinator is too slow",
				zap.Int("type", event.eventType),
				zap.Duration("duration", duration))
		}
	}()
	// first check the online/offline nodes
	c.checkOnNodeChanged()

	switch event.eventType {
	case EventMessage:
		c.onMessage(event.message)
	case EventPeriod:
		c.onPeriodTask()
	}
	return false
}

func (c *Controller) checkOnNodeChanged() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.nodeChanged {
		c.onNodeChanged()
		c.nodeChanged = false
	}
}

func (c *Controller) onPeriodTask() {
	// resend bootstrap message
	c.sendMessages(c.bootstrapper.ResendBootstrapMessage())
	c.collectMetrics()
}

func (c *Controller) onMessage(msg *messaging.TargetMessage) {
	switch msg.Type {
	case messaging.TypeCoordinatorBootstrapResponse:
		c.onMaintainerBootstrapResponse(msg)
	case messaging.TypeMaintainerHeartbeatRequest:
		if c.bootstrapper.CheckAllNodeInitialized() {
			req := msg.Message[0].(*heartbeatpb.MaintainerHeartbeat)
			c.HandleStatus(msg.From, req.Statuses)
		}
	default:
		log.Panic("unexpected message type",
			zap.String("type", msg.Type.String()))
	}
}

func (c *Controller) onNodeChanged() {
	currentNodes := c.bootstrapper.GetAllNodes()

	activeNodes := c.nodeManager.GetAliveNodes()
	newNodes := make([]*node.Info, 0, len(activeNodes))
	for id, n := range activeNodes {
		if _, ok := currentNodes[id]; !ok {
			newNodes = append(newNodes, n)
		}
	}
	var removedNodes []node.ID
	for id := range currentNodes {
		if _, ok := activeNodes[id]; !ok {
			removedNodes = append(removedNodes, id)
			c.RemoveNode(id)
		}
	}
	log.Info("node changed",
		zap.Int("new", len(newNodes)),
		zap.Int("removed", len(removedNodes)))
	c.sendMessages(c.bootstrapper.HandleNewNodes(newNodes))
	cachedResponse := c.bootstrapper.HandleRemoveNodes(removedNodes)
	if cachedResponse != nil {
		log.Info("bootstrap done after removed some nodes")
		c.onBootstrapDone(cachedResponse)
	}
}

func (c *Controller) sendMessages(msgs []*messaging.TargetMessage) {
	for _, msg := range msgs {
		_ = c.messageCenter.SendCommand(msg)
	}
}

func (c *Controller) onMaintainerBootstrapResponse(msg *messaging.TargetMessage) {
	log.Info("received maintainer bootstrap response",
		zap.Any("server", msg.From))
	cachedResp := c.bootstrapper.HandleBootstrapResponse(
		msg.From,
		msg.Message[0].(*heartbeatpb.CoordinatorBootstrapResponse),
	)
	c.onBootstrapDone(cachedResp)
}

type remoteMaintainer struct {
	nodeID node.ID
	status *heartbeatpb.MaintainerStatus
}

func (c *Controller) onBootstrapDone(cachedResp map[node.ID]*heartbeatpb.CoordinatorBootstrapResponse) {
	if cachedResp == nil {
		return
	}
	log.Info("all nodes have sent bootstrap response",
		zap.Int("size", len(cachedResp)))
	// runningCfs is the changefeeds that are already running on other nodes
	runningCfs := make(map[common.ChangeFeedID]remoteMaintainer)
	for server, bootstrapMsg := range cachedResp {
		log.Info("received bootstrap response",
			zap.Any("server", server),
			zap.Int("size", len(bootstrapMsg.Statuses)))
		for _, info := range bootstrapMsg.Statuses {
			cfID := common.NewChangefeedIDFromPB(info.ChangefeedID)
			if _, ok := runningCfs[cfID]; ok {
				log.Panic("maintainer runs on multiple node",
					zap.String("cf", cfID.Name()))
			}
			runningCfs[cfID] = remoteMaintainer{
				nodeID: server,
				status: info,
			}
		}
	}
	c.FinishBootstrap(runningCfs)
}

// HandleStatus handle the status report from the node
func (c *Controller) HandleStatus(from node.ID, statusList []*heartbeatpb.MaintainerStatus) {
	cfs := make(map[common.ChangeFeedID]*changefeed.Changefeed, len(statusList))
	for _, status := range statusList {
		cfID := common.NewChangefeedIDFromPB(status.ChangefeedID)
		c.operatorController.UpdateOperatorStatus(cfID, from, status)
		cf := c.GetTask(cfID)
		if cf == nil {
			if status.State != heartbeatpb.ComponentState_Working {
				continue
			}
			if op := c.operatorController.GetOperator(cfID); op == nil {
				log.Warn("no changgefeed found and no operator for it, ignore",
					zap.String("changefeed", cfID.Name()),
					zap.String("from", from.String()),
					zap.Any("status", status))
				// if the changefeed is not found, and the status is working, we need to remove it from maintainer
				_ = c.messageCenter.SendCommand(changefeed.RemoveMaintainerMessage(cfID, from, true, true))
			}
			continue
		}
		nodeID := cf.GetNodeID()
		if nodeID == "" {
			// the changefeed is stopped
			continue
		}
		if nodeID != from {
			// todo: handle the case that the node id is mismatch
			log.Warn("remote changefeed maintainer nodeID mismatch with local record",
				zap.String("changefeed", cfID.Name()),
				zap.Stringer("remoteNodeID", from),
				zap.Stringer("localNodeID", nodeID))
			continue
		}
		cfs[cfID] = cf

		changed, state, err := cf.UpdateStatus(status)
		if changed {
			log.Info("changefeed status changed",
				zap.String("changefeed", cfID.Name()),
				zap.Any("state", state),
				zap.Any("error", err))
			var mErr *model.RunningError
			if err != nil {
				mErr = &model.RunningError{
					Time:    time.Now(),
					Addr:    err.Node,
					Code:    err.Code,
					Message: err.Message,
				}
			}
			c.stateChangedCh <- &ChangefeedStateChangeEvent{
				ChangefeedID: cfID,
				State:        state,
				err:          mErr,
			}
		}
	}
	select {
	case c.updatedChangefeedCh <- cfs:
	default:
	}
}

// FinishBootstrap is called when all nodes have sent bootstrap response
// It will load all changefeeds from metastore, and compare with running changefeeds
// Then initialize the changefeeds that are not running on other nodes
// And construct all changefeeds state in memory.
func (c *Controller) FinishBootstrap(runningChangefeeds map[common.ChangeFeedID]remoteMaintainer) {
	if c.bootstrapped.Load() {
		log.Panic("already bootstrapped",
			zap.Any("runningChangefeeds", runningChangefeeds))
	}
	// load all changefeeds from metastore, and check if the changefeed is already in workingMap
	allChangefeeds, err := c.backend.GetAllChangefeeds(context.Background())
	if err != nil {
		log.Panic("load all changefeeds failed", zap.Error(err))
	}
	log.Info("load all changefeeds", zap.Int("size", len(allChangefeeds)))
	// Compare all changefeeds and running changefeeds, and add them to changefeedDB
	for cfID, cfMeta := range allChangefeeds {
		rm, ok := runningChangefeeds[cfID]
		if !ok {
			// The changefeed is not running on other nodes, add it to changefeedDB.
			// We will create this changefeed later.
			cf := changefeed.NewChangefeed(cfID, cfMeta.Info, cfMeta.Status.CheckpointTs, false)
			if shouldRunChangefeed(cf.GetInfo().State) {
				c.changefeedDB.AddAbsentChangefeed(cf)
			} else {
				c.changefeedDB.AddStoppedChangefeed(cf)
			}
		} else {
			log.Info("changefeed maintainer already running in other server",
				zap.String("changefeed", cfID.String()),
				zap.String("node", rm.nodeID.String()),
				zap.String("status", common.FormatMaintainerStatus(rm.status)))
			cf := changefeed.NewChangefeed(cfID, cfMeta.Info, rm.status.CheckpointTs, false)
			c.changefeedDB.AddReplicatingMaintainer(cf, rm.nodeID)
			delete(runningChangefeeds, cfID)
		}

		// check if the changefeed is stopping or removing, we need to stop all dispatchers completely
		switch cfMeta.Status.Progress {
		case config.ProgressStopping, config.ProgressRemoving:
			remove := cfMeta.Status.Progress == config.ProgressRemoving
			c.operatorController.StopChangefeed(context.Background(), cfID, remove)
			log.Info("stop changefeed when bootstrapping", zap.String("changefeed", cfID.String()), zap.Any("meta", cfMeta))
		}
	}

	// Remove the changefeeds that are not in allChangefeeds, there are stale changefeeds.
	for id, rm := range runningChangefeeds {
		log.Warn("maintainer not found in local, remove it",
			zap.String("changefeed", id.Name()),
			zap.String("node", rm.nodeID.String()),
		)
		_ = c.messageCenter.SendCommand(changefeed.RemoveMaintainerMessage(id, rm.nodeID, true, true))
	}

	// start operator and scheduler
	c.taskHandlerMutex.Lock()
	defer c.taskHandlerMutex.Unlock()
	c.taskHandlers = append(c.taskHandlers, c.scheduler.Start(c.taskScheduler)...)
	operatorControllerHandle := c.taskScheduler.Submit(c.operatorController, time.Now())
	c.taskHandlers = append(c.taskHandlers, operatorControllerHandle)
	c.bootstrapped.Store(true)
}

func (c *Controller) Stop() {
	c.taskHandlerMutex.Lock()
	defer c.taskHandlerMutex.Unlock()
	for _, h := range c.taskHandlers {
		h.Cancel()
	}
}

func (c *Controller) CreateChangefeed(ctx context.Context, info *config.ChangeFeedInfo) error {
	c.apiLock.Lock()
	defer c.apiLock.Unlock()

	if !c.bootstrapped.Load() {
		return errors.New("not initialized, wait a moment")
	}
	old := c.changefeedDB.GetByChangefeedDisplayName(info.ChangefeedID.DisplayName)
	if old != nil {
		return errors.New("changefeed already exists")
	}
	if ok := c.operatorController.HasOperator(info.ChangefeedID.DisplayName); ok {
		return errors.New("changefeed is in scheduling")
	}
	err := c.backend.CreateChangefeed(ctx, info)
	if err != nil {
		return errors.Trace(err)
	}
	c.changefeedDB.AddAbsentChangefeed(changefeed.NewChangefeed(info.ChangefeedID, info, info.StartTs, true))
	return nil
}

func (c *Controller) RemoveChangefeed(ctx context.Context, id common.ChangeFeedID) (uint64, error) {
	c.apiLock.Lock()
	defer c.apiLock.Unlock()

	cf := c.changefeedDB.GetByID(id)
	if cf == nil {
		return 0, errors.New("changefeed not found")
	}
	err := c.backend.SetChangefeedProgress(ctx, id, config.ProgressRemoving)
	if err != nil {
		return 0, errors.Trace(err)
	}
	c.operatorController.StopChangefeed(ctx, id, true)
	return cf.GetStatus().CheckpointTs, nil
}

func (c *Controller) PauseChangefeed(ctx context.Context, id common.ChangeFeedID) error {
	c.apiLock.Lock()
	defer c.apiLock.Unlock()

	cf := c.changefeedDB.GetByID(id)
	if cf == nil {
		return errors.New("changefeed not found")
	}
	if err := c.backend.PauseChangefeed(ctx, id); err != nil {
		return errors.Trace(err)
	}
	if clone, err := cf.GetInfo().Clone(); err != nil {
		return errors.Trace(err)
	} else {
		clone.State = model.StateStopped
		cf.SetInfo(clone)
	}
	c.operatorController.StopChangefeed(ctx, id, false)
	return nil
}

func (c *Controller) ResumeChangefeed(ctx context.Context, id common.ChangeFeedID, newCheckpointTs uint64, overwriteCheckpointTs bool) error {
	c.apiLock.Lock()
	defer c.apiLock.Unlock()

	cf := c.changefeedDB.GetByID(id)
	if cf == nil {
		return errors.New("changefeed not found")
	}
	if err := c.backend.ResumeChangefeed(ctx, id, newCheckpointTs); err != nil {
		return errors.Trace(err)
	}
	if clone, err := cf.GetInfo().Clone(); err != nil {
		return errors.Trace(err)
	} else {
		clone.State = model.StateNormal
		cf.SetInfo(clone)
	}

	status := cf.GetClonedStatus()
	status.CheckpointTs = newCheckpointTs
	_, _, err := cf.ForceUpdateStatus(status)
	if err != nil {
		return errors.New(err.Message)
	}

	c.changefeedDB.Resume(id, true, overwriteCheckpointTs)
	return nil
}

func (c *Controller) UpdateChangefeed(ctx context.Context, change *config.ChangeFeedInfo) error {
	c.apiLock.Lock()
	defer c.apiLock.Unlock()

	cf := c.changefeedDB.GetByID(change.ChangefeedID)
	if cf == nil {
		return errors.New("changefeed not found")
	}
	if err := c.backend.UpdateChangefeed(ctx, change, cf.GetStatus().CheckpointTs, config.ProgressStopping); err != nil {
		return errors.Trace(err)
	}
	c.changefeedDB.ReplaceStoppedChangefeed(change)
	return nil
}

func (c *Controller) ListChangefeeds(_ context.Context) ([]*config.ChangeFeedInfo, []*config.ChangeFeedStatus, error) {
	c.apiLock.RLock()
	defer c.apiLock.RUnlock()

	cfs := c.changefeedDB.GetAllChangefeeds()
	infos := make([]*config.ChangeFeedInfo, 0, len(cfs))
	statuses := make([]*config.ChangeFeedStatus, 0, len(cfs))
	for _, cf := range cfs {
		infos = append(infos, cf.GetInfo())
		statuses = append(statuses, &config.ChangeFeedStatus{CheckpointTs: cf.GetStatus().CheckpointTs})
	}
	return infos, statuses, nil
}

func (c *Controller) GetChangefeed(
	_ context.Context,
	changefeedDisplayName common.ChangeFeedDisplayName,
) (
	*config.ChangeFeedInfo,
	*config.ChangeFeedStatus,
	error,
) {
	c.apiLock.RLock()
	defer c.apiLock.RUnlock()

	cf := c.changefeedDB.GetByChangefeedDisplayName(changefeedDisplayName)
	if cf == nil {
		return nil, nil, errors.ErrChangeFeedNotExists.GenWithStackByArgs(changefeedDisplayName.Name)
	}

	maintainerID := cf.GetNodeID()
	nodeInfo := c.nodeManager.GetNodeInfo(maintainerID)
	maintainerAddr := ""
	if nodeInfo != nil {
		maintainerAddr = nodeInfo.AdvertiseAddr
	}
	status := &config.ChangeFeedStatus{CheckpointTs: cf.GetStatus().CheckpointTs}
	status.SetMaintainerAddr(maintainerAddr)
	return cf.GetInfo(), status, nil
}

// GetTask queries a task by channgefeed ID, return nil if not found
func (c *Controller) GetTask(id common.ChangeFeedID) *changefeed.Changefeed {
	return c.changefeedDB.GetByID(id)
}

// RemoveNode is called when a node is removed
func (c *Controller) RemoveNode(id node.ID) {
	c.operatorController.OnNodeRemoved(id)
}

func (c *Controller) submitPeriodTask() {
	task := func() time.Time {
		c.eventCh.In() <- &Event{eventType: EventPeriod}
		return time.Now().Add(time.Millisecond * 500)
	}
	periodTaskhandler := c.taskScheduler.SubmitFunc(task, time.Now().Add(time.Millisecond*500))
	c.taskHandlers = append(c.taskHandlers, periodTaskhandler)
}

func (c *Controller) newBootstrapMessage(id node.ID) *messaging.TargetMessage {
	log.Info("send coordinator bootstrap request", zap.Any("to", id))
	return messaging.NewSingleTargetMessage(
		id,
		messaging.MaintainerManagerTopic,
		&heartbeatpb.CoordinatorBootstrapRequest{Version: c.version})
}

func (c *Controller) collectMetrics() {
	if time.Since(c.lastPrintStatusTime) > time.Second*20 {
		metrics.ChangefeedStateGauge.WithLabelValues("Total").Set(float64(c.changefeedDB.GetSize()))
		metrics.ChangefeedStateGauge.WithLabelValues("Working").Set(float64(c.changefeedDB.GetReplicatingSize()))
		metrics.ChangefeedStateGauge.WithLabelValues("Scheduling").Set(float64(c.operatorController.OperatorSize()))
		metrics.ChangefeedStateGauge.WithLabelValues("Absent").Set(float64(c.changefeedDB.GetAbsentSize()))
		metrics.ChangefeedStateGauge.WithLabelValues("Stopped").Set(float64(c.changefeedDB.GetStoppedSize()))
		c.lastPrintStatusTime = time.Now()
	}
}
