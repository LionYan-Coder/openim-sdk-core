// Copyright © 2023 OpenIM SDK. All rights reserved.
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

package interaction

import (
	"context"
	"open_im_sdk/pkg/common"
	"open_im_sdk/pkg/constant"
	"open_im_sdk/pkg/db/db_interface"
	"open_im_sdk/sdk_struct"

	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/log"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/proto/sdkws"
	"github.com/golang/protobuf/proto"
)

const (
	timeout         = 60
	retryTimes      = 2
	defaultPullNums = 1
)

type Seq struct {
	maxSeq      int64
	minSeq      int64
	sessionType int32
}

type SyncedSeq struct {
	maxSeqSynced int64
	sessionType  int32
}

// The callback synchronization starts. The reconnection ends
type MsgSyncer struct {
	loginUserID        string                // login user ID
	longConnMgr        *LongConnMgr          // long connection manager
	PushMsgAndMaxSeqCh chan common.Cmd2Value // channel for receiving push messages and the maximum SEQ number
	conversationCh     chan common.Cmd2Value // channel for triggering new message arriving in a conversation
	syncedMaxSeqs      map[string]SyncedSeq  // map of the maximum synced SEQ numbers for all group IDs
	db                 db_interface.DataBase // data store
	syncTimes          int                   // times of sync
	ctx                context.Context       // context
	cancel             context.CancelFunc    // cancel function
}

// NewMsgSyncer creates a new instance of the message synchronizer.
func NewMsgSyncer(ctx context.Context, conversationCh, PushMsgAndMaxSeqCh, recvSeqch chan common.Cmd2Value,
	loginUserID string, longConnMgr *LongConnMgr, db db_interface.DataBase, syncTimes int) (*MsgSyncer, error) {
	m := &MsgSyncer{
		loginUserID:        loginUserID,
		longConnMgr:        longConnMgr,
		PushMsgAndMaxSeqCh: PushMsgAndMaxSeqCh,
		conversationCh:     conversationCh,
		ctx:                ctx,
		syncedMaxSeqs:      make(map[string]SyncedSeq),
		db:                 db,
		syncTimes:          syncTimes,
	}
	err := m.loadSeq(ctx)
	go m.DoListener()
	return m, err
}

// seq The db reads the data to the memory,set syncedMaxSeqs
func (m *MsgSyncer) loadSeq(ctx context.Context) error {
	m.syncedMaxSeqs = make(map[string]SyncedSeq)
	groupIDs, err := m.db.GetReadDiffusionGroupIDList(ctx)
	if err != nil {
		log.ZError(ctx, "get group id list failed", err)
		return err
	}
	for _, groupID := range groupIDs {
		nMaxSeq, err := m.db.GetSuperGroupNormalMsgSeq(ctx, groupID)
		if err != nil {
			log.ZError(ctx, "get group normal seq failed", err, "groupID", groupID)
			return err
		}
		aMaxSeq, err := m.db.GetSuperGroupAbnormalMsgSeq(ctx, groupID)
		if err != nil {
			log.ZError(ctx, "get group abnormal seq failed", err, "groupID", groupID)
			return err
		}
		var maxSeqSynced int64 = nMaxSeq
		if aMaxSeq > nMaxSeq {
			maxSeqSynced = aMaxSeq
		}

		m.syncedMaxSeqs[groupID] = SyncedSeq{
			maxSeqSynced: maxSeqSynced,
			sessionType:  constant.SuperGroupChatType,
		}
	}
	return nil
}

// DoListener Listen to the message pipe of the message synchronizer
// and process received and pushed messages
func (m *MsgSyncer) DoListener() {
	for {
		select {
		case cmd := <-m.PushMsgAndMaxSeqCh:
			m.handlePushMsgAndEvent(cmd)
		case <-m.ctx.Done():
			log.ZInfo(m.ctx, "msg syncer done, sdk logout.....")
			return
		}
	}
}

// init, reconnect, sync by heartbeat
// func (m *MsgSyncer) compareSeqsAndTrigger(ctx context.Context, newSeqMap map[string]Seq, cmd string) {
func (m *MsgSyncer) compareSeqsAndTrigger(cmd common.Cmd2Value) {
	ctx := cmd.Ctx
	newSeqMap := cmd.Value.(map[string]Seq)

	// sync callback to conversation
	switch cmd.Cmd {
	case constant.CmdInit:
		m.triggerSync()
		defer m.triggerSyncFinished()
	case constant.CmdReconnect:
		m.triggerReconnect()
		defer m.triggerReconnectFinished()
	}
	for sourceID, newSeq := range newSeqMap {
		if syncedSeq, ok := m.syncedMaxSeqs[sourceID]; ok {
			if newSeq.maxSeq > syncedSeq.maxSeqSynced {
				_ = m.sync(ctx, sourceID, newSeq.sessionType, syncedSeq.maxSeqSynced, newSeq.maxSeq)
			}
		} else {
			// new conversation
			_ = m.sync(ctx, sourceID, newSeq.sessionType, 0, newSeq.maxSeq)
		}
	}
	m.syncTimes++
}

func (m *MsgSyncer) sync(ctx context.Context, sourceID string, sessionType int32, syncedMaxSeq, maxSeq int64) (err error) {
	if err = m.syncAndTriggerMsgs(ctx, sourceID, sessionType, syncedMaxSeq, maxSeq); err != nil {
		log.ZError(ctx, "sync msgs failed", err, "sourceID", sourceID)
		return err
	}
	m.syncedMaxSeqs[sourceID] = SyncedSeq{
		maxSeqSynced: maxSeq,
		sessionType:  sessionType,
	}
	return nil
}

// get seqs need sync interval
func (m *MsgSyncer) getSeqsNeedSync(syncedMaxSeq, maxSeq int64) []int64 {
	var seqs []int64
	for i := syncedMaxSeq + 1; i <= maxSeq; i++ {
		seqs = append(seqs, i)
	}
	return seqs
}

// recv msg from
func (m *MsgSyncer) handlePushMsgAndEvent(cmd common.Cmd2Value) {
	ctx := cmd.Ctx
	msg := cmd.Value.(*sdkws.MsgData)
	// parsing cmd
	// if cmd.Cmd == constant.CmdMaxSeq {
	// 	m.compareSeqsAndTrigger(cmd)
	// }
	switch cmd.Cmd {
	case constant.CmdConnSuccesss:
		m.doConnected()
	case constant.CmdMaxSeq:
		m.doMaxSeq(cmd.Value.(*sdk_struct.CmdMaxSeqToMsgSync))
	case constant.CmdPushMsg:
		m.doPushMsg(cmd.Value.(*sdkws.PushMessages))
	}

	// online msg
	if msg.Seq == 0 {
		_ = m.triggerConversation(ctx, []*sdkws.MsgData{msg})
		return
	}
	// seq is triggered directly and refreshed continuously
	if msg.Seq == m.syncedMaxSeqs[msg.GroupID].maxSeqSynced+1 {
		_ = m.triggerConversation(ctx, []*sdkws.MsgData{msg})
		oldSeq := m.syncedMaxSeqs[msg.GroupID]
		oldSeq.maxSeqSynced = msg.Seq
		m.syncedMaxSeqs[msg.GroupID] = oldSeq
	} else {
		m.sync(ctx, msg.GroupID, msg.SessionType, m.syncedMaxSeqs[msg.GroupID].maxSeqSynced, msg.Seq)
	}
}

// Fragment synchronization message, seq refresh after successful trigger
func (m *MsgSyncer) syncAndTriggerMsgs(ctx context.Context, sourceID string, sessionType int32, syncedMaxSeq, maxSeq int64) error {
	msgs, err := m.syncMsgBySeqsInterval(ctx, sourceID, sessionType, syncedMaxSeq, maxSeq)
	if err != nil {
		log.ZError(ctx, "syncMsgFromSvr err", err, "sourceID", sourceID, "sessionType", sessionType, "syncedMaxSeq", syncedMaxSeq, "maxSeq", maxSeq)
		return err
	}
	_ = m.triggerConversation(ctx, msgs)
	return err
}

func (m *MsgSyncer) splitSeqs(split int, seqsNeedSync []int64) (splitSeqs [][]int64) {
	if len(seqsNeedSync) <= split {
		splitSeqs = append(splitSeqs, seqsNeedSync)
		return
	}
	for i := 0; i < len(seqsNeedSync); i += split {
		end := i + split
		if end > len(seqsNeedSync) {
			end = len(seqsNeedSync)
		}
		splitSeqs = append(splitSeqs, seqsNeedSync[i:end])
	}
	return
}

// cached的不拉取
func (m *MsgSyncer) syncMsgBySeqsInterval(ctx context.Context, sourceID string, sesstionType int32, syncedMaxSeq, syncedMinSeq int64) (partMsgs []*sdkws.MsgData, err error) {
	return partMsgs, nil
}

// synchronizes messages by SEQs.
func (m *MsgSyncer) syncMsgBySeqs(ctx context.Context, sourceID string, sessionType int32, seqsNeedSync []int64) (allMsgs []*sdkws.MsgData, err error) {
	pullMsgReq := sdkws.PullMessageBySeqsReq{}
	pullMsgReq.UserID = m.loginUserID

	split := constant.SplitPullMsgNum
	seqsList := m.splitSeqs(split, seqsNeedSync)
	for i := 0; i < len(seqsList); {
		resp, err := m.longConnMgr.SendReqWaitResp(ctx, &pullMsgReq, constant.WSPullMsgBySeqList, retryTimes, m.loginUserID)
		if err != nil {
			log.ZError(ctx, "syncMsgFromSvrSplit err", err, "pullMsgReq", pullMsgReq)
			continue
		}
		i++
		var pullMsgResp sdkws.PullMessageBySeqsResp
		err = proto.Unmarshal(resp.Data, &pullMsgResp)
		if err != nil {
			log.ZError(ctx, "Unmarshal failed", err, "resp", resp)
			continue
		}
		allMsgs = append(allMsgs, pullMsgResp.List...)
	}
	return allMsgs, nil
}

// triggers a conversation with a new message.
func (m *MsgSyncer) triggerConversation(ctx context.Context, msgs []*sdkws.MsgData) error {
	err := common.TriggerCmdNewMsgCome(sdk_struct.CmdNewMsgComeToConversation{Ctx: ctx, MsgList: msgs}, m.conversationCh)
	if err != nil {
		log.ZError(ctx, "triggerCmdNewMsgCome err", err, "msgs", msgs)
	}
	return err
}

// triggers a reconnection.
func (m *MsgSyncer) triggerReconnect() {
	for groupID, syncedSeq := range m.syncedMaxSeqs {
		if syncedSeq.maxSeqSynced == 0 {
			continue
		}
		err := m.sync(m.ctx, groupID, syncedSeq.sessionType, 0, syncedSeq.maxSeqSynced)
		if err != nil {
			log.ZError(m.ctx, "sync failed", err, "groupID", groupID)
		}
	}
}

// finishes a reconnection.
func (m *MsgSyncer) triggerReconnectFinished() {
	for groupID, syncedSeq := range m.syncedMaxSeqs {
		if syncedSeq.maxSeqSynced == 0 {
			continue
		}
		err := m.sync(m.ctx, groupID, syncedSeq.sessionType, 0, syncedSeq.maxSeqSynced)
		if err != nil {
			log.ZError(m.ctx, "sync failed", err, "groupID", groupID)
		}
	}
}

// triggers a synchronization.
func (m *MsgSyncer) triggerSync() {
	for groupID, syncedSeq := range m.syncedMaxSeqs {
		if syncedSeq.maxSeqSynced == 0 {
			continue
		}
		err := m.sync(m.ctx, groupID, syncedSeq.sessionType, syncedSeq.maxSeqSynced, syncedSeq.maxSeqSynced+defaultPullNums)
		if err != nil {
			log.ZError(m.ctx, "sync failed", err, "groupID", groupID)
		}
	}
}

// finishes a synchronization.
func (m *MsgSyncer) triggerSyncFinished() {
	log.Info("Synchronization complete")
}
