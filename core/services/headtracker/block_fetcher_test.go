package headtracker_test

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/smartcontractkit/chainlink/core/internal/cltest"
	"github.com/smartcontractkit/chainlink/core/internal/utils"
	"github.com/smartcontractkit/chainlink/core/services/eth/mocks"
	"github.com/smartcontractkit/chainlink/core/services/headtracker"
	"github.com/smartcontractkit/chainlink/core/store/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestBlockFetcher_Start(t *testing.T) {
	t.Parallel()

	config := headtracker.NewBlockFetcherConfigWithDefaults()

	t.Run("fetches a range of blocks", func(t *testing.T) {
		store, cleanup := cltest.NewStore(t)
		defer cleanup()
		logger := store.Config.CreateProductionLogger()

		ethClient := new(mocks.Client)
		blockFetcher := headtracker.NewBlockFetcher(ethClient, config, logger)

		ethClient.On("BatchCallContext", mock.Anything, mock.MatchedBy(func(b []rpc.BatchElem) bool {
			return len(b) == 2 &&
				b[0].Method == "eth_getBlockByNumber" && b[0].Args[0] == "0x29" && b[0].Args[1] == true &&
				b[1].Method == "eth_getBlockByNumber" && b[1].Args[0] == "0x2a" && b[1].Args[1] == true
		})).Return(nil).Run(func(args mock.Arguments) {
			elems := args.Get(1).([]rpc.BatchElem)
			elems[0].Result = &headtracker.Block{
				Number: 42,
				Hash:   utils.NewHash(),
			}
			elems[1].Result = &headtracker.Block{
				Number: 41,
				Hash:   utils.NewHash(),
			}
		})

		blockRange, err := blockFetcher.BlockRange(context.Background(), 41, 42)
		require.NoError(t, err)

		assert.Len(t, blockRange, 2)
		assert.Len(t, blockFetcher.BlockCache(), 2)

		ethClient.AssertExpectations(t)
	})
}

func TestBlockFetcher_ConstructsChain(t *testing.T) {

	config := headtracker.NewBlockFetcherConfigWithDefaults()
	store, cleanup := cltest.NewStore(t)
	defer cleanup()
	logger := store.Config.CreateProductionLogger()

	ethClient := new(mocks.Client)
	blockFetcher := headtracker.NewBlockFetcher(ethClient, config, logger)

	block40 := newBlock(40, common.Hash{})
	block41 := newBlock(41, block40.Hash())
	block42 := newBlock(42, block41.Hash())
	h := headFromBlock(block42)

	setupMockCalls(ethClient, []*types.Block{block40, block41, block42})

	head, err := blockFetcher.Chain(context.Background(), *h)
	require.NoError(t, err)
	assert.Equal(t, 3, int(head.ChainLength()))
}

func setupMockCalls(ethClient *mocks.Client, blocks []*types.Block) {
	for _, b := range blocks {
		block := b
		ethClient.On("FastBlockByHash", mock.Anything, mock.MatchedBy(func(hash common.Hash) bool {
			return hash == block.Hash()
		})).Return(block, nil)

		ethClient.On("BlockByNumber", mock.Anything, mock.MatchedBy(func(number *big.Int) bool {
			return number.Int64() == block.Number().Int64()
		})).Return(block, nil)
	}
}

func newBlock(number int, parentHash common.Hash) *types.Block {
	header := &types.Header{
		Root:       cltest.NewHash(),
		ParentHash: parentHash,
		Number:     big.NewInt(int64(number)),
	}
	block := types.NewBlock(header, make([]*types.Transaction, 0), make([]*types.Header, 0), make([]*types.Receipt, 0), nil)
	return block
}

func headFromBlock(block *types.Block) *models.Head {
	h := &models.Head{Hash: block.Hash(), Number: block.Number().Int64(), ParentHash: block.ParentHash()}
	return h
}
