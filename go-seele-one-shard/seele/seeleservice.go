/**
*  @file
*  @copyright defined in go-seele/LICENSE
 */

package seele

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/seeleteam/go-seele/common"
	"github.com/seeleteam/go-seele/core"
	"github.com/seeleteam/go-seele/core/store"
	"github.com/seeleteam/go-seele/core/state"
	"github.com/seeleteam/go-seele/database"
	"github.com/seeleteam/go-seele/database/leveldb"
	"github.com/seeleteam/go-seele/event"
	"github.com/seeleteam/go-seele/log"
	"github.com/seeleteam/go-seele/miner"
	"github.com/seeleteam/go-seele/node"
	"github.com/seeleteam/go-seele/p2p"
	rpc "github.com/seeleteam/go-seele/rpc2"
	"github.com/seeleteam/go-seele/seele/download"
)

const chainHeaderChangeBuffSize = 100

// SeeleService implements full node service.
type SeeleService struct {
	networkID     uint64
	p2pServer     *p2p.Server
	seeleProtocol *SeeleProtocol
	log           *log.SeeleLog

	txPools         [NumOfChains]*core.TransactionPool
	debtPools       [NumOfChains]*core.DebtPool
	chains          [NumOfChains]*core.Blockchain
	chainDBs        [NumOfChains]database.Database // database used to store blocks.
	accountStateDB database.Database // database used to store account state info.
	accountStateDBRootHash common.Hash
	miner          *miner.Miner

	lastHeaders              [NumOfChains]common.Hash
	chainHeaderChangeChannels [NumOfChains]chan common.Hash

	lock           sync.RWMutex // lock for update accountstateDB
}

// ServiceContext is a collection of service configuration inherited from node
type ServiceContext struct {
	DataDir string
}

func (s *SeeleService) TxPool() [NumOfChains]*core.TransactionPool { return s.txPools }
func (s *SeeleService) DebtPool() [NumOfChains]*core.DebtPool      { return s.debtPools }
func (s *SeeleService) BlockChain() [NumOfChains]*core.Blockchain  { return s.chains }
func (s *SeeleService) NetVersion() uint64            { return s.networkID }
func (s *SeeleService) Miner() *miner.Miner           { return s.miner }
func (s *SeeleService) Downloader() *downloader.Downloader {
	return s.seeleProtocol.Downloader()
}
func (s *SeeleService) AccountStateDB() database.Database { return s.accountStateDB }
// GetCurrentState returns the current state of the accounts
func (s *SeeleService) GetCurrentState() (*state.Statedb, error) {
	return state.NewStatedb(s.accountStateDBRootHash, s.accountStateDB)
}

func (s *SeeleService) UpdateDB(db database.Database) error {
	s.accountStateDB = db
	return nil
}

func (s *SeeleService) UpdateDBRootHash(dbRootHash common.Hash) error {
	s.accountStateDBRootHash = dbRootHash
	return nil
}

func (s *SeeleService) Lock() error {
	s.lock.Lock()
	return nil
}

func (s *SeeleService) Unlock() error {
	s.lock.Unlock()
	return nil
}

// NewSeeleService create SeeleService
func NewSeeleService(ctx context.Context, conf *node.Config, log *log.SeeleLog) (s *SeeleService, err error) {
	s = &SeeleService{
		log:       log,
		networkID: conf.P2PConfig.NetworkID,
	}

	serviceContext := ctx.Value("ServiceContext").(ServiceContext)

	// Initialize blockchain DB.
	for i := 0; i < NumOfChains; i++ {
		chainNumString := strconv.Itoa(i)
		chainDBPath := filepath.Join(serviceContext.DataDir, BlockChainDir, chainNumString)
		log.Info("NewSeeleService BlockChain datadir is %s", chainDBPath)	
		s.chainDBs[i],err = leveldb.NewLevelDB(chainDBPath)
		if err != nil {
			log.Error("NewSeeleService Create BlockChain err. %s", err)
			return nil, err
		}
		leveldb.StartMetrics(s.chainDBs[i], "chaindb"+chainNumString, log)
	}
	
	// Initialize account state info DB.
	accountStateDBPath := filepath.Join(serviceContext.DataDir, AccountStateDir)
	log.Info("NewSeeleService account state datadir is %s", accountStateDBPath)
	s.accountStateDB, err = leveldb.NewLevelDB(accountStateDBPath)
	if err != nil {
		for i := 0; i < NumOfChains; i++ {
			s.chainDBs[i].Close()
		}
		log.Error("NewSeeleService Create BlockChain err: failed to create account state DB, %s", err)
		return nil, err
	}

	// initialize accountStateDB with genesis info
	genesis := core.GetGenesis(conf.SeeleConfig.GenesisConfig)
	statedb, err := core.GetStateDB(genesis.Info)
	if err != nil {
		return nil, err
	}

	s.accountStateDBRootHash, err = statedb.Hash()
	if err != nil {
		return nil, err
	}

	batch := s.accountStateDB.NewBatch()
	statedb.Commit(batch)
	if err = batch.Commit(); err != nil {
		return nil, err
	}

	// initialize and validate genesis
	for i := 0; i < NumOfChains; i++ {
		bcStore := store.NewCachedStore(store.NewBlockchainDatabase(s.chainDBs[i]))
		err = genesis.InitializeAndValidate(bcStore)
		if err != nil {
			for i := 0; i < NumOfChains; i++ {
				s.chainDBs[i].Close()
			}
			s.accountStateDB.Close()
			log.Error("NewSeeleService genesis.Initialize err. %s", err)
			return nil, err
		}
	
		chainNumString := strconv.Itoa(i)
		recoveryPointFile := filepath.Join(serviceContext.DataDir, chainNumString, BlockChainRecoveryPointFile)
		s.chains[i], err = core.NewBlockchain(bcStore, recoveryPointFile, uint64(i), s)
		if err != nil {
			for i := 0; i < NumOfChains; i++ {
				s.chainDBs[i].Close()
			}
			s.accountStateDB.Close()
			log.Error("failed to init chain in NewSeeleService. %s", err)
			return nil, err
		}
	}

	err = s.initPool(conf)
	if err != nil {
		for i := 0; i < NumOfChains; i++ {
			s.chainDBs[i].Close()
		}
		s.accountStateDB.Close()
		log.Error("failed to create transaction pool in NewSeeleService, %s", err)
		return nil, err
	}
	

	s.seeleProtocol, err = NewSeeleProtocol(s, log)
	if err != nil {
		for i := 0; i < NumOfChains; i++ {
			s.chainDBs[i].Close()
		}
		s.accountStateDB.Close()
		log.Error("failed to create seeleProtocol in NewSeeleService, %s", err)
		return nil, err
	}

	s.miner = miner.NewMiner(conf.SeeleConfig.Coinbase, s)

	return s, nil
}

func (s *SeeleService) initPool(conf *node.Config) error {
	var err error
	for i := 0; i < NumOfChains; i++ {
		s.lastHeaders[i], err = s.chains[i].GetStore().GetHeadBlockHash()
		if err != nil {
			return fmt.Errorf("failed to get chain header, %s", err)
		}

		s.chainHeaderChangeChannels[i] = make(chan common.Hash, chainHeaderChangeBuffSize)
		s.debtPools[i] = core.NewDebtPool(s.chains[i], s)
		s.txPools[i] = core.NewTransactionPool(conf.SeeleConfig.TxConf, s.chains[i], uint64(i), s)

		event.ChainHeaderChangedEventMananger.AddAsyncListener(s.chainHeaderChanged)
		go s.MonitorChainHeaderChange(uint64(i))

	}
	return nil
}

// chainHeaderChanged handle chain header changed event.
// add forked transaction back
// deleted invalid transaction
func (s *SeeleService) chainHeaderChanged(e event.Event) {
	newHeader := e.(event.ChainHeaderChangedMsg).HeaderHash 
	if newHeader.IsEmpty() {
		return
	}
	chainNum := e.(event.ChainHeaderChangedMsg).ChainNum
	s.chainHeaderChangeChannels[chainNum] <- newHeader
}

// MonitorChainHeaderChange monitor and handle chain header event
func (s *SeeleService) MonitorChainHeaderChange(chainNum uint64) {
	for {
		select {
		case newHeader := <-s.chainHeaderChangeChannels[chainNum]:
			if s.lastHeaders[chainNum].IsEmpty() {
				s.lastHeaders[chainNum] = newHeader
				return
			}

			s.txPools[chainNum].HandleChainHeaderChanged(newHeader, s.lastHeaders[chainNum])
			s.debtPools[chainNum].HandleChainHeaderChanged(newHeader, s.lastHeaders[chainNum])

			s.lastHeaders[chainNum] = newHeader
		}
	}
}

// Protocols implements node.Service, returning all the currently configured
// network protocols to start.
func (s *SeeleService) Protocols() (protos []p2p.Protocol) {
	protos = append(protos, s.seeleProtocol.Protocol)
	return
}

// Start implements node.Service, starting goroutines needed by SeeleService.
func (s *SeeleService) Start(srvr *p2p.Server) error {
	s.p2pServer = srvr

	s.seeleProtocol.Start()
	return nil
}

// Stop implements node.Service, terminating all internal goroutines.
func (s *SeeleService) Stop() error {
	s.seeleProtocol.Stop()

	//TODO
	// s.txPool.Stop() s.chain.Stop()
	// retries? leave it to future
	for i := 0; i < NumOfChains; i++ {
		s.chainDBs[i].Close()
	}
	s.accountStateDB.Close()
	return nil
}

// APIs implements node.Service, returning the collection of RPC services the seele package offers.
 func (s *SeeleService) APIs() (apis []rpc.API) {
 	return append(apis, []rpc.API{
 		{
 			Namespace: "seele",
 			Version:   "1.0",
 			Service:   NewPublicSeeleAPI(s),
 			Public:    true,
 		},
 		{
 			Namespace: "txpool",
 			Version:   "1.0",
 			Service:   NewTransactionPoolAPI(s),
 			Public:    true,
 		},
 		{
 			Namespace: "download",
 			Version:   "1.0",
 			Service:   downloader.NewPrivatedownloaderAPI(s.seeleProtocol.downloader),
 			Public:    false,
 		},
 		{
 			Namespace: "network",
 			Version:   "1.0",
 			Service:   NewPrivateNetworkAPI(s),
 			Public:    false,
 		},
 		{
 			Namespace: "debug",
 			Version:   "1.0",
 			Service:   NewPrivateDebugAPI(s),
 			Public:    false,
 		},
 		{
 			Namespace: "miner",
 			Version:   "1.0",
 			Service:   NewPrivateMinerAPI(s),
 			Public:    false,
 		},
 	}...)
 }
