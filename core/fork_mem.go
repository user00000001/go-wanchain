package core

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"

	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/core/types"
	"github.com/wanchain/go-wanchain/pos"
	"github.com/wanchain/go-wanchain/pos/posdb"

	"github.com/wanchain/go-wanchain/crypto"
	"encoding/json"
	"github.com/wanchain/go-wanchain/core/vm"
	"crypto/ecdsa"

	"github.com/wanchain/go-wanchain/log"
	"time"
)

type chainType uint

type RbLeadersSelInt interface {
	GetTargetBlkNumber(epochId uint64) uint64
	GetRBProposerGroup(epochID uint64) []vm.Leader
}

type SlLeadersSelInt interface {
	GetEpochLeadersPK(epochID uint64) []*ecdsa.PublicKey
}

const (
	BLOCKCHAIN  = iota //0
	HEADERCHAIN        //1
)



type ForkMemBlockChain struct {
	useEpochGenesis	   bool
	rbLeaderSelector   RbLeadersSelInt
	slotLeaderSelector SlLeadersSelInt

	ctype 			chainType
	kBufferedChains  map[string][]common.Hash
	kBufferedBlks	 map[common.Hash]*types.Block
	curMaxBlkNum	 int64
	lock sync.RWMutex
}

func NewForkMemBlockChain(ctype chainType) *ForkMemBlockChain {

	f := &ForkMemBlockChain{}
	f.useEpochGenesis = false
	f.ctype = ctype
	f.kBufferedChains = make(map[string][]common.Hash)
	f.kBufferedBlks = make(map[common.Hash]*types.Block)
	f.curMaxBlkNum = 0
	return f
}

type BlockSorter []*types.Block

//Len()
func (s BlockSorter) Len() int {
	return len(s)
}

func (s BlockSorter) Less(i, j int) bool {
	return s[i].NumberU64() < s[j].NumberU64()
}

//Swap()
func (s BlockSorter) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (f *ForkMemBlockChain) calEpochSlotIDFromTime(timeUnix uint64) (epochId uint64, slotId uint64) {
	if pos.EpochBaseTime == 0 {
		return
	}

	epochTimespan := uint64(pos.SlotTime * pos.SlotCount)
	epochId = uint64((timeUnix - pos.EpochBaseTime) / epochTimespan)
	slotId = uint64((timeUnix - pos.EpochBaseTime) / pos.SlotTime % pos.SlotCount)
	return
}

func (f *ForkMemBlockChain) GetBlockEpochIdAndSlotId(header *types.Header) (blkEpochId uint64, blkSlotId uint64, err error) {
	blkTime := header.Time.Uint64()

	blkTd := header.Difficulty.Uint64()

	blkEpochId = (blkTd >> 32)
	blkSlotId = ((blkTd & 0xffffffff) >> 8)

	calEpochId, calSlotId := f.calEpochSlotIDFromTime(blkTime)
	//calEpochId,calSlotId := uint64(blkTime),uint64(blkTime)

	if calEpochId != blkEpochId {
		fmt.Println(calEpochId, blkEpochId, calSlotId, blkSlotId)
		return 0, 0, errors.New("epochid and slotid is not match with blk time")
	}

	return
}


func (f *ForkMemBlockChain) CalEpochSlotID() (uint64,uint64){

	if pos.EpochBaseTime == 0 {
		return 0,0
	}

	timeUnix := uint64(time.Now().Unix())
	epochTimespan := uint64(pos.SlotTime * pos.SlotCount)
	curEpochId := uint64((timeUnix - pos.EpochBaseTime) / epochTimespan)
	curSlotId := uint64((timeUnix - pos.EpochBaseTime) / pos.SlotTime % pos.SlotCount)
	//slottools.CurEpochID = curEpochId
	fmt.Println("CalEpochSlotID:", curEpochId, curSlotId)
	return curEpochId,curSlotId
}

func (f *ForkMemBlockChain) Maxvalid(workBlk *types.Block) (types.Blocks, error) {

	f.lock.Lock()
	defer f.lock.Unlock()

	var chainBlks types.Blocks
	var midSidBlk *types.Block

	if workBlk == nil {
		return nil, errors.New("can not get current block in working chain")
	}

	workBlkNum := int64(workBlk.NumberU64())

	fmt.Println("begin select maxvalid workblk=", workBlkNum)

	//if work block is in the highest one or higher than buffer,use work blk,work chain will not change
	if workBlkNum >= f.curMaxBlkNum {
		return nil, nil
	}

	maxNumKey := big.NewInt(int64(f.curMaxBlkNum)).Text(10)
	hashs := f.kBufferedChains[maxNumKey]

	minSid := ^uint64(0)
	midSidBlk = f.kBufferedBlks[hashs[0]]
	epidOld := uint64(0)
	//if there are more
	if len(hashs) > 1 {
		//same block height
		for _, hs := range hashs {

			blk := f.kBufferedBlks[hs]

			epidNew, sid, err := f.GetBlockEpochIdAndSlotId(blk.Header())
			if err != nil {
				continue
			}

			fmt.Print("maxvalid block hash", common.ToHex(hs[:]), "  block number", blk.NumberU64(), epidNew, sid)

			if epidOld == 0 {
				epidOld = epidNew
			}

			if sid < minSid {
				minSid = sid
				midSidBlk = blk
			}
		}
	}
	// reduce new chain
	for ; midSidBlk != nil && int64(midSidBlk.NumberU64()) != workBlkNum; midSidBlk = f.kBufferedBlks[midSidBlk.ParentHash()] {
		chainBlks = append(chainBlks, midSidBlk)
	}

	//find common prefix
	if midSidBlk != nil && midSidBlk.Hash() != workBlk.Hash() {
		for {
			chainBlks = append(chainBlks, midSidBlk)
			if (workBlk != nil && workBlk.NumberU64() == 1) || (workBlk != nil && workBlk.Hash() == midSidBlk.Hash() && workBlk.NumberU64() == midSidBlk.NumberU64()) {
				break
			}

			midSidBlk = f.kBufferedBlks[midSidBlk.ParentHash()]
			if midSidBlk == nil {
				return nil, errors.New("can not find common prefix")
			}

			workBlk = f.kBufferedBlks[workBlk.ParentHash()]
			if workBlk == nil {
				return nil, errors.New("can not find common prefix")
			}

		}
	}

	sort.Sort(BlockSorter(chainBlks))
	fmt.Println("maxValid \n\n")
	for _, blk := range chainBlks {
		fmt.Println("blkNum=", blk.NumberU64(), " hash=", common.ToHex(blk.Hash().Bytes()))
	}

	fmt.Println("end select maxvalid workBlkNum=", workBlkNum)
	return chainBlks, nil
}

func (f *ForkMemBlockChain) PushHeaders(headerChain []*types.Header) error {
	fmt.Println("begin pushHeaders")
	if f.ctype != HEADERCHAIN {
		return errors.New("error chain type which require HEADERCHAIN")
	}

	for _, header := range headerChain {

		blk := types.NewBlockWithHeader(header)
		err := f.Push(blk)
		if err != nil {
			return err
		}

	}
	fmt.Println("end pushHeaders")
	return nil
}

func (f *ForkMemBlockChain) PushBlocks(blockChain types.Blocks) error {
	fmt.Println("push block begin")

	if f.ctype != BLOCKCHAIN {
		return errors.New("error chain type which require BLOCKCHAIN")
	}

	for _, blk := range blockChain {
		err := f.Push(blk)
		if err != nil {
			return err
		}
	}

	fmt.Println("push block end")
	return nil
}

func (f *ForkMemBlockChain) Push(block *types.Block) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	newbn := int64(block.NumberU64())

	if f.curMaxBlkNum == 0 {
		f.curMaxBlkNum = newbn
	} else {
		//input need to be continous block
		if f.curMaxBlkNum+1 == newbn {
			f.curMaxBlkNum = newbn
		}
	}

	num := block.Number().Text(10)

	f.kBufferedChains[num] = append(f.kBufferedChains[num], block.Hash())
	f.kBufferedBlks[block.Hash()] = block

	return nil
}

func (f *ForkMemBlockChain) PopBack() {
	f.lock.Lock()
	defer f.lock.Unlock()

	//need to store k data
	if len(f.kBufferedChains) > int(pos.SlotCount) {

		blkNumBeforeK := f.curMaxBlkNum - int64(pos.SlotCount)

		if blkNumBeforeK < 0 {
			return
		}

		bnText := big.NewInt(int64(blkNumBeforeK))

		blkHashs := f.kBufferedChains[bnText.Text(10)]

		for _, bh := range blkHashs {
			delete(f.kBufferedBlks, bh)
		}

		delete(f.kBufferedChains, bnText.Text(10))
	}

	return
}

func (f *ForkMemBlockChain) PrintAllBffer() {

	for idx, blkHashs := range f.kBufferedChains {
		for _, bh := range blkHashs {
			fmt.Println("block number=", idx, " hash=", common.ToHex(bh[:]))
		}
	}

	for _, blk := range f.kBufferedBlks {
		epid, sid, _ := f.GetBlockEpochIdAndSlotId(blk.Header())
		fmt.Println(" hash=", common.ToHex(blk.Hash().Bytes()), " epid=", epid, " sid=", sid)
	}
}
//////////////////////////////////////////////////////////////////////////


func (f *ForkMemBlockChain) updateReOrg(epochId uint64, length uint64) {
	reOrgDb := posdb.GetDbByName("forkdb")
	if reOrgDb == nil {
		reOrgDb = posdb.NewDb("forkdb")
	}

	numberBytes, _ := reOrgDb.Get(epochId, "reorgNumber")

	num := uint64(0)
	if numberBytes != nil {
		num = binary.BigEndian.Uint64(numberBytes) + 1
	}

	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, num)

	reOrgDb.Put(epochId, "reorgNumber", b)

	b = make([]byte, 8)
	binary.BigEndian.PutUint64(b, length)
	reOrgDb.Put(epochId, "reorgLength", b)
}

func (f *ForkMemBlockChain) updateFork(epochId uint64) {
	reOrgDb := posdb.GetDbByName("forkdb")
	if reOrgDb == nil {
		reOrgDb = posdb.NewDb("forkdb")
	}

	numberBytes, _ := reOrgDb.Get(0, "forkNumber")

	num := uint64(0)
	if numberBytes != nil {

		num = binary.BigEndian.Uint64(numberBytes) + 1
	}

	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, num)

	reOrgDb.Put(epochId, "forkNumber", b)
}


func (f *ForkMemBlockChain) GetEpochGenesis(epochid uint64,blk *types.Block) ([]byte,error){

	epGen := &types.EpochGenesis{}
	epGen.ProtocolMagic = []byte("wanchainpos")
	epGen.EpochId = epochid
	epGen.PreEpochLastBlkHash = blk.Hash()

	f.rbLeaderSelector.GetRBProposerGroup(epochid)


	epGen.SlotLeaders = make([][]byte,0)
	pks := f.slotLeaderSelector.GetEpochLeadersPK(epochid)
	if pks!=nil {
		for _, slpk := range pks {
			epGen.SlotLeaders = append(epGen.SlotLeaders, crypto.FromECDSAPub(slpk))
		}
	}

	byteVal,err := json.Marshal(epGen)

	if err!=nil {
		return nil,err
	}

	epGen.GenesisBlkHash = crypto.Keccak256Hash(byteVal)

	return json.Marshal(epGen)
}

func (f *ForkMemBlockChain) VerifyEpochGenesis(bc *BlockChain,blk *types.Block) bool{

	if !f.useEpochGenesis {
		return true
	}


	epGen := &types.EpochGenesis{}
	epGen.ProtocolMagic = []byte("wanchainpos")

	epochid,_,err:= f.GetBlockEpochIdAndSlotId(blk.Header())
	if err!=nil {
		log.Info("verify genesis failed because of wrong epochid or slotid")
		return false
	}
	epGen.EpochId = epochid

	lastBlk := bc.GetBlockByNumber(blk.NumberU64()-1)
	epGen.PreEpochLastBlkHash = lastBlk.Hash()

	epGen.RBLeaders = make([][]byte,0)
	leaders := f.rbLeaderSelector.GetRBProposerGroup(epochid)
	for _,ldr := range leaders {
		epGen.RBLeaders = append(epGen.RBLeaders,ldr.PubSec256)
	}


	epGen.SlotLeaders = make([][]byte,0)
	pks := f.slotLeaderSelector.GetEpochLeadersPK(epochid)
	if pks!=nil {
		for _, slpk := range pks {
			epGen.SlotLeaders = append(epGen.SlotLeaders, crypto.FromECDSAPub(slpk))
		}
	}

	byteVal,err := json.Marshal(epGen)

	if err!=nil {
		log.Info("verify genesis marshal failed")
		return false
	}

	genesisBlkHash := crypto.Keccak256Hash(byteVal)

	res := (genesisBlkHash==blk.ParentHash())

	return res
}


func (f *ForkMemBlockChain) IsFirstBlockInEpoch(firstBlk *types.Block) bool {
	_,slotid,err:= f.GetBlockEpochIdAndSlotId(firstBlk.Header())
	if err!=nil {
		log.Info("verify genesis failed because of wrong epochid or slotid")
		return false
	}

	if slotid==0 {
		return true
	}

	return false
}


func (f *ForkMemBlockChain) GetLastBlkInPreEpoch(bc *BlockChain,firstBlk *types.Block) *types.Block {
	return nil
}








