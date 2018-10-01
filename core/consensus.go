// Copyright 2018 The dexon-consensus-core Authors
// This file is part of the dexon-consensus-core library.
//
// The dexon-consensus-core library is free software: you can redistribute it
// and/or modify it under the terms of the GNU Lesser General Public License as
// published by the Free Software Foundation, either version 3 of the License,
// or (at your option) any later version.
//
// The dexon-consensus-core library is distributed in the hope that it will be
// useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser
// General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the dexon-consensus-core library. If not, see
// <http://www.gnu.org/licenses/>.

package core

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/dexon-foundation/dexon-consensus-core/common"
	"github.com/dexon-foundation/dexon-consensus-core/core/blockdb"
	"github.com/dexon-foundation/dexon-consensus-core/core/crypto"
	"github.com/dexon-foundation/dexon-consensus-core/core/types"
)

// ErrMissingBlockInfo would be reported if some information is missing when
// calling PrepareBlock. It implements error interface.
type ErrMissingBlockInfo struct {
	MissingField string
}

func (e *ErrMissingBlockInfo) Error() string {
	return "missing " + e.MissingField + " in block"
}

// Errors for consensus core.
var (
	ErrProposerNotInNodeSet = fmt.Errorf(
		"proposer is not in node set")
	ErrIncorrectHash = fmt.Errorf(
		"hash of block is incorrect")
	ErrIncorrectSignature = fmt.Errorf(
		"signature of block is incorrect")
	ErrGenesisBlockNotEmpty = fmt.Errorf(
		"genesis block should be empty")
	ErrUnknownBlockProposed = fmt.Errorf(
		"unknown block is proposed")
	ErrUnknownBlockConfirmed = fmt.Errorf(
		"unknown block is confirmed")
	ErrIncorrectBlockPosition = fmt.Errorf(
		"position of block is incorrect")
	ErrIncorrectBlockTime = fmt.Errorf(
		"block timestampe is incorrect")
)

// consensusBAReceiver implements agreementReceiver.
type consensusBAReceiver struct {
	// TODO(mission): consensus would be replaced by shard and network.
	consensus       *Consensus
	agreementModule *agreement
	chainID         uint32
	restartNotary   chan bool
}

func (recv *consensusBAReceiver) ProposeVote(vote *types.Vote) {
	if err := recv.agreementModule.prepareVote(vote); err != nil {
		log.Println(err)
		return
	}
	go func() {
		if err := recv.agreementModule.processVote(vote); err != nil {
			log.Println(err)
			return
		}
		recv.consensus.network.BroadcastVote(vote)
	}()
}

func (recv *consensusBAReceiver) ProposeBlock() {
	block := recv.consensus.proposeBlock(recv.chainID)
	recv.consensus.baModules[recv.chainID].addCandidateBlock(block)
	if err := recv.consensus.preProcessBlock(block); err != nil {
		log.Println(err)
		return
	}
	recv.consensus.network.BroadcastBlock(block)
}

func (recv *consensusBAReceiver) ConfirmBlock(hash common.Hash) {
	block, exist := recv.consensus.baModules[recv.chainID].findCandidateBlock(hash)
	if !exist {
		log.Println(ErrUnknownBlockConfirmed, hash)
		return
	}
	if err := recv.consensus.processBlock(block); err != nil {
		log.Println(err)
		return
	}
	recv.restartNotary <- false
}

// consensusDKGReceiver implements dkgReceiver.
type consensusDKGReceiver struct {
	ID           types.NodeID
	gov          Governance
	authModule   *Authenticator
	nodeSetCache *NodeSetCache
	network      Network
}

// ProposeDKGComplaint proposes a DKGComplaint.
func (recv *consensusDKGReceiver) ProposeDKGComplaint(
	complaint *types.DKGComplaint) {
	if err := recv.authModule.SignDKGComplaint(complaint); err != nil {
		log.Println(err)
		return
	}
	recv.gov.AddDKGComplaint(complaint)
}

// ProposeDKGMasterPublicKey propose a DKGMasterPublicKey.
func (recv *consensusDKGReceiver) ProposeDKGMasterPublicKey(
	mpk *types.DKGMasterPublicKey) {
	if err := recv.authModule.SignDKGMasterPublicKey(mpk); err != nil {
		log.Println(err)
		return
	}
	recv.gov.AddDKGMasterPublicKey(mpk)
}

// ProposeDKGPrivateShare propose a DKGPrivateShare.
func (recv *consensusDKGReceiver) ProposeDKGPrivateShare(
	prv *types.DKGPrivateShare) {
	if err := recv.authModule.SignDKGPrivateShare(prv); err != nil {
		log.Println(err)
		return
	}
	receiverPubKey, exists := recv.nodeSetCache.GetPublicKey(prv.ReceiverID)
	if !exists {
		log.Println("public key for receiver not found")
		return
	}
	recv.network.SendDKGPrivateShare(receiverPubKey, prv)
}

// ProposeDKGAntiNackComplaint propose a DKGPrivateShare as an anti complaint.
func (recv *consensusDKGReceiver) ProposeDKGAntiNackComplaint(
	prv *types.DKGPrivateShare) {
	if prv.ProposerID == recv.ID {
		if err := recv.authModule.SignDKGPrivateShare(prv); err != nil {
			log.Println(err)
			return
		}
	}
	recv.network.BroadcastDKGPrivateShare(prv)
}

// Consensus implements DEXON Consensus algorithm.
type Consensus struct {
	// Node Info.
	ID            types.NodeID
	authModule    *Authenticator
	currentConfig *types.Config

	// Modules.
	nbModule *nonBlocking

	// BA.
	baModules []*agreement
	receivers []*consensusBAReceiver

	// DKG.
	dkgRunning int32
	dkgReady   *sync.Cond
	cfgModule  *configurationChain

	// Dexon consensus modules.
	rbModule *reliableBroadcast
	toModule *totalOrdering
	ctModule *consensusTimestamp
	ccModule *compactionChain

	// Interfaces.
	db        blockdb.BlockDatabase
	gov       Governance
	network   Network
	tickerObj Ticker

	// Misc.
	nodeSetCache *NodeSetCache
	round        uint64
	lock         sync.RWMutex
	ctx          context.Context
	ctxCancel    context.CancelFunc
}

// NewConsensus construct an Consensus instance.
func NewConsensus(
	app Application,
	gov Governance,
	db blockdb.BlockDatabase,
	network Network,
	prv crypto.PrivateKey) *Consensus {

	// TODO(w): load latest blockHeight from DB, and use config at that height.
	var round uint64
	config := gov.GetConfiguration(round)
	// TODO(w): notarySet is different for each chain, need to write a
	// GetNotarySetForChain(nodeSet, shardID, chainID, crs) function to get the
	// correct notary set for a given chain.
	nodeSetCache := NewNodeSetCache(gov)
	crs := gov.GetCRS(round)
	// Setup acking by information returned from Governace.
	nodes, err := nodeSetCache.GetNodeSet(0)
	if err != nil {
		panic(err)
	}
	rb := newReliableBroadcast()
	rb.setChainNum(config.NumChains)
	for nID := range nodes.IDs {
		rb.addNode(nID)
	}
	// Setup context.
	ctx, ctxCancel := context.WithCancel(context.Background())

	// Setup sequencer by information returned from Governace.
	to := newTotalOrdering(
		uint64(config.K),
		uint64(float32(len(nodes.IDs)-1)*config.PhiRatio+1),
		config.NumChains)

	ID := types.NewNodeID(prv.PublicKey())
	authModule := NewAuthenticator(prv)
	cfgModule := newConfigurationChain(
		ID,
		&consensusDKGReceiver{
			ID:           ID,
			gov:          gov,
			authModule:   authModule,
			nodeSetCache: nodeSetCache,
			network:      network,
		},
		gov)
	// Register DKG for the initial round. This is a temporary function call for
	// simulation.
	cfgModule.registerDKG(0, len(nodes.IDs)/3)

	// Check if the application implement Debug interface.
	debug, _ := app.(Debug)
	con := &Consensus{
		ID:            ID,
		currentConfig: config,
		rbModule:      rb,
		toModule:      to,
		ctModule:      newConsensusTimestamp(),
		ccModule:      newCompactionChain(db),
		nbModule:      newNonBlocking(app, debug),
		gov:           gov,
		db:            db,
		network:       network,
		tickerObj:     newTicker(gov, TickerBA),
		dkgReady:      sync.NewCond(&sync.Mutex{}),
		cfgModule:     cfgModule,
		nodeSetCache:  nodeSetCache,
		ctx:           ctx,
		ctxCancel:     ctxCancel,
		authModule:    authModule,
	}

	con.baModules = make([]*agreement, config.NumChains)
	con.receivers = make([]*consensusBAReceiver, config.NumChains)
	for i := uint32(0); i < config.NumChains; i++ {
		chainID := i
		recv := &consensusBAReceiver{
			consensus:     con,
			chainID:       chainID,
			restartNotary: make(chan bool, 1),
		}
		agreementModule := newAgreement(
			con.ID,
			recv,
			nodes.IDs,
			newGenesisLeaderSelector(crs),
			con.authModule,
		)
		// Hacky way to make agreement module self contained.
		recv.agreementModule = agreementModule
		con.baModules[chainID] = agreementModule
		con.receivers[chainID] = recv
	}
	return con
}

// Run starts running DEXON Consensus.
func (con *Consensus) Run() {
	go con.processMsg(con.network.ReceiveChan())
	con.runDKGTSIG()
	con.dkgReady.L.Lock()
	defer con.dkgReady.L.Unlock()
	for con.dkgRunning != 2 {
		con.dkgReady.Wait()
	}
	ticks := make([]chan struct{}, 0, con.currentConfig.NumChains)
	for i := uint32(0); i < con.currentConfig.NumChains; i++ {
		tick := make(chan struct{})
		ticks = append(ticks, tick)
		go con.runBA(i, tick)
	}
	go con.processWitnessData()

	// Reset ticker.
	<-con.tickerObj.Tick()
	<-con.tickerObj.Tick()
	for {
		<-con.tickerObj.Tick()
		for _, tick := range ticks {
			go func(tick chan struct{}) { tick <- struct{}{} }(tick)
		}
	}
}

func (con *Consensus) runBA(chainID uint32, tick <-chan struct{}) {
	// TODO(jimmy-dexon): move this function inside agreement.
	agreement := con.baModules[chainID]
	recv := con.receivers[chainID]
	recv.restartNotary <- true
	nIDs := make(map[types.NodeID]struct{})
	// Reset ticker
	<-tick
BALoop:
	for {
		select {
		case <-con.ctx.Done():
			break BALoop
		default:
		}
		for i := 0; i < agreement.clocks(); i++ {
			<-tick
		}
		select {
		case newNotary := <-recv.restartNotary:
			if newNotary {
				nodes, err := con.nodeSetCache.GetNodeSet(con.round)
				if err != nil {
					panic(err)
				}
				nIDs = nodes.GetSubSet(con.gov.GetConfiguration(con.round).NumNotarySet,
					types.NewNotarySetTarget(con.gov.GetCRS(con.round), 0, chainID))
			}
			aID := types.Position{
				ShardID: 0,
				ChainID: chainID,
				Height:  con.rbModule.nextHeight(chainID),
			}
			agreement.restart(nIDs, aID)
		default:
		}
		err := agreement.nextState()
		if err != nil {
			log.Printf("[%s] %s\n", con.ID.String(), err)
			break BALoop
		}
	}
}

// runDKGTSIG starts running DKG+TSIG protocol.
func (con *Consensus) runDKGTSIG() {
	con.dkgReady.L.Lock()
	defer con.dkgReady.L.Unlock()
	if con.dkgRunning != 0 {
		return
	}
	con.dkgRunning = 1
	go func() {
		defer func() {
			con.dkgReady.L.Lock()
			defer con.dkgReady.L.Unlock()
			con.dkgReady.Broadcast()
			con.dkgRunning = 2
		}()
		round := con.round
		if err := con.cfgModule.runDKG(round); err != nil {
			panic(err)
		}
		nodes, err := con.nodeSetCache.GetNodeSet(round)
		if err != nil {
			panic(err)
		}
		hash := HashConfigurationBlock(
			nodes.IDs,
			con.gov.GetConfiguration(round),
			common.Hash{},
			con.cfgModule.prevHash)
		psig, err := con.cfgModule.preparePartialSignature(round, hash)
		if err != nil {
			panic(err)
		}
		if err = con.authModule.SignDKGPartialSignature(psig); err != nil {
			panic(err)
		}
		if err = con.cfgModule.processPartialSignature(psig); err != nil {
			panic(err)
		}
		con.network.BroadcastDKGPartialSignature(psig)
		if _, err = con.cfgModule.runBlockTSig(round, hash); err != nil {
			panic(err)
		}
	}()
}

// Stop the Consensus core.
func (con *Consensus) Stop() {
	con.ctxCancel()
}

func (con *Consensus) processMsg(msgChan <-chan interface{}) {
	for {
		var msg interface{}
		select {
		case msg = <-msgChan:
		case <-con.ctx.Done():
			return
		}

		switch val := msg.(type) {
		case *types.Block:
			if err := con.preProcessBlock(val); err != nil {
				log.Println(err)
			}
		case *types.WitnessAck:
			if err := con.ProcessWitnessAck(val); err != nil {
				log.Println(err)
			}
		case *types.Vote:
			if err := con.ProcessVote(val); err != nil {
				log.Println(err)
			}
		case *types.DKGPrivateShare:
			if err := con.cfgModule.processPrivateShare(val); err != nil {
				log.Println(err)
			}

		case *types.DKGPartialSignature:
			if err := con.cfgModule.processPartialSignature(val); err != nil {
				log.Println(err)
			}
		}
	}
}

func (con *Consensus) proposeBlock(chainID uint32) *types.Block {
	block := &types.Block{
		ProposerID: con.ID,
		Position: types.Position{
			ChainID: chainID,
			Height:  con.rbModule.nextHeight(chainID),
		},
	}
	if err := con.prepareBlock(block, time.Now().UTC()); err != nil {
		log.Println(err)
		return nil
	}
	// TODO(mission): decide CRS by block's round, which could be determined by
	//                block's info (ex. position, timestamp).
	if err := con.authModule.SignCRS(
		block, crypto.Keccak256Hash(con.gov.GetCRS(0))); err != nil {
		log.Println(err)
		return nil
	}
	return block
}

// ProcessVote is the entry point to submit ont vote to a Consensus instance.
func (con *Consensus) ProcessVote(vote *types.Vote) (err error) {
	v := vote.Clone()
	err = con.baModules[v.Position.ChainID].processVote(v)
	return err
}

// processWitnessData process witness acks.
func (con *Consensus) processWitnessData() {
	ch := con.nbModule.BlockProcessedChan()

	for {
		select {
		case <-con.ctx.Done():
			return
		case result := <-ch:
			block, err := con.db.Get(result.BlockHash)
			if err != nil {
				panic(err)
			}
			block.Witness.Data = result.Data
			if err := con.db.Update(block); err != nil {
				panic(err)
			}
			// TODO(w): move the acking interval into governance.
			if block.Witness.Height%5 != 0 {
				continue
			}
			witnessAck, err := con.authModule.SignAsWitnessAck(&block)
			if err != nil {
				panic(err)
			}
			err = con.ProcessWitnessAck(witnessAck)
			if err != nil {
				panic(err)
			}
			con.nbModule.WitnessAckDelivered(witnessAck)
		}
	}
}

// sanityCheck checks if the block is a valid block
func (con *Consensus) sanityCheck(b *types.Block) (err error) {
	// Check block.Position.
	if b.Position.ShardID != 0 || b.Position.ChainID >= con.rbModule.chainNum() {
		return ErrIncorrectBlockPosition
	}
	// Check the timestamp of block.
	if !b.IsGenesis() {
		chainTime := con.rbModule.chainTime(b.Position.ChainID)
		if b.Timestamp.Before(chainTime.Add(con.currentConfig.MinBlockInterval)) ||
			b.Timestamp.After(chainTime.Add(con.currentConfig.MaxBlockInterval)) {
			return ErrIncorrectBlockTime
		}
	}
	// Check the hash of block.
	hash, err := hashBlock(b)
	if err != nil || hash != b.Hash {
		return ErrIncorrectHash
	}

	// Check the signer.
	pubKey, err := crypto.SigToPub(b.Hash, b.Signature)
	if err != nil {
		return err
	}
	if !b.ProposerID.Equal(crypto.Keccak256Hash(pubKey.Bytes())) {
		return ErrIncorrectSignature
	}
	return nil
}

// preProcessBlock performs Byzantine Agreement on the block.
func (con *Consensus) preProcessBlock(b *types.Block) (err error) {
	if err := con.sanityCheck(b); err != nil {
		return err
	}
	if err := con.baModules[b.Position.ChainID].processBlock(b); err != nil {
		return err
	}
	return
}

// processBlock is the entry point to submit one block to a Consensus instance.
func (con *Consensus) processBlock(block *types.Block) (err error) {
	if err := con.sanityCheck(block); err != nil {
		return err
	}
	var (
		deliveredBlocks []*types.Block
		earlyDelivered  bool
	)
	// To avoid application layer modify the content of block during
	// processing, we should always operate based on the cloned one.
	b := block.Clone()

	con.lock.Lock()
	defer con.lock.Unlock()
	// Perform reliable broadcast checking.
	if err = con.rbModule.processBlock(b); err != nil {
		return err
	}
	con.nbModule.BlockConfirmed(block.Hash)
	for _, b := range con.rbModule.extractBlocks() {
		// Notify application layer that some block is strongly acked.
		con.nbModule.StronglyAcked(b.Hash)
		// Perform total ordering.
		deliveredBlocks, earlyDelivered, err = con.toModule.processBlock(b)
		if err != nil {
			return
		}
		if len(deliveredBlocks) == 0 {
			continue
		}
		for _, b := range deliveredBlocks {
			if err = con.db.Put(*b); err != nil {
				return
			}
		}
		// TODO(mission): handle membership events here.
		hashes := make(common.Hashes, len(deliveredBlocks))
		for idx := range deliveredBlocks {
			hashes[idx] = deliveredBlocks[idx].Hash
		}
		con.nbModule.TotalOrderingDelivered(hashes, earlyDelivered)
		// Perform timestamp generation.
		err = con.ctModule.processBlocks(deliveredBlocks)
		if err != nil {
			return
		}
		for _, b := range deliveredBlocks {
			if err = con.ccModule.processBlock(b); err != nil {
				return
			}
			if err = con.db.Update(*b); err != nil {
				return
			}
			con.nbModule.BlockDelivered(*b)
			// TODO(mission): Find a way to safely recycle the block.
			//                We should deliver block directly to
			//                nonBlocking and let them recycle the
			//                block.
		}
	}
	return
}

func (con *Consensus) checkPrepareBlock(
	b *types.Block, proposeTime time.Time) (err error) {
	if (b.ProposerID == types.NodeID{}) {
		err = &ErrMissingBlockInfo{MissingField: "ProposerID"}
		return
	}
	return
}

// PrepareBlock would setup header fields of block based on its ProposerID.
func (con *Consensus) prepareBlock(b *types.Block,
	proposeTime time.Time) (err error) {
	if err = con.checkPrepareBlock(b, proposeTime); err != nil {
		return
	}
	con.lock.RLock()
	defer con.lock.RUnlock()

	con.rbModule.prepareBlock(b)
	b.Timestamp = proposeTime
	b.Payload = con.nbModule.PreparePayload(b.Position)
	if err = con.authModule.SignBlock(b); err != nil {
		return
	}
	return
}

// PrepareGenesisBlock would setup header fields for genesis block.
func (con *Consensus) PrepareGenesisBlock(b *types.Block,
	proposeTime time.Time) (err error) {
	if err = con.checkPrepareBlock(b, proposeTime); err != nil {
		return
	}
	if len(b.Payload) != 0 {
		err = ErrGenesisBlockNotEmpty
		return
	}
	b.Position.Height = 0
	b.ParentHash = common.Hash{}
	b.Timestamp = proposeTime
	if err = con.authModule.SignBlock(b); err != nil {
		return
	}
	return
}

// ProcessWitnessAck is the entry point to submit one witness ack.
func (con *Consensus) ProcessWitnessAck(witnessAck *types.WitnessAck) (err error) {
	witnessAck = witnessAck.Clone()
	// TODO(mission): check witness set for that round.
	var round uint64
	exists, err := con.nodeSetCache.Exists(round, witnessAck.ProposerID)
	if err != nil {
		return
	}
	if !exists {
		err = ErrProposerNotInNodeSet
		return
	}
	err = con.ccModule.processWitnessAck(witnessAck)
	return
}

// WitnessAcks returns the latest WitnessAck received from all other nodes.
func (con *Consensus) WitnessAcks() map[types.NodeID]*types.WitnessAck {
	return con.ccModule.witnessAcks()
}
