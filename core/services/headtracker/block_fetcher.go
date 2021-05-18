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

func NewBlockFetcher(ethClient eth.Client, config BlockFetcherConfig, logger *logger.Logger) *BlockFetcher {

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
		logger:              logger,
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

func (bf *BlockFetcher) Chain(ctx context.Context, latestHead models.Head) (models.Head, error) {
	bf.logger.Debugw("BlockFetcher#Chain for head", "number", latestHead.Number, "hash", latestHead.Hash)

	from := latestHead.Number - int64(bf.config.EthFinalityDepth()-1)
	if from < 0 {
		from = 0
	}

	// typically all the heads should be constructed into a chain without issues, unless there was a very recent reorgs
	// and new blocks were not downloaded yet
	headWithChain, err := bf.downloader.syncLatestHead(ctx, latestHead)
	if err != nil {
		return models.Head{}, errors.Wrapf(err, "BlockFetcher#Chain error for syncLatestHead: %v", latestHead.Number)
	}
	return *headWithChain, nil
}

func (bf *BlockFetcher) SyncLatestHead(ctx context.Context, head models.Head) error {
	_, err := bf.downloader.syncLatestHead(ctx, head)
	return err
}
