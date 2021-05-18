package headtracker

import (
	"context"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	"github.com/smartcontractkit/chainlink/core/store/models"
	"github.com/smartcontractkit/chainlink/core/utils"
)

// BlockFetcherConfig defines the interface for the supplied config
type BlockFetcherConfig interface {
	BlockFetcherHistorySize() uint16
	BlockFetcherBatchSize() uint32

	EthFinalityDepth() uint
	EthHeadTrackerHistoryDepth() uint
	BlockBackfillDepth() uint64
	GasUpdaterBlockHistorySize() uint16
	GasUpdaterBlockDelay() uint16
	GasUpdaterBatchSize() uint32
}

type BlockFetcherInterface interface {
	FetchLatestHead(ctx context.Context) (*models.Head, error)
	BlockRange(ctx context.Context, head models.Head, fromBlock int64, toBlock int64) ([]Block, error)
}

type BlockFetcher struct {
	ethClient           eth.Client
	downloader          *BlockDownloader
	logger              *logger.Logger
	addresses           map[common.Address]struct{}
	config              BlockFetcherConfig
	rollingBlockHistory map[int64]Block
	latestBlockNum      int64
	mut                 sync.Mutex
}

type BlockDownload struct {
	StartedAt time.Time

	Number int64

	// may be nil
	Hash *common.Hash

	// may be nil
	Head *models.Head
}

func (bf *BlockFetcher) BlockCache() []*Block {
	return bf.downloader.RecentSorted()
}

func NewBlockFetcher(ethClient eth.Client, config BlockFetcherConfig) *BlockFetcher {

	if config.GasUpdaterBlockHistorySize()+config.GasUpdaterBlockDelay() > config.BlockFetcherHistorySize() {
		panic("") //TODO:
	}

	if config.EthHeadTrackerHistoryDepth() > uint(config.BlockFetcherHistorySize()) {
		panic("") //TODO:
	}

	if config.BlockBackfillDepth() > uint64(config.BlockFetcherHistorySize()) {
		panic("") //TODO:
	}

	return &BlockFetcher{
		ethClient:           ethClient,
		downloader:          NewBlockDownloader(ethClient, config.BlockFetcherBatchSize(), config.BlockFetcherHistorySize()),
		logger:              logger.Default,
		config:              config,
		addresses:           make(map[common.Address]struct{}),
		rollingBlockHistory: make(map[int64]Block, 0),
	}
}

func (bf *BlockFetcher) Backfill(chStop chan struct{}) {

	ctx, _ := utils.ContextFromChan(chStop)

	latestHead, err := bf.FetchLatestHead(ctx)
	if err != nil {
		bf.logger.Errorw("BlockFetcher#FetchLatestHead error", "err", err)
		//return nil, err
	}

	from := latestHead.Number - int64(bf.config.BlockFetcherHistorySize()-1)
	if from < 0 {
		from = 0
	}

	bf.downloader.StartDownloadAsync(ctx, from, latestHead.Number)

	//defer wgDone.Done()
	//for {
	//	select {
	//	case <-chStop:
	//		return
	//	case <-ht.backfillMB.Notify():
	//		for {
	//			head, exists := ht.backfillMB.Retrieve()
	//			if !exists {
	//				break
	//			}
	//			h, is := head.(models.Head)
	//			if !is {
	//				panic(fmt.Sprintf("expected `models.Head`, got %T", head))
	//			}
	//			{
	//				ctx, cancel := utils.ContextFromChan(ht.chStop)
	//				err := ht.Backfill(ctx, h, ht.store.Config.EthFinalityDepth())
	//				defer cancel()
	//				if err != nil {
	//					ht.logger().Warnw("HeadTracker: unexpected error while backfilling heads", "err", err)
	//				} else if ctx.Err() != nil {
	//					break
	//				}
	//			}
	//		}
	//	}
	//}
}

// FetchLatestHead - Fetches the latest head from the blockchain, regardless of local cache
func (bf *BlockFetcher) FetchLatestHead(ctx context.Context) (*models.Head, error) {
	return bf.ethClient.HeaderByNumber(ctx, nil)
}

// BlockRange - Returns a range of blocks, either from local memory or fetched
func (bf *BlockFetcher) BlockRange(ctx context.Context, head models.Head, fromBlock int64, toBlock int64) ([]Block, error) {
	bf.logger.Debugw("BlockFetcher#BlockRange requested range", "fromBlock", fromBlock, "toBlock", toBlock)

	blocks, err := bf.downloader.GetBlockRange(ctx, head, fromBlock, toBlock)
	if err != nil {
		return nil, errors.Wrapf(err, "BlockFetcher#GetBlockRange error for %v -> %v",
			fromBlock, toBlock)
	}
	blocksSlice := make([]Block, 0)
	for _, block := range blocks {
		blocksSlice = append(blocksSlice, *block)
	}

	return blocksSlice, nil
}

func (bf *BlockFetcher) Chain(ctx context.Context, latestHead models.Head) (*models.Head, error) {
	bf.logger.Debugw("BlockFetcher#Chain for head", "number", latestHead.Number, "hash", latestHead.Hash)

	from := latestHead.Number - int64(bf.config.EthFinalityDepth()-1)
	if from < 0 {
		from = 0
	}

	//for i := head.Number - 1; i >= baseHeight; i-- {
	//	// NOTE: Sequential requests here mean it's a potential performance bottleneck, be aware!
	//	var existingHead *models.Head
	//	existingHead, err = ht.store.HeadByHash(ctx, head.ParentHash)
	//	if ctx.Err() != nil {
	//		break
	//	} else if err != nil {
	//		return errors.Wrap(err, "HeadByHash failed")
	//	}
	//	if existingHead != nil {
	//		head = *existingHead
	//		continue
	//	}
	//	head, err = ht.fetchAndSaveHead(ctx, i)
	//	fetched++
	//	if ctx.Err() != nil {
	//		break
	//	} else if err != nil {
	//		return errors.Wrap(err, "fetchAndSaveHead failed")
	//	}
	//}
	//

	blocks, err := bf.downloader.GetBlockRange(ctx, latestHead, from, latestHead.Number)

	headWithChain := bf.constructChain(latestHead, blocks)

	if err != nil {
		return nil, errors.Wrapf(err, "BlockFetcher#Chain error for GetBlockRange: %v", latestHead.Number)
	}

	return headWithChain, nil
}

// typically all the heads should be constructed into a chain without issues, unless there was a very recent reorgs
// and new blocks were not downloaded yet

// typically all the heads should be constructed into a chain without issues, unless there was a very recent reorgs
// and new blocks were not downloaded yet
func (bf *BlockFetcher) constructChain(head models.Head, blocks []*Block) *models.Head {
	blocksByHash := make(map[common.Hash]*Block)
	for _, b := range blocks {
		block := b
		blocksByHash[block.Hash] = block
	}

	//current := head
	return nil
	//for {
	//	parentHead, ok := blocksByHash[current.ParentHash]
	//	if ok {
	//		current.Parent = parentHead.Number
	//	}
	//}

	//head.Parent = head.

}
