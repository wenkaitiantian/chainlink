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

type BlockDownloader struct {
	ethClient eth.Client
	logger    *logger.Logger

	batchSize   int
	historySize int

	inProgress     []BlockDownload
	recent         map[common.Hash]*Block
	latestBlockNum int64

	notifications chan struct{}

	mut sync.Mutex
}

func NewBlockDownloader(ethClient eth.Client, batchSize uint32, historySize uint16) *BlockDownloader {
	return &BlockDownloader{
		logger:      logger.Default,
		ethClient:   ethClient,
		batchSize:   int(batchSize),
		historySize: int(historySize),
		recent:      make(map[common.Hash]*Block, 0),
	}
}

func (bd *BlockDownloader) StartDownloadAsync(ctx context.Context, fromBlock int64, toBlock int64) {
	timeout := 2 * time.Minute

	go func() {
		utils.RetryWithBackoff(ctx, func() (retry bool) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			err := bd.downloadRange(ctx, fromBlock, toBlock)
			if err != nil {
				bd.logger.Errorw("BlockFetcher#StartDownload error while downloading blocks. Will retry",
					"err", err, "fromBlock", fromBlock, "toBlock", toBlock)
				return true
			}
			return false
		})
	}()
}

func (bd *BlockDownloader) RecentSorted() []*Block {

	toReturn := make([]*Block, 0)

	for _, b := range bd.recent {
		block := b
		toReturn = append(toReturn, block)
	}

	sort.Slice(toReturn, func(i, j int) bool {
		return toReturn[i].Number < toReturn[j].Number
	})
	return toReturn
}
func (bd *BlockDownloader) GetBlockRange(ctx context.Context, head models.Head, fromBlock int64, toBlock int64) ([]*Block, error) {
	bd.logger.Debugw("BlockDownloader#BlockRange requested range", "fromBlock", fromBlock, "toBlock", toBlock)

	if fromBlock < 0 || toBlock < fromBlock {
		return make([]*Block, 0), errors.Errorf("Invalid range: %d -> %d", fromBlock, toBlock)
	}

	blocks := make(map[int64]*Block, 0)

	err := bd.downloadRange(ctx, fromBlock, toBlock)
	if err != nil {
		return make([]*Block, 0), errors.Wrapf(err, "BlockFetcher#GetBlockRange error while downloading blocks %v -> %v",
			fromBlock, toBlock)
	}
	bd.mut.Lock()

	for _, b := range bd.recent {
		block := b
		if block.Number >= fromBlock && block.Number <= toBlock {
			blocks[block.Number] = block
		}
	}
	bd.mut.Unlock()

	blockRange := make([]*Block, 0)
	for _, b := range blocks {
		block := b
		blockRange = append(blockRange, block)
	}

	return blockRange, nil
	// check if currently downloaded
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

func (bd *BlockDownloader) downloadRange(ctx context.Context, fromBlock int64, toBlock int64) error {

	bd.mut.Lock()

	existingBlocks := make(map[int64]*Block)

	for _, b := range bd.recent {
		block := b
		if block.Number >= fromBlock && block.Number <= toBlock {
			existingBlocks[block.Number] = block
		}
	}
	bd.mut.Unlock()

	var blockNumsToFetch []int64
	// schedule fetch of missing blocks
	for i := fromBlock; i <= toBlock; i++ {
		if _, exists := existingBlocks[i]; exists {
			continue
		}
		blockNumsToFetch = append(blockNumsToFetch, i)
	}

	if len(blockNumsToFetch) > 0 {
		blocksFetched, err := bd.fetchBlocksByNumbers(ctx, blockNumsToFetch)
		if err != nil {
			bd.logger.Errorw("BlockDownloader#BlockRange error while fetching missing blocks", "err", err, "fromBlock", fromBlock, "toBlock", toBlock)
			return err
		}

		if len(blocksFetched) < len(blockNumsToFetch) {
			bd.logger.Warnw("BlockDownloader#BlockRange did not fetch all requested blocks",
				"fromBlock", fromBlock, "toBlock", toBlock, "requestedLen", len(blockNumsToFetch), "blocksFetched", len(blocksFetched))
		}

		bd.mut.Lock()

		for _, blockItem := range blocksFetched {
			block := blockItem

			existingBlocks[block.Number] = &block
			bd.recent[block.Hash] = &block

			if bd.latestBlockNum < block.Number {
				bd.latestBlockNum = block.Number
			}
		}

		bd.cleanRecent()
		bd.mut.Unlock()

		select {
		case bd.notifications <- struct{}{}:
		default:
		}
	}
	return nil
}

func (bd *BlockDownloader) cleanRecent() {
	var blockNumsToDelete []common.Hash
	for _, b := range bd.recent {
		block := b
		if block.Number < bd.latestBlockNum-int64(bd.historySize) {
			blockNumsToDelete = append(blockNumsToDelete, block.Hash)
		}
	}
	for _, toDelete := range blockNumsToDelete {
		delete(bd.recent, toDelete)
	}
}

func (bd *BlockDownloader) findBlockByHash(hash common.Hash) *Block {
	bd.mut.Lock()
	defer bd.mut.Unlock()
	block, ok := bd.recent[hash]
	if ok {
		return block
	}
	return nil
}

func (bd *BlockDownloader) fetchBlocksByNumbers(ctx context.Context, numbers []int64) (map[int64]Block, error) {
	var reqs []rpc.BatchElem
	for _, number := range numbers {
		req := rpc.BatchElem{
			Method: "eth_getBlockByNumber",
			Args:   []interface{}{Int64ToHex(number), true},
			Result: &Block{},
		}
		reqs = append(reqs, req)
	}

	bd.logger.Debugw(fmt.Sprintf("BlockFetcher: fetching %v blocks", len(reqs)), "n", len(reqs))
	if err := bd.batchFetch(ctx, reqs); err != nil {
		return nil, err
	}

	blocks := make(map[int64]Block)
	for i, req := range reqs {
		result, err := req.Result, req.Error
		if err != nil {
			bd.logger.Warnw("BlockFetcher#fetchBlocksByNumbers error while fetching block", "err", err, "blockNum", numbers[i])
			continue
		}

		b, is := result.(*Block)
		if !is {
			return nil, errors.Errorf("expected result to be a %T, got %T", &Block{}, result)
		}
		if b == nil {
			//TODO: can this happen on "Fetching it too early results in an empty block." ?
			bd.logger.Warnw("BlockFetcher#fetchBlocksByNumbers got nil block", "blockNum", numbers[i], "index", i)
			continue
		}
		if b.Hash == (common.Hash{}) {
			bd.logger.Warnw("BlockFetcher#fetchBlocksByNumbers block was missing hash", "block", b, "blockNum", numbers[i], "erroredBlockNum", b.Number)
			continue
		}

		blocks[b.Number] = *b
	}
	return blocks, nil
}

func (bd *BlockDownloader) batchFetch(ctx context.Context, reqs []rpc.BatchElem) error {
	batchSize := bd.batchSize

	if batchSize == 0 {
		batchSize = len(reqs)
	}
	for i := 0; i < len(reqs); i += batchSize {
		j := i + batchSize
		if j > len(reqs) {
			j = len(reqs)
		}

		logger.Debugw(fmt.Sprintf("BlockFetcher: batch fetching blocks %v thru %v", HexToInt64(reqs[i].Args[0]), HexToInt64(reqs[j-1].Args[0])))

		if err := bd.ethClient.BatchCallContext(ctx, reqs[i:j]); err != nil {
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

func (bd *BlockDownloader) syncLatestHead(ctx context.Context, head models.Head) (*models.Head, error) {
	from := head.Number - int64(bd.historySize-1)
	if from < 0 {
		from = 0
	}

	mark := time.Now()
	fetched := 0

	bd.logger.Debugw("BlockFetcher: starting sync head",
		"blockNumber", head.Number,
		"fromBlockHeight", from,
		"toBlockHeight", head.Number)
	defer func() {
		if ctx.Err() != nil {
			return
		}
		bd.logger.Debugw("BlockFetcher: finished sync head",
			"fetched", fetched,
			"blockNumber", head.Number,
			"time", time.Since(mark),
			"fromBlockHeight", from,
			"toBlockHeight", head.Number)
	}()

	ethBlockPtr, err := bd.ethClient.FastBlockByHash(ctx, head.Hash)
	if err != nil {
		return nil, errors.Wrap(err, "FastBlockByHash failed")
	}

	var ethBlock = *ethBlockPtr
	var block = fromEthBlock(ethBlock)
	var chainTip = head
	var chain = &chainTip

	bd.logger.Warnf("block: %v", block.Number)

	for i := block.Number - 1; i >= from; i-- {
		// NOTE: Sequential requests here mean it's a potential performance bottleneck, be aware!
		var existingBlock *Block
		existingBlock = bd.findBlockByHash(head.Hash)
		if existingBlock != nil {
			block = *existingBlock

		} else {
			bd.logger.Warnf("BlockByNumber: %v", i)
			ethBlockPtr, err = bd.ethClient.BlockByNumber(ctx, big.NewInt(i))
			fetched++
			if ctx.Err() != nil {
				break //TODO:
			} else if err != nil {
				return nil, errors.Wrap(err, "BlockByNumber failed")
			}

			block = fromEthBlock(*ethBlockPtr)

			bd.mut.Lock()
			bd.recent[block.Hash] = &block
			bd.mut.Unlock()
		}

		head := headFromBlock(block)
		chain.Parent = &head
		chain = &head
	}
	return &chainTip, nil
}

//func aaa() {

//	var h = &head
//	var reqs []rpc.BatchElem
//	for i := 0; i < 50 && h != nil; i++ {
//		bf.logger.Debugf("=== block hash %v", h.Hash)
//		req := rpc.BatchElem{
//			Method: "eth_getBlockByHash",
//			Args:   []interface{}{h.Hash, true},
//			Result: &json.RawMessage{},
//		}
//		reqs = append(reqs, req)
//		h = h.Parent
//	}
//
//	ctx, cancel := eth.DefaultQueryCtx(ctx)
//	defer cancel()
//
//	err := bf.store.EthClient.BatchCallContext(ctx, reqs)
//	if err != nil {
//		return errors.Wrap(err, "HeadTracker#handleNewHighestHead failed fetching multiple blocks")
//	}
//
//	elapsed1 := time.Since(start1)
//	blockBatchFetchDuration.Observe(float64(elapsed1.Milliseconds()))
//
//	totalSize := 0
//	for _, req := range reqs {
//		result, errReq := req.Result, req.Error
//
//		//res := result.(*Block)
//		if errReq != nil {
//			bf.logger.Errorf("=== block err %v", req.Error.Error())
//			return errReq
//		}
//
//		var raw json.RawMessage = *result.(*json.RawMessage)
//
//		var head *types.Header
//		var body eth.RpcBlock
//		if errReq = json.Unmarshal(raw, &head); errReq != nil {
//			return errReq
//		}
//		if errReq = json.Unmarshal(raw, &body); errReq != nil {
//			return errReq
//		}
//		bf.logger.Debugf("=== block hash %v with %v txs, size: %v", head.Hash(), len(body.Transactions), len(raw))
//		totalSize += len(raw)
//	}
//	promBlockSizenHist.Observe(float64(totalSize))
//	bf.logger.Debugf("========================= HeadTracker: getting %v, len(reqs) blocks took %v ms, total size: %v", len(reqs), elapsed1.Milliseconds(), totalSize)
//	return nil
//}
