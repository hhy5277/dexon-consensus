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
	"fmt"
	"sync"

	"github.com/dexon-foundation/dexon-consensus-core/common"
	"github.com/dexon-foundation/dexon-consensus-core/core/crypto"
	"github.com/dexon-foundation/dexon-consensus-core/core/crypto/dkg"
	"github.com/dexon-foundation/dexon-consensus-core/core/types"
)

// Errors for dkg module.
var (
	ErrNotDKGParticipant = fmt.Errorf(
		"not a DKG participant")
	ErrNotQualifyDKGParticipant = fmt.Errorf(
		"not a qualified DKG participant")
	ErrIDShareNotFound = fmt.Errorf(
		"private share not found for specific ID")
	ErrNotReachThreshold = fmt.Errorf(
		"threshold not reach")
	ErrIncorrectPrivateShareSignature = fmt.Errorf(
		"incorrect private share signature")
	ErrMismatchPartialSignatureHash = fmt.Errorf(
		"mismatch partialSignature hash")
	ErrIncorrectPartialSignatureSignature = fmt.Errorf(
		"incorrect partialSignature signature")
	ErrIncorrectPartialSignature = fmt.Errorf(
		"incorrect partialSignature")
	ErrNotEnoughtPartialSignatures = fmt.Errorf(
		"not enough of partial signatures")
	ErrRoundAlreadyPurged = fmt.Errorf(
		"cache of round already been purged")
)

type dkgReceiver interface {
	// ProposeDKGComplaint proposes a DKGComplaint.
	ProposeDKGComplaint(complaint *types.DKGComplaint)

	// ProposeDKGMasterPublicKey propose a DKGMasterPublicKey.
	ProposeDKGMasterPublicKey(mpk *types.DKGMasterPublicKey)

	// ProposeDKGPrivateShare propose a DKGPrivateShare.
	ProposeDKGPrivateShare(prv *types.DKGPrivateShare)

	// ProposeDKGAntiNackComplaint propose a DKGPrivateShare as an anti complaint.
	ProposeDKGAntiNackComplaint(prv *types.DKGPrivateShare)

	// ProposeDKGFinalize propose a DKGFinalize message.
	ProposeDKGFinalize(final *types.DKGFinalize)
}

type dkgProtocol struct {
	ID                 types.NodeID
	recv               dkgReceiver
	round              uint64
	threshold          int
	idMap              map[types.NodeID]dkg.ID
	mpkMap             map[types.NodeID]*dkg.PublicKeyShares
	masterPrivateShare *dkg.PrivateKeyShares
	prvShares          *dkg.PrivateKeyShares
	prvSharesReceived  map[types.NodeID]struct{}
	nodeComplained     map[types.NodeID]struct{}
	// Complaint[from][to]'s anti is saved to antiComplaint[from][to].
	antiComplaintReceived map[types.NodeID]map[types.NodeID]struct{}
}

type dkgShareSecret struct {
	privateKey *dkg.PrivateKey
}

// DKGGroupPublicKey is the result of DKG protocol.
type DKGGroupPublicKey struct {
	round          uint64
	qualifyIDs     dkg.IDs
	qualifyNodeIDs map[types.NodeID]struct{}
	idMap          map[types.NodeID]dkg.ID
	publicKeys     map[types.NodeID]*dkg.PublicKey
	groupPublicKey *dkg.PublicKey
	threshold      int
}

// TSigVerifier is the interface verifying threshold signature.
type TSigVerifier interface {
	VerifySignature(hash common.Hash, sig crypto.Signature) bool
}

// TSigVerifierCache is the cache for TSigVerifier.
type TSigVerifierCache struct {
	gov       Governance
	verifier  map[uint64]TSigVerifier
	minRound  uint64
	cacheSize int
	lock      sync.RWMutex
}

type tsigProtocol struct {
	groupPublicKey *DKGGroupPublicKey
	hash           common.Hash
	sigs           map[dkg.ID]dkg.PartialSignature
	threshold      int
}

func newDKGID(ID types.NodeID) dkg.ID {
	return dkg.NewID(ID.Hash[:])
}

func newDKGProtocol(
	ID types.NodeID,
	recv dkgReceiver,
	round uint64,
	threshold int) *dkgProtocol {

	prvShare, pubShare := dkg.NewPrivateKeyShares(threshold)

	recv.ProposeDKGMasterPublicKey(&types.DKGMasterPublicKey{
		ProposerID:      ID,
		Round:           round,
		DKGID:           newDKGID(ID),
		PublicKeyShares: *pubShare,
	})

	return &dkgProtocol{
		ID:                    ID,
		recv:                  recv,
		round:                 round,
		threshold:             threshold,
		idMap:                 make(map[types.NodeID]dkg.ID),
		mpkMap:                make(map[types.NodeID]*dkg.PublicKeyShares),
		masterPrivateShare:    prvShare,
		prvShares:             dkg.NewEmptyPrivateKeyShares(),
		prvSharesReceived:     make(map[types.NodeID]struct{}),
		nodeComplained:        make(map[types.NodeID]struct{}),
		antiComplaintReceived: make(map[types.NodeID]map[types.NodeID]struct{}),
	}
}

func (d *dkgProtocol) processMasterPublicKeys(
	mpks []*types.DKGMasterPublicKey) error {
	d.idMap = make(map[types.NodeID]dkg.ID, len(mpks))
	d.mpkMap = make(map[types.NodeID]*dkg.PublicKeyShares, len(mpks))
	d.prvSharesReceived = make(map[types.NodeID]struct{}, len(mpks))
	ids := make(dkg.IDs, len(mpks))
	for i := range mpks {
		nID := mpks[i].ProposerID
		d.idMap[nID] = mpks[i].DKGID
		d.mpkMap[nID] = &mpks[i].PublicKeyShares
		ids[i] = mpks[i].DKGID
	}
	d.masterPrivateShare.SetParticipants(ids)
	for _, mpk := range mpks {
		share, ok := d.masterPrivateShare.Share(mpk.DKGID)
		if !ok {
			return ErrIDShareNotFound
		}
		d.recv.ProposeDKGPrivateShare(&types.DKGPrivateShare{
			ProposerID:   d.ID,
			ReceiverID:   mpk.ProposerID,
			Round:        d.round,
			PrivateShare: *share,
		})
	}
	return nil
}

func (d *dkgProtocol) proposeNackComplaints() {
	for nID := range d.mpkMap {
		if _, exist := d.prvSharesReceived[nID]; exist {
			continue
		}
		d.recv.ProposeDKGComplaint(&types.DKGComplaint{
			ProposerID: d.ID,
			Round:      d.round,
			PrivateShare: types.DKGPrivateShare{
				ProposerID: nID,
				Round:      d.round,
			},
		})
	}
}

func (d *dkgProtocol) processNackComplaints(complaints []*types.DKGComplaint) (
	err error) {
	for _, complaint := range complaints {
		if !complaint.IsNack() {
			continue
		}
		if complaint.PrivateShare.ProposerID != d.ID {
			continue
		}
		id, exist := d.idMap[complaint.ProposerID]
		if !exist {
			err = ErrNotDKGParticipant
			continue
		}
		share, ok := d.masterPrivateShare.Share(id)
		if !ok {
			err = ErrIDShareNotFound
			continue
		}
		d.recv.ProposeDKGAntiNackComplaint(&types.DKGPrivateShare{
			ProposerID:   d.ID,
			ReceiverID:   complaint.ProposerID,
			Round:        d.round,
			PrivateShare: *share,
		})
	}
	return
}

func (d *dkgProtocol) enforceNackComplaints(complaints []*types.DKGComplaint) {
	for _, complaint := range complaints {
		if !complaint.IsNack() {
			continue
		}
		to := complaint.PrivateShare.ProposerID
		// Do not propose nack complaint to itself.
		if to == d.ID {
			continue
		}
		from := complaint.ProposerID
		// Nack complaint is already proposed.
		if from == d.ID {
			continue
		}
		if _, exist :=
			d.antiComplaintReceived[from][to]; !exist {
			d.recv.ProposeDKGComplaint(&types.DKGComplaint{
				ProposerID: d.ID,
				Round:      d.round,
				PrivateShare: types.DKGPrivateShare{
					ProposerID: to,
					Round:      d.round,
				},
			})
		}
	}
}

func (d *dkgProtocol) sanityCheck(prvShare *types.DKGPrivateShare) error {
	if _, exist := d.idMap[prvShare.ProposerID]; !exist {
		return ErrNotDKGParticipant
	}
	ok, err := verifyDKGPrivateShareSignature(prvShare)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIncorrectPrivateShareSignature
	}
	return nil
}

func (d *dkgProtocol) processPrivateShare(
	prvShare *types.DKGPrivateShare) error {
	if d.round != prvShare.Round {
		return nil
	}
	receiverID, exist := d.idMap[prvShare.ReceiverID]
	// This node is not a DKG participant, ignore the private share.
	if !exist {
		return nil
	}
	if err := d.sanityCheck(prvShare); err != nil {
		return err
	}
	mpk := d.mpkMap[prvShare.ProposerID]
	ok, err := mpk.VerifyPrvShare(receiverID, &prvShare.PrivateShare)
	if err != nil {
		return err
	}
	if prvShare.ReceiverID == d.ID {
		d.prvSharesReceived[prvShare.ProposerID] = struct{}{}
	}
	if !ok {
		if _, exist := d.nodeComplained[prvShare.ProposerID]; exist {
			return nil
		}
		complaint := &types.DKGComplaint{
			ProposerID:   d.ID,
			Round:        d.round,
			PrivateShare: *prvShare,
		}
		d.nodeComplained[prvShare.ProposerID] = struct{}{}
		d.recv.ProposeDKGComplaint(complaint)
	} else if prvShare.ReceiverID == d.ID {
		sender := d.idMap[prvShare.ProposerID]
		if err := d.prvShares.AddShare(sender, &prvShare.PrivateShare); err != nil {
			return err
		}
	} else {
		// The prvShare is an anti complaint.
		if _, exist := d.antiComplaintReceived[prvShare.ReceiverID]; !exist {
			d.antiComplaintReceived[prvShare.ReceiverID] =
				make(map[types.NodeID]struct{})
			d.recv.ProposeDKGAntiNackComplaint(prvShare)
		}
		d.antiComplaintReceived[prvShare.ReceiverID][prvShare.ProposerID] =
			struct{}{}
	}
	return nil
}

func (d *dkgProtocol) proposeFinalize() {
	d.recv.ProposeDKGFinalize(&types.DKGFinalize{
		ProposerID: d.ID,
		Round:      d.round,
	})
}

func (d *dkgProtocol) recoverShareSecret(qualifyIDs dkg.IDs) (
	*dkgShareSecret, error) {
	if len(qualifyIDs) < d.threshold {
		return nil, ErrNotReachThreshold
	}
	prvKey, err := d.prvShares.RecoverPrivateKey(qualifyIDs)
	if err != nil {
		return nil, err
	}
	return &dkgShareSecret{
		privateKey: prvKey,
	}, nil
}

func (ss *dkgShareSecret) sign(hash common.Hash) dkg.PartialSignature {
	// DKG sign will always success.
	sig, _ := ss.privateKey.Sign(hash)
	return dkg.PartialSignature(sig)
}

// NewDKGGroupPublicKey creats a DKGGroupPublicKey instance.
func NewDKGGroupPublicKey(
	round uint64,
	mpks []*types.DKGMasterPublicKey, complaints []*types.DKGComplaint,
	threshold int) (
	*DKGGroupPublicKey, error) {
	// Calculate qualify members.
	disqualifyIDs := map[types.NodeID]struct{}{}
	complaintsByID := map[types.NodeID]int{}
	for _, complaint := range complaints {
		if complaint.IsNack() {
			complaintsByID[complaint.PrivateShare.ProposerID]++
		} else {
			disqualifyIDs[complaint.PrivateShare.ProposerID] = struct{}{}
		}
	}
	for nID, num := range complaintsByID {
		if num > threshold {
			disqualifyIDs[nID] = struct{}{}
		}
	}
	qualifyIDs := make(dkg.IDs, 0, len(mpks)-len(disqualifyIDs))
	qualifyNodeIDs := make(map[types.NodeID]struct{})
	mpkMap := make(map[dkg.ID]*types.DKGMasterPublicKey, cap(qualifyIDs))
	idMap := make(map[types.NodeID]dkg.ID)
	for _, mpk := range mpks {
		if _, exist := disqualifyIDs[mpk.ProposerID]; exist {
			continue
		}
		mpkMap[mpk.DKGID] = mpk
		idMap[mpk.ProposerID] = mpk.DKGID
		qualifyIDs = append(qualifyIDs, mpk.DKGID)
		qualifyNodeIDs[mpk.ProposerID] = struct{}{}
	}
	// Recover qualify members' public key.
	pubKeys := make(map[types.NodeID]*dkg.PublicKey, len(qualifyIDs))
	for _, recvID := range qualifyIDs {
		pubShares := dkg.NewEmptyPublicKeyShares()
		for _, id := range qualifyIDs {
			pubShare, err := mpkMap[id].PublicKeyShares.Share(recvID)
			if err != nil {
				return nil, err
			}
			if err := pubShares.AddShare(id, pubShare); err != nil {
				return nil, err
			}
		}
		pubKey, err := pubShares.RecoverPublicKey(qualifyIDs)
		if err != nil {
			return nil, err
		}
		pubKeys[mpkMap[recvID].ProposerID] = pubKey
	}
	// Recover Group Public Key.
	pubShares := make([]*dkg.PublicKeyShares, 0, len(qualifyIDs))
	for _, id := range qualifyIDs {
		pubShares = append(pubShares, &mpkMap[id].PublicKeyShares)
	}
	groupPK := dkg.RecoverGroupPublicKey(pubShares)
	return &DKGGroupPublicKey{
		round:          round,
		qualifyIDs:     qualifyIDs,
		qualifyNodeIDs: qualifyNodeIDs,
		idMap:          idMap,
		publicKeys:     pubKeys,
		threshold:      threshold,
		groupPublicKey: groupPK,
	}, nil
}

// VerifySignature verifies if the signature is correct.
func (gpk *DKGGroupPublicKey) VerifySignature(
	hash common.Hash, sig crypto.Signature) bool {
	return gpk.groupPublicKey.VerifySignature(hash, sig)
}

// NewTSigVerifierCache creats a DKGGroupPublicKey instance.
func NewTSigVerifierCache(gov Governance, cacheSize int) *TSigVerifierCache {
	return &TSigVerifierCache{
		gov:       gov,
		verifier:  make(map[uint64]TSigVerifier),
		cacheSize: cacheSize,
	}
}

// UpdateAndGet calls Update and then Get.
func (tc *TSigVerifierCache) UpdateAndGet(round uint64) (
	TSigVerifier, bool, error) {
	ok, err := tc.Update(round)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	v, ok := tc.Get(round)
	return v, ok, nil
}

// Update the cache and returns if success.
func (tc *TSigVerifierCache) Update(round uint64) (bool, error) {
	tc.lock.Lock()
	defer tc.lock.Unlock()
	if round < tc.minRound {
		return false, ErrRoundAlreadyPurged
	}
	if _, exist := tc.verifier[round]; exist {
		return true, nil
	}
	if !tc.gov.IsDKGFinal(round) {
		return false, nil
	}
	gpk, err := NewDKGGroupPublicKey(round,
		tc.gov.DKGMasterPublicKeys(round),
		tc.gov.DKGComplaints(round),
		int(tc.gov.Configuration(round).DKGSetSize/3)+1)
	if err != nil {
		return false, err
	}
	if len(tc.verifier) == 0 {
		tc.minRound = round
	}
	tc.verifier[round] = gpk
	if len(tc.verifier) > tc.cacheSize {
		delete(tc.verifier, tc.minRound)
	}
	for {
		if _, exist := tc.verifier[tc.minRound]; !exist {
			tc.minRound++
		} else {
			break
		}
	}
	return true, nil
}

// Get the TSigVerifier of round and returns if it exists.
func (tc *TSigVerifierCache) Get(round uint64) (TSigVerifier, bool) {
	tc.lock.RLock()
	defer tc.lock.RUnlock()
	verifier, exist := tc.verifier[round]
	return verifier, exist
}

func newTSigProtocol(
	gpk *DKGGroupPublicKey,
	hash common.Hash) *tsigProtocol {
	return &tsigProtocol{
		groupPublicKey: gpk,
		hash:           hash,
		sigs:           make(map[dkg.ID]dkg.PartialSignature, gpk.threshold+1),
	}
}

func (tsig *tsigProtocol) sanityCheck(psig *types.DKGPartialSignature) error {
	_, exist := tsig.groupPublicKey.publicKeys[psig.ProposerID]
	if !exist {
		return ErrNotQualifyDKGParticipant
	}
	ok, err := verifyDKGPartialSignatureSignature(psig)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIncorrectPartialSignatureSignature
	}
	if psig.Hash != tsig.hash {
		return ErrMismatchPartialSignatureHash
	}
	return nil
}

func (tsig *tsigProtocol) processPartialSignature(
	psig *types.DKGPartialSignature) error {
	if psig.Round != tsig.groupPublicKey.round {
		return nil
	}
	id, exist := tsig.groupPublicKey.idMap[psig.ProposerID]
	if !exist {
		return ErrNotQualifyDKGParticipant
	}
	if err := tsig.sanityCheck(psig); err != nil {
		return err
	}
	pubKey := tsig.groupPublicKey.publicKeys[psig.ProposerID]
	if !pubKey.VerifySignature(
		tsig.hash, crypto.Signature(psig.PartialSignature)) {
		return ErrIncorrectPartialSignature
	}
	tsig.sigs[id] = psig.PartialSignature
	return nil
}

func (tsig *tsigProtocol) signature() (crypto.Signature, error) {
	if len(tsig.sigs) < tsig.groupPublicKey.threshold {
		return crypto.Signature{}, ErrNotEnoughtPartialSignatures
	}
	ids := make(dkg.IDs, 0, len(tsig.sigs))
	psigs := make([]dkg.PartialSignature, 0, len(tsig.sigs))
	for id, psig := range tsig.sigs {
		ids = append(ids, id)
		psigs = append(psigs, psig)
	}
	return dkg.RecoverSignature(psigs, ids)
}
