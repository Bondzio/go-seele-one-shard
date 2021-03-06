/**
*  @file
*  @copyright defined in go-seele/LICENSE
 */

package seele

import (
	"errors"
	"sync"
	"time"
	"math/big"

	"github.com/seeleteam/go-seele/common"
	"github.com/seeleteam/go-seele/core"
	"github.com/seeleteam/go-seele/core/types"
	"github.com/seeleteam/go-seele/event"
	"github.com/seeleteam/go-seele/log"
	"github.com/seeleteam/go-seele/p2p"
	"github.com/seeleteam/go-seele/seele/download"
)

var (
	errSyncFinished = errors.New("Sync Finished!")
)

var (
	transactionHashMsgCode    uint16 = 0
	transactionRequestMsgCode uint16 = 1
	transactionsMsgCode       uint16 = 2
	blockHashMsgCode          uint16 = 3
	blockRequestMsgCode       uint16 = 4
	blockMsgCode              uint16 = 5

	statusDataMsgCode      uint16 = 6
	statusChainHeadMsgCode uint16 = 7

	debtMsgCode uint16 = 13

	protocolMsgCodeLength uint16 = 14
)

func codeToStr(code uint16) string {
	switch code {
	case transactionHashMsgCode:
		return "transactionHashMsgCode"
	case transactionRequestMsgCode:
		return "transactionRequestMsgCode"
	case transactionsMsgCode:
		return "transactionsMsgCode"
	case blockHashMsgCode:
		return "blockHashMsgCode"
	case blockRequestMsgCode:
		return "blockRequestMsgCode"
	case blockMsgCode:
		return "blockMsgCode"
	case statusDataMsgCode:
		return "statusDataMsgCode"
	case statusChainHeadMsgCode:
		return "statusChainHeadMsgCode"
	case debtMsgCode:
		return "debtMsgCode"
	}

	return downloader.CodeToStr(code)
}

// SeeleProtocol service implementation of seele
type SeeleProtocol struct {
	p2p.Protocol
	peerSet *peerSet

	networkID  uint64
	downloader *downloader.Downloader
	txPool     [NumOfChains]*core.TransactionPool
	debtPool   [NumOfChains]*core.DebtPool
	chain      [NumOfChains]*core.Blockchain

	wg     sync.WaitGroup
	quitCh chan struct{}
	syncCh chan struct{}
	log    *log.SeeleLog
}

// Downloader return a pointer of the downloader
func (s *SeeleProtocol) Downloader() *downloader.Downloader { return s.downloader }

// NewSeeleProtocol create SeeleProtocol
func NewSeeleProtocol(seele *SeeleService, log *log.SeeleLog) (s *SeeleProtocol, err error) {
	s = &SeeleProtocol{
		Protocol: p2p.Protocol{
			Name:    SeeleProtoName,
			Version: SeeleVersion,
			Length:  protocolMsgCodeLength,
		},
		networkID:  seele.networkID,
		txPool:     seele.TxPool(),
		debtPool:   seele.debtPools,
		chain:      seele.BlockChain(),
		downloader: downloader.NewDownloader(seele.BlockChain()),
		log:        log,
		quitCh:     make(chan struct{}),
		syncCh:     make(chan struct{}),

		peerSet: newPeerSet(),
	}

	s.Protocol.AddPeer = s.handleAddPeer
	s.Protocol.DeletePeer = s.handleDelPeer
	s.Protocol.GetPeer = s.handleGetPeer

	event.TransactionInsertedEventManager.AddAsyncListener(s.handleNewTx)
	event.BlockMinedEventManager.AddAsyncListener(s.handleNewMinedBlock)
	return s, nil
}

func (sp *SeeleProtocol) Start() {
	sp.log.Debug("SeeleProtocol.Start called!")
	go sp.syncer()
}

// Stop stops protocol, called when seeleService quits.
func (sp *SeeleProtocol) Stop() {
	event.BlockMinedEventManager.RemoveListener(sp.handleNewMinedBlock)
	event.TransactionInsertedEventManager.RemoveListener(sp.handleNewTx)
	close(sp.quitCh)
	close(sp.syncCh)
	sp.wg.Wait()
}

// syncer try to synchronise with remote peer
func (sp *SeeleProtocol) syncer() {
	defer sp.downloader.Terminate()
	defer sp.wg.Done()
	sp.wg.Add(1)

	forceSync := time.NewTicker(forceSyncInterval)
	for {
		select {
		case <-sp.syncCh:
			go sp.synchronise(sp.peerSet.bestPeer(common.LocalShardNumber))
		case <-forceSync.C:
			go sp.synchronise(sp.peerSet.bestPeer(common.LocalShardNumber))
		case <-sp.quitCh:
			return
		}
	}
}

func (sp *SeeleProtocol) synchronise(bestPeers []*bestPeerForEachChain) {
	if bestPeers == nil {
		return
	}

	if common.PrintExplosionLog {
		sp.log.Debug("sp.synchronise called.")
	}	

	for i, bp := range bestPeers{
		if bp == nil {continue}
		block := sp.chain[i].CurrentBlock()
		localTD, err := sp.chain[i].GetStore().GetBlockTotalDifficulty(block.HeaderHash)
		if err != nil {
			sp.log.Error("sp.synchronise GetBlockTotalDifficulty err.[%s]", err)
			return
		}
		pHead, pTd := bp.bestPeer.HeadByChain(bp.chainNum)
		sp.log.Debug("BestPeer Info, chainNum:%d pHead:%s", bp.chainNum, pHead.ToHex())

		// if total difficulty is not smaller than remote peer td, then do not need synchronise.
		if localTD.Cmp(pTd) >= 0 {
			continue
		}

		// miner stops only when the miner and the new received block are on the same chain 
		event.BlockDownloaderEventManager.Fire(event.DownloaderStartEvent)
		// defer func() {
		//	if err != nil {
		//		sp.log.Info("download end with failed, err %s, chainNum: %d", err, bp.chainNum)
		//		event.BlockDownloaderEventManager.Fire(event.DownloaderFailedEvent)
		//	} else {
		//		sp.log.Debug("download end success, chainNum: %d", bp.chainNum)
		//		event.BlockDownloaderEventManager.Fire(event.DownloaderDoneEvent)
		//	}
		// }()

		err = sp.downloader.Synchronise(bp.bestPeer.peerStrID, bp.chainNum, pHead, pTd, localTD)
		if err != nil {
			if err == downloader.ErrIsSynchronising {
				sp.log.Info("exit synchronise as it is already running.")
			} else {
				sp.log.Error("synchronise err. %s", err)
			}
	
			sp.log.Info("download end with failed, err %s, chainNum: %d", err, bp.chainNum)
			event.BlockDownloaderEventManager.Fire(event.DownloaderFailedEvent)	

			return
		}

		//broadcast chain head
		sp.broadcastChainHead(bp.chainNum)
	}

	sp.log.Debug("download end success")
	event.BlockDownloaderEventManager.Fire(event.DownloaderDoneEvent)

	return
}

func (sp *SeeleProtocol) broadcastChainHead(chainNum uint64) {
	block := sp.chain[chainNum].CurrentBlock()
	head := block.HeaderHash
	localTD, err := sp.chain[chainNum].GetStore().GetBlockTotalDifficulty(head)
	if err != nil {
		sp.log.Error("broadcastChainHead GetBlockTotalDifficulty err. %s", err)
		return
	}

	//sp.log.Info("broadcastChainHead, ChainNum: %d, head: %s", chainNum, head.ToHex())
	status := &chainHeadStatus{
		TD:           localTD,
		CurrentBlock: head,
		ChainNum:     chainNum,
	}
	sp.peerSet.ForEach(common.LocalShardNumber, func(peer *peer) bool {
		err := peer.sendHeadStatus(status)
		if err != nil {
			sp.log.Warn("failed to send chain head info %s", err)
		}
		return true
	})
}

// syncTransactions sends pending transactions to remote peer.
func (sp *SeeleProtocol) syncTransactions(p *peer) {
	defer sp.wg.Done()
	sp.wg.Add(1)

	var pending []*transactionMsg
	var txMsg 	transactionMsg
 	for i := 0; i < NumOfChains; i++ {
		pendingInOnePool := sp.txPool[i].GetTransactions(false, true)
		for _, tx := range pendingInOnePool {
			txMsg.Tx = tx
			txMsg.ChainNum = uint64(i)
			pending = append(pending, &txMsg)
		} 
	}

	sp.log.Debug("syncTransactions peerid:%s pending length:%d", p.peerStrID, len(pending))
	if len(pending) == 0 {
		return
	}
	var (
		resultCh = make(chan error, 1)
		curPos   = 0
	)

	send := func(pos int) {
		// sends txs from pos
		needSend := len(pending) - pos
		if needSend > txsyncPackSize {
			needSend = txsyncPackSize
		}

		if needSend == 0 {
			resultCh <- errSyncFinished
			return
		}
		curPos = curPos + needSend
		go func() { resultCh <- p.sendTransactions(pending[pos : pos+needSend]) }()
	}

	send(curPos)
	
loopOut:
	for {
		select {
		case err := <-resultCh:
			if err == errSyncFinished || err != nil {
				break loopOut
			}
			send(curPos)
		case <-sp.quitCh:
			break loopOut
		}
	}
	close(resultCh)
}

func (p *SeeleProtocol) handleNewTx(e event.Event) {
	if common.PrintExplosionLog {
		p.log.Debug("find new tx")
	}

	var NewTxHashMsg transactionHashMsg
	NewTxHashMsg.ChainNum = e.(event.HandleNewTxMsg).ChainNum
	NewTxHashMsg.TxHash = e.(event.HandleNewTxMsg).Tx.Hash
	
	tx := e.(event.HandleNewTxMsg).Tx
	// find shardId by tx from address.
	shardId := tx.Data.From.Shard()
	p.peerSet.ForEach(shardId, func(peer *peer) bool {
		if err := peer.sendTransactionHash(&NewTxHashMsg); err != nil {
			p.log.Warn("failed to send transaction to %s, %s", peer.Node.GetUDPAddr(), err)
		}
		return true
	})
}

func (p *SeeleProtocol) propagateDebt(debts []*types.Debt) {
	debtsMap := make([][]*types.Debt, common.ShardCount+1)

	for _, d := range debts {
		debtsMap[d.Data.Shard] = append(debtsMap[d.Data.Shard], d)
	}

	p.propagateDebtMap(debtsMap)
}

func (p *SeeleProtocol) propagateDebtMap(debtsMap [][]*types.Debt) {
	p.peerSet.ForEachAll(func(peer *peer) bool {
		if len(debtsMap[peer.Node.Shard]) > 0 {
			err := peer.sendDebts(debtsMap[peer.Node.Shard])
			if err != nil {
				p.log.Warn("failed to send debts to %s %s", peer.Node, err)
			}
		}

		return true
	})
}

func (p *SeeleProtocol) handleNewMinedBlock(e event.Event) {
	block := e.(event.HandleNewMinedBlockMsg).Block
	chainNum := e.(event.HandleNewMinedBlockMsg).ChainNum

	var blkHashMsg  blockHashMsg
	blkHashMsg.BlockHash = block.HeaderHash
	blkHashMsg.ChainNum  = chainNum

	p.peerSet.ForEach(common.LocalShardNumber, func(peer *peer) bool {
		err := peer.SendBlockHash(&blkHashMsg)
		if err != nil {
			p.log.Warn("failed to send mined block hash %s", err.Error())
		}
		return true
	})

	// propagate confirmed block
	if block.Header.Height > common.ConfirmedBlockNumber {
		confirmedHeight := block.Header.Height - common.ConfirmedBlockNumber
		confirmedBlock, err := p.chain[chainNum].GetStore().GetBlockByHeight(confirmedHeight)
		if err != nil {
			p.log.Warn("failed to load confirmed block height %d, err %s", confirmedHeight, err)
		}

		debts := types.NewDebtMap(confirmedBlock.Transactions)
		for _, d := range debts[common.LocalShardNumber] {
			debtChainNum := d.Data.ChainNum
			p.log.Debug("Debts from confirmed block, add to debtPool: %d", debtChainNum)
			p.debtPool[debtChainNum].Add(d)
		}
		p.propagateDebtMap(debts)
	}

	p.log.Info("handleNewMinedBlock broadcast chainhead changed. chainNum: %d, new block: %d %s <- %s ",
		chainNum, block.Header.Height, block.HeaderHash.ToHex(), block.Header.PreviousBlockHash.ToHex())

	p.broadcastChainHead(chainNum)
}

func (p *SeeleProtocol) handleAddPeer(p2pPeer *p2p.Peer, rw p2p.MsgReadWriter) {
	if p.peerSet.Find(p2pPeer.Node.ID) != nil {
		p.log.Error("handleAddPeer called, but peer of this public-key has already existed, so need quit!")
		return
	}

	newPeer := newPeer(SeeleVersion, p2pPeer, rw, p.log)

	block := make([]*types.Block,NumOfChains)
 	head := make([]common.Hash,NumOfChains)
	localTD := make([]*big.Int,NumOfChains)
	var err error 
 	for i := 0; i < NumOfChains; i++ {
 		block[i] = p.chain[i].CurrentBlock()
 		head[i] = block[i].HeaderHash
 		localTD[i], err = p.chain[i].GetStore().GetBlockTotalDifficulty(head[i])
 		if err != nil {
 			return
 		}
 	}

	genesisBlock, err := p.chain[0].GetStore().GetBlockByHeight(0)
	if err != nil {
		return
	}

	if err := newPeer.handShake(p.networkID, localTD, head, genesisBlock.HeaderHash, genesisBlock.Header.Difficulty.Uint64()); err != nil {
		p.log.Error("handleAddPeer err. %s", err)
		newPeer.Disconnect(DiscHandShakeErr)
		return
	}

	p.log.Info("add peer %s -> %s to SeeleProtocol.", p2pPeer.LocalAddr(), p2pPeer.RemoteAddr())
	p.peerSet.Add(newPeer)
	p.downloader.RegisterPeer(newPeer.peerStrID, newPeer)
	go p.syncTransactions(newPeer)
	go p.handleMsg(newPeer)
}

func (s *SeeleProtocol) handleGetPeer(address common.Address) interface{} {
	if p := s.peerSet.peerMap[address]; p != nil {
		return p.Info()
	}
	return nil
}

func (s *SeeleProtocol) handleDelPeer(peer *p2p.Peer) {
	s.log.Debug("delete peer from peer set. %s", peer.Node)
	s.peerSet.Remove(peer.Node.ID)
	s.downloader.UnRegisterPeer(idToStr(peer.Node.ID))
}

func (p *SeeleProtocol) SendDifferentShardTx(txMsg *transactionMsg, shard uint) {
	tx := txMsg.Tx
	chainNum := txMsg.ChainNum
	sendTxFun := func(peer *peer) bool {
		if !peer.knownTxs.Contains(tx.Hash) {
			err := peer.sendTransaction(tx, chainNum)
			if err != nil {
				p.log.Warn("failed to send transaction to peer %s, tx hash %s", peer.Node, tx.Hash)
				return true
			}

			peer.knownTxs.Add(tx.Hash, nil)
		}

		return true
	}

	if p.peerSet.getPeerCountByShard(shard) > 0 {
		p.peerSet.ForEach(shard, sendTxFun)
	} else {
		p.peerSet.ForEachAll(sendTxFun)
	}
}

func (p *SeeleProtocol) handleMsg(peer *peer) {
handler:
	for {
		msg, err := peer.rw.ReadMsg()
		if err != nil {
			p.log.Error("get error when read msg from %s, %s", peer.peerStrID, err)
			break
		}

		// skip unsupported message from different shard peer
		if peer.Node.Shard != common.LocalShardNumber {
			if msg.Code != transactionsMsgCode && msg.Code != debtMsgCode {
				continue
			}
		}

		if common.PrintExplosionLog {
			p.log.Debug("got msg with type:%s", codeToStr(msg.Code))
		}

		switch msg.Code {
		case transactionHashMsgCode:
			var txHashMsg transactionHashMsg
			err := common.Deserialize(msg.Payload, &txHashMsg)
			if err != nil {
				p.log.Warn("failed to deserialize transaction hash msg, %s", err.Error())
				continue
			}

			txHash := txHashMsg.TxHash
			if common.PrintExplosionLog {
				p.log.Debug("got tx hash %s", txHash.ToHex())
			}

			if !peer.knownTxs.Contains(txHash) {
				peer.knownTxs.Add(txHash, nil) //update peer known transaction
				err := peer.sendTransactionRequest(&txHashMsg)
				if err != nil {
					p.log.Warn("failed to send transaction request msg, %s", err.Error())
					break handler
				}
			} else {
				if common.PrintExplosionLog {
					p.log.Debug("already have this tx %s", txHash.ToHex())
				}
			}

		case transactionRequestMsgCode:
			var txHashMsg transactionHashMsg
			err := common.Deserialize(msg.Payload, &txHashMsg)
			if err != nil {
				p.log.Warn("failed to deserialize transaction request msg %s", err.Error())
				continue
			}

			txHash := txHashMsg.TxHash
			chainNum := txHashMsg.ChainNum

			if common.PrintExplosionLog {
				p.log.Debug("got tx request %s", txHash.ToHex())
			}

			tx := p.txPool[chainNum].GetTransaction(txHash)
			if tx == nil {
				p.log.Debug("[transactionRequestMsgCode] not found tx in tx pool %s", txHash.ToHex())
				continue
			}

			err = peer.sendTransaction(tx, chainNum)
			if err != nil {
				p.log.Warn("failed to send transaction msg %s", err.Error())
				break handler
			}

		case transactionsMsgCode:
			var txMsgs []*transactionMsg
			err := common.Deserialize(msg.Payload, &txMsgs)
			if err != nil {
				p.log.Warn("failed to deserialize transaction msg %s", err.Error())
				break
			}

			if common.PrintExplosionLog {
				p.log.Debug("received %d transactions", len(txMsgs))
			}

			for _, txMsg := range txMsgs {
				tx := txMsg.Tx
				chainNum := txMsg.ChainNum
				peer.knownTxs.Add(tx.Hash, nil)
				shard := tx.Data.From.Shard()
				if shard != common.LocalShardNumber {
					go p.SendDifferentShardTx(txMsg, shard)
					continue
				} else {
					p.txPool[chainNum].AddTransaction(tx)
				}
			}

		case blockHashMsgCode:
			var blkHashMsg blockHashMsg
			err := common.Deserialize(msg.Payload, &blkHashMsg)
			if err != nil {
				p.log.Warn("failed to deserialize block hash msg %s", err.Error())
				continue
			}

			blockHash := blkHashMsg.BlockHash

			p.log.Debug("got block hash msg %s", blockHash.ToHex())

			if !peer.knownBlocks.Contains(blockHash) {
				peer.knownBlocks.Add(blockHash, nil)
				err := peer.SendBlockRequest(&blkHashMsg)
				if err != nil {
					p.log.Warn("failed to send block request msg %s", err.Error())
					break handler
				}
			}

		case blockRequestMsgCode:
			var blkHashMsg blockHashMsg
			err := common.Deserialize(msg.Payload, &blkHashMsg)
			if err != nil {
				p.log.Warn("failed to deserialize block request msg %s", err.Error())
				continue
			}

			blockHash := blkHashMsg.BlockHash
			chainNum := blkHashMsg.ChainNum

			p.log.Debug("got block request msg %s", blockHash.ToHex())
			block, err := p.chain[chainNum].GetStore().GetBlock(blockHash)
			if err != nil {
				p.log.Warn("not found request block %s", err.Error())
				continue
			}

			var blkMsg blockMsg
			blkMsg.Block = block
			blkMsg.ChainNum = chainNum

			err = peer.SendBlock(&blkMsg)
			if err != nil {
				p.log.Warn("failed to send block msg %s", err.Error())
			}

		case blockMsgCode:
			var blkMsg blockMsg
			err := common.Deserialize(msg.Payload, &blkMsg)
			if err != nil {
				p.log.Warn("failed to deserialize block msg %s", err.Error())
				continue
			}

			block := blkMsg.Block
			chainNum := blkMsg.ChainNum

			p.log.Info("got block message and save it. height:%d, hash:%s", block.Header.Height, block.HeaderHash.ToHex())
			peer.knownBlocks.Add(block.HeaderHash, nil)
			if block.GetShardNumber() == common.LocalShardNumber {
				// @todo need to make sure WriteBlock handle block fork
				p.chain[chainNum].WriteBlock(block)
			}

		case debtMsgCode:
			var debts []*types.Debt
			err := common.Deserialize(msg.Payload, &debts)
			if err != nil {
				p.log.Warn("failed to deserialize debts msg %s", err)
				continue
			}

			p.log.Info("got %d debts message [%s]", len(debts), codeToStr(msg.Code))
			for _, d := range debts {
				peer.knownDebts.Add(d.Hash, nil)
				chainNum := d.Data.ChainNum
				p.log.Debug("received debts message, add to debtPool: %d", chainNum)
				p.debtPool[chainNum].Add(d)
			}
			
			go p.propagateDebt(debts)

		case downloader.GetBlockHeadersMsg:
			var query blockHeadersQuery
			err := common.Deserialize(msg.Payload, &query)
			if err != nil {
				p.log.Error("failed to deserialize downloader.GetBlockHeadersMsg, quit! %s", err.Error())
				break
			}
			var headList []*types.BlockHeader
			var head *types.BlockHeader
			orgNum := query.Number
			chainNum := query.ChainNum

			if query.Hash != common.EmptyHash {
				p.log.Info("chainNum: %d, Query Hash: %s", chainNum, query.Hash.ToHex())
				if head, err = p.chain[chainNum].GetStore().GetBlockHeader(query.Hash); err != nil {
					p.log.Error("HandleMsg GetBlockHeader err from query hash. %s", err)
					break
				}
				orgNum = head.Height
			}

			p.log.Debug("Received downloader.GetBlockHeadersMsg chainNum %d, start %d, amount %d", chainNum, orgNum, query.Amount)
			maxHeight := p.chain[chainNum].CurrentBlock().Header.Height
			for cnt := uint64(0); cnt < query.Amount; cnt++ {
				var curNum uint64
				if query.Reverse {
					curNum = orgNum - cnt
				} else {
					curNum = orgNum + cnt
				}

				if curNum > maxHeight {
					break
				}
				hash, err := p.chain[chainNum].GetStore().GetBlockHash(curNum)
				if err != nil {
					p.log.Error("get error when get block hash by height. err=%s curNum=%d", err, curNum)
					break
				}

				if head, err = p.chain[chainNum].GetStore().GetBlockHeader(hash); err != nil {
					p.log.Error("get error when get block by block hash. err: %s, hash:%s", err, hash)
					break
				}
				headList = append(headList, head)
			}

			if err = peer.sendBlockHeaders(query.Magic, headList, chainNum); err != nil {
				p.log.Error("HandleMsg sendBlockHeaders err. %s", err)
				break handler
			}
			p.log.Debug("send downloader.sendBlockHeaders. len=%d", len(headList))

		case downloader.GetBlocksMsg:
			p.log.Debug("Received downloader.GetBlocksMsg")
			var query blocksQuery
			err := common.Deserialize(msg.Payload, &query)
			if err != nil {
				p.log.Error("failed to deserialize downloader.GetBlocksMsg, quit! %s", err.Error())
				break
			}

			var blocksL []*types.Block
			var head *types.BlockHeader
			var block *types.Block
			orgNum := query.Number
			chainNum := query.ChainNum
			if query.Hash != common.EmptyHash {
				if head, err = p.chain[chainNum].GetStore().GetBlockHeader(query.Hash); err != nil {
					p.log.Error("HandleMsg GetBlockHeader err. %s", err)
					break
				}
				orgNum = head.Height
			}

			p.log.Debug("Received downloader.GetBlocksMsg length %d, start %d, end %d, chainNum: %d", query.Amount, orgNum, orgNum+query.Amount, chainNum)

			totalLen := 0
			var numL []uint64
			for cnt := uint64(0); cnt < query.Amount; cnt++ {
				curNum := orgNum + cnt
				hash, err := p.chain[chainNum].GetStore().GetBlockHash(curNum)
				if err != nil {
					p.log.Warn("failed to get block with height %d, err %s", curNum, err)
					break
				}

				if block, err = p.chain[chainNum].GetStore().GetBlock(hash); err != nil {
					p.log.Error("HandleMsg GetBlocksMsg p.chain.GetStore().GetBlock err. %s", err)
					break handler
				}

				curLen := len(common.SerializePanic(block))
				if totalLen > 0 && (totalLen+curLen) > downloader.MaxMessageLength {
					break
				}
				totalLen += curLen
				blocksL = append(blocksL, block)
				numL = append(numL, curNum)
			}

			if len(blocksL) == 0 {
				p.log.Debug("send blocks with empty")
			} else {
				p.log.Debug("send blocks length %d, start %d, end %d", len(blocksL), blocksL[0].Header.Height, blocksL[len(blocksL)-1].Header.Height)
			}

			if err = peer.sendBlocks(query.Magic, blocksL, chainNum); err != nil {
				p.log.Error("HandleMsg GetBlocksMsg sendBlocks err. %s", err)
				break handler
			}

			p.log.Debug("send downloader.sendBlocks")

		case downloader.BlockHeadersMsg, downloader.BlocksPreMsg, downloader.BlocksMsg:
			p.log.Debug("Received downloader Msg. %s peerid:%s", codeToStr(msg.Code), peer.peerStrID)
			p.downloader.DeliverMsg(peer.peerStrID, msg)

		case statusChainHeadMsgCode:
			var status chainHeadStatus
			err := common.Deserialize(msg.Payload, &status)
			if err != nil {
				p.log.Error("failed to deserialize statusChainHeadMsgCode, quit! %s", err.Error())
				break
			}

			p.log.Debug("Received statusChainHeadMsgCode")
			//p.log.Info("Received peer status, ChainNum: %d, peer head: %s", status.ChainNum, status.CurrentBlock.ToHex())
			peer.SetHead(status.CurrentBlock, status.TD, status.ChainNum)
			p.syncCh <- struct{}{}

		default:
			p.log.Warn("unknown code %s", msg.Code)
		}
	}

	p.handleDelPeer(peer.Peer)
	p.log.Debug("seele.peer.run out!peer=%s!", peer.peerStrID)
}
