package slotleader

import (
	"crypto/ecdsa"
	Rand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/wanchain/go-wanchain/core/vm"

	"github.com/wanchain/go-wanchain/pos"

	"github.com/wanchain/go-wanchain/accounts/keystore"
	"github.com/wanchain/go-wanchain/core"
	"github.com/wanchain/go-wanchain/crypto"
	"github.com/wanchain/go-wanchain/functrace"
	"github.com/wanchain/go-wanchain/log"
	"github.com/wanchain/go-wanchain/pos/posdb"
	"github.com/wanchain/go-wanchain/pos/postools/slottools"
	"github.com/wanchain/go-wanchain/rpc"
	"github.com/wanchain/pos/uleaderselection"
)

var (
	ErrInvalidCommitParameter = errors.New("Invalid input parameters")
)

//--------------Workflow functions-------------------------------------------------------------
// Init can set import info for slotleader at startup
func (s *SlotLeaderSelection) Init(blockChain *core.BlockChain, rc *rpc.Client, key *keystore.Key, epochInstance interface{}) {
	s.blockChain = blockChain
	s.rc = rc
	s.key = key
	s.epochInstance = epochInstance
	if blockChain != nil {
		log.Info("SlotLeaderSelecton init success")
	}
}

//Loop check work every 10 second. Called by backend loop
//It's all slotLeaderSelection's main workflow loop
//It's not loop at all, it is loop called by backend
func (s *SlotLeaderSelection) Loop(rc *rpc.Client, key *keystore.Key, epochInstance interface{}, epochID uint64, slotID uint64) {
	functrace.Enter("SlotLeaderSelection Loop")
	s.rc = rc
	s.key = key
	s.epochInstance = epochInstance

	log.Info("Now epchoID and slotID:", "epochID", posdb.Uint64ToString(epochID), "slotID", posdb.Uint64ToString(slotID))
	log.Info("Last on chain epchoID and slotID:", "epochID", s.getLastEpochIDFromChain(), "slotID", s.getLastSlotIDFromChain())

	//Check if epoch is new
	s.checkNewEpochStart(epochID)

	workStage := s.getWorkStage(epochID)

	switch workStage {
	case slotLeaderSelectionInit:
		log.Debug("Enter slotLeaderSelectionInit")

		s.clearData()

		s.buildEpochLeaderGroup(epochID)

		s.setWorkingEpochID(epochID)

		err := s.generateSlotLeadsGroup(epochID)
		if err != nil {
			log.Error(err.Error())
			panic("generateSlotLeadsGroup error")
		}

		s.setWorkStage(epochID, slotLeaderSelectionStage1)

	case slotLeaderSelectionStage1:
		log.Debug("Enter slotLeaderSelectionStage1")
		// If not in current epoch leaders, Do nothing in this epoch.

		// If it's too late to run, wait for next epoch
		if slotID > pos.Stage1K/2 {
			s.setWorkStage(epochID, slotLeaderSelectionStageFinished)
			log.Warn("Passed the moment of slotLeaderSelectionStage1 wait for next epoch", "epochID", epochID, "slotID", slotID)
			break
		}

		if !s.isLocalPkInCurrentEpochLeaders() {
			s.setWorkStage(epochID, slotLeaderSelectionStageFinished)
		}

		err := s.startStage1Work()
		if err != nil {
			log.Error(err.Error())
			ErrorCountAdd()
		} else {
			s.setWorkStage(epochID, slotLeaderSelectionStage2)
		}

	case slotLeaderSelectionStage2:
		log.Debug("Enter slotLeaderSelectionStage2")

		if slotID < pos.Stage2K+1 {
			break
		}

		if slotID > pos.Stage4K {
			s.setWorkStage(epochID, slotLeaderSelectionStageFinished)
			break
		}

		err := s.startStage2Work()
		if err != nil {
			log.Error(err.Error())
			ErrorCountAdd()
		} else {
			s.setWorkStage(epochID, slotLeaderSelectionStage3)
		}

	case slotLeaderSelectionStage3:
		log.Debug("Enter slotLeaderSelectionStage3")

		if slotID < pos.Stage5K {
			break
		}

		err := s.generateSecurityMsg(epochID, s.key.PrivateKey)
		if err != nil {
			log.Warn(err.Error())
			WarnCountAdd()
		} else {
			log.Info("generateSecurityMsg SMA success!")
		}

		if err != nil && errorRetry > 0 {
			errorRetry--
			break
		}

		s.setWorkStage(epochID, slotLeaderSelectionStageFinished)
		errorRetry = 3

	case slotLeaderSelectionStageFinished:
		log.Debug("Enter slotLeaderSelectionStageFinished")

	default:
	}
	functrace.Exit()
}

// startStage1Work start the stage 1 work and send tx
func (s *SlotLeaderSelection) startStage1Work() error {
	functrace.Enter("")
	selfPublicKey, _ := s.getLocalPublicKey()

	selfPublicKeyIndex, inEpochLeaders := s.epochLeadersMap[hex.EncodeToString(crypto.FromECDSAPub(selfPublicKey))]
	if inEpochLeaders {
		log.Debug(fmt.Sprintf("Local node in epoch leaders times: %d", len(selfPublicKeyIndex)))

		workingEpochID := s.getWorkingEpochID()

		for i := 0; i < len(selfPublicKeyIndex); i++ {
			data, err := s.generateCommitment(selfPublicKey, workingEpochID, selfPublicKeyIndex[i])
			if err != nil {
				return err
			}

			err = s.sendStage1Tx(data)
			if err != nil {
				log.Error(err.Error())
				return err
			}
		}
	} else {
		log.Debug("Local node is not in epoch leaders")
	}

	functrace.Exit()
	return nil
}

// startStage2Work start the stage 2 work and send tx
func (s *SlotLeaderSelection) startStage2Work() error {

	functrace.Enter("startStage2Work")
	s.getWorkingEpochID()
	selfPublicKey, _ := s.getLocalPublicKey()
	selfPublicKeyIndex := make([]uint64, 0)
	var inEpochLeaders bool
	selfPublicKeyIndex, inEpochLeaders = s.epochLeadersMap[hex.EncodeToString(crypto.FromECDSAPub(selfPublicKey))]
	if inEpochLeaders {
		for i := 0; i < len(selfPublicKeyIndex); i++ {
			workingEpochID := s.getWorkingEpochID()
			data, err := s.buildStage2TxPayload(workingEpochID, uint64(selfPublicKeyIndex[i]))
			if err != nil {
				return err
			}

			err = s.sendStage2Tx(data)
			if err != nil {
				return err
			}
		}
	}
	functrace.Exit()
	return nil
}

//generateCommitment generate a commitment and send it by tx message
//Returns the commitment buffer []byte which is publicKey and alpha * publicKey
//payload should be send with tx.
func (s *SlotLeaderSelection) generateCommitment(publicKey *ecdsa.PublicKey,
	epochID uint64, selfIndexInEpochLeader uint64) ([]byte, error) {
	if publicKey == nil || publicKey.X == nil || publicKey.Y == nil {
		return nil, ErrInvalidCommitParameter
	}

	if !crypto.S256().IsOnCurve(publicKey.X, publicKey.Y) {
		return nil, ErrNotOnCurve
	}

	alpha, err := uleaderselection.RandFieldElement(Rand.Reader)
	if err != nil {
		return nil, err
	}
	fmt.Println("alpha:", hex.EncodeToString(alpha.Bytes()))

	commitment, err := uleaderselection.GenerateCommitment(publicKey, alpha)
	if err != nil {
		return nil, err
	}

	buffer, err := slottools.RlpPackStage1DataForTx(epochID, selfIndexInEpochLeader, commitment[1], vm.GetSlotLeaderScAbiString())

	posdb.GetDb().PutWithIndex(epochID, selfIndexInEpochLeader, "alpha", alpha.Bytes())

	log.Debug(fmt.Sprintf("----Put alpha epochID:%d, selfIndex:%d, alpha:%s, mi:%s, pk:%s", epochID, selfIndexInEpochLeader, alpha.String(), hex.EncodeToString(crypto.FromECDSAPub(commitment[1])), hex.EncodeToString(crypto.FromECDSAPub(commitment[0]))))

	return buffer, err
}

func (s *SlotLeaderSelection) checkStageValid(slotID uint64) bool {
	return true
}

func (s *SlotLeaderSelection) checkNewEpochStart(epochID uint64) {
	//If New epoch start
	workingEpochID := s.getWorkingEpochID()
	if epochID > workingEpochID {
		s.setWorkStage(epochID, slotLeaderSelectionInit)
	}
}
