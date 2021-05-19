package headtracker

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
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
	ethClient eth.Client
	logger    *logger.Logger
	config    BlockFetcherConfig

	inProgress     []BlockDownload
	recent         map[common.Hash]*Block
	latestBlockNum int64

	notifications chan struct{}

	mut        sync.Mutex
	syncingMut sync.Mutex
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
	return bf.RecentSorted()
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
		ethClient: ethClient,
		logger:    logger,
		config:    config,
		recent:    make(map[common.Hash]*Block, 0),
	}
}

func (bf *BlockFetcher) Backfill(chStop chan struct{}) {
	ctx, _ := utils.ContextFromChan(chStop)

	latestHead, err := bf.FetchLatestHead(ctx)
	if err != nil {
		bf.logger.Errorw("BlockFetcher#FetchLatestHead returned an error, backfill will be skipped", "err", err)
	}

	from := latestHead.Number - int64(bf.config.BlockFetcherHistorySize()-1)
	if from < 0 {
		from = 0
	}

	bf.StartDownloadAsync(ctx, from, latestHead.Number)
}

// FetchLatestHead - Fetches the latest head from the blockchain, regardless of local cache
func (bf *BlockFetcher) FetchLatestHead(ctx context.Context) (*models.Head, error) {
	return bf.ethClient.HeaderByNumber(ctx, nil)
}

// BlockRange - Returns a range of blocks, either from local memory or fetched
func (bf *BlockFetcher) BlockRange(ctx context.Context, head models.Head, fromBlock int64, toBlock int64) ([]Block, error) {
	bf.logger.Debugw("BlockFetcher#BlockRange requested range", "fromBlock", fromBlock, "toBlock", toBlock)

	blocks, err := bf.GetBlockRange(ctx, head, fromBlock, toBlock)
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

	// typically all the heads should be constructed into a chain from memory, unless there was a very recent reorg
	// and new blocks were not downloaded yet
	headWithChain, err := bf.syncLatestHead(ctx, latestHead)
	if err != nil {
		return models.Head{}, errors.Wrapf(err, "BlockFetcher#Chain error for syncLatestHead: %v", latestHead.Number)
	}
	bf.logger.Debug("Returned from Chain")
	return headWithChain, nil
}

func (bf *BlockFetcher) SyncLatestHead(ctx context.Context, head models.Head) error {
	_, err := bf.syncLatestHead(ctx, head)
	bf.logger.Debug("Returned from SyncLatestHead")
	return err
}

func (bf *BlockFetcher) StartDownloadAsync(ctx context.Context, fromBlock int64, toBlock int64) {
	timeout := 2 * time.Minute

	go func() {
		utils.RetryWithBackoff(ctx, func() (retry bool) {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			err := bf.downloadRange(ctx, fromBlock, toBlock)
			if err != nil {
				bf.logger.Errorw("BlockFetcher#StartDownload error while downloading blocks. Will retry",
					"err", err, "fromBlock", fromBlock, "toBlock", toBlock)
				return true
			}
			return false
		})
		bf.logger.Debug("Returned from StartDownloadAsync")
	}()
}

func (bf *BlockFetcher) RecentSorted() []*Block {

	toReturn := make([]*Block, 0)

	for _, b := range bf.recent {
		block := b
		toReturn = append(toReturn, block)
	}

	sort.Slice(toReturn, func(i, j int) bool {
		return toReturn[i].Number < toReturn[j].Number
	})
	return toReturn
}
func (bf *BlockFetcher) GetBlockRange(ctx context.Context, head models.Head, fromBlock int64, toBlock int64) ([]*Block, error) {
	bf.logger.Debugw("BlockFetcher#BlockRange requested range", "fromBlock", fromBlock, "toBlock", toBlock)

	if fromBlock < 0 || toBlock < fromBlock {
		return make([]*Block, 0), errors.Errorf("Invalid range: %d -> %d", fromBlock, toBlock)
	}

	blocks := make(map[int64]*Block, 0)

	err := bf.downloadRange(ctx, fromBlock, toBlock)
	if err != nil {
		return make([]*Block, 0), errors.Wrapf(err, "BlockFetcher#GetBlockRange error while downloading blocks %v -> %v",
			fromBlock, toBlock)
	}
	bf.mut.Lock()

	for _, b := range bf.recent {
		block := b
		if block.Number >= fromBlock && block.Number <= toBlock {
			blocks[block.Number] = block
		}
	}
	bf.mut.Unlock()

	blockRange := make([]*Block, 0)
	for _, b := range blocks {
		block := b
		blockRange = append(blockRange, block)
	}

	return blockRange, nil
	// check if currently being downloaded
	// trigger download of missing ones
	//for {
	//	select {
	//	case _ = <-bd.notifications:
	//		// case after?
	//	default:
	//
	//	}
	//}
}

func (bf *BlockFetcher) downloadRange(ctx context.Context, fromBlock int64, toBlock int64) error {

	bf.mut.Lock()

	existingBlocks := make(map[int64]*Block)

	for _, b := range bf.recent {
		block := b
		if block.Number >= fromBlock && block.Number <= toBlock {
			existingBlocks[block.Number] = block
		}
	}
	bf.mut.Unlock()

	var blockNumsToFetch []int64
	// schedule fetch of missing blocks
	for i := fromBlock; i <= toBlock; i++ {
		if _, exists := existingBlocks[i]; exists {
			continue
		}
		blockNumsToFetch = append(blockNumsToFetch, i)
	}

	if len(blockNumsToFetch) > 0 {
		blocksFetched, err := bf.fetchBlocksByNumbers(ctx, blockNumsToFetch)
		if err != nil {
			bf.logger.Errorw("BlockFetcher#BlockRange error while fetching missing blocks", "err", err, "fromBlock", fromBlock, "toBlock", toBlock)
			return err
		}

		if len(blocksFetched) < len(blockNumsToFetch) {
			bf.logger.Warnw("BlockFetcher#BlockRange did not fetch all requested blocks",
				"fromBlock", fromBlock, "toBlock", toBlock, "requestedLen", len(blockNumsToFetch), "blocksFetched", len(blocksFetched))
		}

		bf.mut.Lock()

		for _, blockItem := range blocksFetched {
			block := blockItem

			existingBlocks[block.Number] = &block
			bf.recent[block.Hash] = &block

			if bf.latestBlockNum < block.Number {
				bf.latestBlockNum = block.Number
			}
		}

		bf.cleanRecent()
		bf.mut.Unlock()

		select {
		case bf.notifications <- struct{}{}:
		default:
		}
	}
	return nil
}

func (bf *BlockFetcher) cleanRecent() {
	var blockNumsToDelete []common.Hash
	for _, b := range bf.recent {
		block := b
		if block.Number < bf.latestBlockNum-int64(bf.config.BlockFetcherHistorySize()) {
			blockNumsToDelete = append(blockNumsToDelete, block.Hash)
		}
	}
	for _, toDelete := range blockNumsToDelete {
		delete(bf.recent, toDelete)
	}
}

func (bf *BlockFetcher) findBlockByHash(hash common.Hash) *Block {
	bf.mut.Lock()
	defer bf.mut.Unlock()
	block, ok := bf.recent[hash]
	if ok {
		return block
	}
	return nil
}

func (bf *BlockFetcher) fetchBlocksByNumbers(ctx context.Context, numbers []int64) (map[int64]Block, error) {
	var reqs []rpc.BatchElem
	for _, number := range numbers {
		req := rpc.BatchElem{
			Method: "eth_getBlockByNumber",
			Args:   []interface{}{Int64ToHex(number), true},
			Result: &Block{},
		}
		reqs = append(reqs, req)
	}

	bf.logger.Debugw(fmt.Sprintf("BlockFetcher: fetching %v blocks", len(reqs)), "n", len(reqs))
	if err := bf.batchFetch(ctx, reqs); err != nil {
		return nil, err
	}

	blocks := make(map[int64]Block)
	for i, req := range reqs {
		result, err := req.Result, req.Error
		if err != nil {
			bf.logger.Warnw("BlockFetcher#fetchBlocksByNumbers error while fetching block", "err", err, "blockNum", numbers[i])
			continue
		}

		b, is := result.(*Block)
		if !is {
			return nil, errors.Errorf("expected result to be a %T, got %T", &Block{}, result)
		}
		if b == nil {
			//TODO: can this happen on "Fetching it too early results in an empty block." ?
			bf.logger.Warnw("BlockFetcher#fetchBlocksByNumbers got nil block", "blockNum", numbers[i], "index", i)
			continue
		}
		if b.Hash == (common.Hash{}) {
			bf.logger.Warnw("BlockFetcher#fetchBlocksByNumbers block was missing hash", "block", b, "blockNum", numbers[i], "erroredBlockNum", b.Number)
			continue
		}

		blocks[b.Number] = *b
	}
	return blocks, nil
}

func (bf *BlockFetcher) batchFetch(ctx context.Context, reqs []rpc.BatchElem) error {
	batchSize := int(bf.config.BlockFetcherBatchSize())

	if batchSize == 0 {
		batchSize = len(reqs)
	}
	for i := 0; i < len(reqs); i += batchSize {
		j := i + batchSize
		if j > len(reqs) {
			j = len(reqs)
		}

		logger.Debugw(fmt.Sprintf("BlockFetcher: batch fetching blocks %v thru %v", HexToInt64(reqs[i].Args[0]), HexToInt64(reqs[j-1].Args[0])))

		err := bf.ethClient.BatchCallContext(ctx, reqs[i:j])
		if ctx.Err() != nil {
			break
		} else if err != nil {
			return errors.Wrap(err, "BlockFetcher#fetchBlocks error fetching blocks with BatchCallContext")
		}
	}
	return nil
}

func fromEthBlock(ethBlock types.Block) Block {
	var block Block
	block.Number = ethBlock.Number().Int64()
	block.Hash = ethBlock.Hash()
	block.ParentHash = ethBlock.ParentHash()
	return block
}

func headFromBlock(ethBlock Block) models.Head {
	var head models.Head
	head.Number = ethBlock.Number
	head.Hash = ethBlock.Hash
	head.ParentHash = ethBlock.ParentHash
	return head
}

func (bf *BlockFetcher) syncLatestHead(ctx context.Context, head models.Head) (models.Head, error) {
	bf.syncingMut.Lock()
	defer bf.syncingMut.Unlock()

	from := head.Number - int64(bf.config.BlockFetcherHistorySize()-1)
	if from < 0 {
		from = 0
	}

	mark := time.Now()
	fetched := 0

	bf.logger.Debugw("BlockFetcher: starting sync head",
		"blockNumber", head.Number,
		"fromBlockHeight", from,
		"toBlockHeight", head.Number)
	defer func() {
		if ctx.Err() != nil {
			return
		}
		bf.logger.Debugw("BlockFetcher: finished sync head",
			"fetched", fetched,
			"blockNumber", head.Number,
			"time", time.Since(mark),
			"fromBlockHeight", from,
			"toBlockHeight", head.Number)
	}()

	ethBlockPtr, err := bf.ethClient.FastBlockByHash(ctx, head.Hash)
	if ctx.Err() != nil {
		return models.Head{}, nil
	}
	if err != nil {
		return models.Head{}, errors.Wrap(err, "FastBlockByHash failed")
	}

	var ethBlock = *ethBlockPtr
	var block = fromEthBlock(ethBlock)

	bf.mut.Lock()
	bf.recent[block.Hash] = &block
	bf.mut.Unlock()

	var chainTip = head
	var currentHead = &chainTip

	bf.logger.Debugf("Latest block number: %v", block.Number)

	for i := block.Number - 1; i >= from; i-- {
		// NOTE: Sequential requests here mean it's a potential performance bottleneck, be aware!
		var existingBlock *Block
		existingBlock = bf.findBlockByHash(currentHead.Hash)
		if existingBlock != nil {
			block = *existingBlock
		} else {
			bf.logger.Debugf("Fetching BlockByNumber: %v, as existing block was not found by %v", i, head.Hash)
			//TODO: perhaps implement FastBlockByNumber
			ethBlockPtr, err = bf.ethClient.BlockByNumber(ctx, big.NewInt(i))
			fetched++
			if ctx.Err() != nil {
				break
			} else if err != nil {
				return models.Head{}, errors.Wrap(err, "BlockByNumber failed")
			}

			block = fromEthBlock(*ethBlockPtr)

			bf.mut.Lock()
			bf.recent[block.Hash] = &block
			bf.mut.Unlock()
		}

		head := headFromBlock(block)
		currentHead.Parent = &head
		currentHead = &head
	}
	bf.logger.Debugf("Returning chain of length %v", chainTip.ChainLength())
	return chainTip, nil
}
