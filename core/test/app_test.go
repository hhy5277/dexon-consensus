package test

import (
	"testing"
	"time"

	"github.com/dexon-foundation/dexon-consensus-core/common"
	"github.com/stretchr/testify/suite"
)

type AppTestSuite struct {
	suite.Suite

	to1, to2, to3 *totalOrderDeliver
}

func (s *AppTestSuite) SetupSuite() {
	s.to1 = &totalOrderDeliver{
		BlockHashes: common.Hashes{
			common.NewRandomHash(),
			common.NewRandomHash(),
		},
		Early: false,
	}
	s.to2 = &totalOrderDeliver{
		BlockHashes: common.Hashes{
			common.NewRandomHash(),
			common.NewRandomHash(),
			common.NewRandomHash(),
		},
		Early: false,
	}
	s.to3 = &totalOrderDeliver{
		BlockHashes: common.Hashes{
			common.NewRandomHash(),
		},
		Early: false,
	}
}

func (s *AppTestSuite) setupAppByTotalOrderDeliver(
	app *App, to *totalOrderDeliver) {

	for _, h := range to.BlockHashes {
		app.StronglyAcked(h)
	}
	app.TotalOrderingDeliver(to.BlockHashes, to.Early)
	for _, h := range to.BlockHashes {
		// To make it simpler, use the index of hash sequence
		// as the time.
		s.deliverBlockWithTimeFromSequenceLength(app, h)
	}
}

func (s *AppTestSuite) deliverBlockWithTimeFromSequenceLength(
	app *App, hash common.Hash) {

	app.DeliverBlock(
		hash,
		time.Time{}.Add(time.Duration(len(app.DeliverSequence))*time.Second))
}

func (s *AppTestSuite) TestCompare() {
	req := s.Require()

	app1 := NewApp()
	s.setupAppByTotalOrderDeliver(app1, s.to1)
	s.setupAppByTotalOrderDeliver(app1, s.to2)
	s.setupAppByTotalOrderDeliver(app1, s.to3)
	// An App with different deliver sequence.
	app2 := NewApp()
	s.setupAppByTotalOrderDeliver(app2, s.to1)
	s.setupAppByTotalOrderDeliver(app2, s.to2)
	hash := common.NewRandomHash()
	app2.StronglyAcked(hash)
	app2.TotalOrderingDeliver(common.Hashes{hash}, false)
	s.deliverBlockWithTimeFromSequenceLength(app2, hash)
	req.Equal(ErrMismatchBlockHashSequence, app1.Compare(app2))
	// An App with different consensus time for the same block.
	app3 := NewApp()
	s.setupAppByTotalOrderDeliver(app3, s.to1)
	s.setupAppByTotalOrderDeliver(app3, s.to2)
	for _, h := range s.to3.BlockHashes {
		app3.StronglyAcked(h)
	}
	app3.TotalOrderingDeliver(s.to3.BlockHashes, s.to3.Early)
	wrongTime := time.Time{}.Add(
		time.Duration(len(app3.DeliverSequence)) * time.Second)
	wrongTime = wrongTime.Add(1 * time.Second)
	app3.DeliverBlock(s.to3.BlockHashes[0], wrongTime)
	req.Equal(ErrMismatchConsensusTime, app1.Compare(app3))
	req.Equal(ErrMismatchConsensusTime, app3.Compare(app1))
	// An App without any delivered blocks.
	app4 := NewApp()
	req.Equal(ErrEmptyDeliverSequence, app4.Compare(app1))
	req.Equal(ErrEmptyDeliverSequence, app1.Compare(app4))
}

func (s *AppTestSuite) TestVerify() {
	req := s.Require()

	// An OK App instance.
	app1 := NewApp()
	s.setupAppByTotalOrderDeliver(app1, s.to1)
	s.setupAppByTotalOrderDeliver(app1, s.to2)
	s.setupAppByTotalOrderDeliver(app1, s.to3)
	req.Nil(app1.Verify())
	// A delivered block without strongly ack
	app1.DeliverBlock(common.NewRandomHash(), time.Time{})
	req.Equal(ErrDeliveredBlockNotAcked, app1.Verify())
	// The consensus time is out of order.
	app2 := NewApp()
	s.setupAppByTotalOrderDeliver(app2, s.to1)
	for _, h := range s.to2.BlockHashes {
		app2.StronglyAcked(h)
	}
	app2.TotalOrderingDeliver(s.to2.BlockHashes, s.to2.Early)
	app2.DeliverBlock(s.to2.BlockHashes[0], time.Time{})
	req.Equal(ErrConsensusTimestampOutOfOrder, app2.Verify())
	// A delivered block is not found in total ordering delivers.
	app3 := NewApp()
	s.setupAppByTotalOrderDeliver(app3, s.to1)
	hash := common.NewRandomHash()
	app3.StronglyAcked(hash)
	s.deliverBlockWithTimeFromSequenceLength(app3, hash)
	req.Equal(ErrMismatchTotalOrderingAndDelivered, app3.Verify())
	// A delivered block is not found in total ordering delivers.
	app4 := NewApp()
	s.setupAppByTotalOrderDeliver(app4, s.to1)
	for _, h := range s.to2.BlockHashes {
		app4.StronglyAcked(h)
	}
	app4.TotalOrderingDeliver(s.to2.BlockHashes, s.to2.Early)
	hash = common.NewRandomHash()
	app4.StronglyAcked(hash)
	app4.TotalOrderingDeliver(common.Hashes{hash}, false)
	s.deliverBlockWithTimeFromSequenceLength(app4, hash)
}

func TestApp(t *testing.T) {
	suite.Run(t, new(AppTestSuite))
}
