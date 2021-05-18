package headtracker_test

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/rpc"
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

	config := &headtracker.BlockFetcherFakeConfig{
		EthFinalityDepthField:           42,
		BlockBackfillDepthField:         50,
		BlockFetcherBatchSizeField:      4,
		BlockFetcherHistorySizeField:    100,
		EthHeadTrackerHistoryDepthField: 100,
		GasUpdaterBatchSizeField:        0,
		GasUpdaterBlockDelayField:       0,
		GasUpdaterBlockHistorySizeField: 2,
	}

	t.Run("fetches a range of blocks", func(t *testing.T) {
		ethClient := new(mocks.Client)

		blockFetcher := headtracker.NewBlockFetcher(ethClient, config)

		h := &models.Head{Hash: utils.NewHash(), Number: 42}
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

		blockRange, err := blockFetcher.BlockRange(context.Background(), *h, 41, 42)
		require.NoError(t, err)

		assert.Len(t, blockRange, 2)
		assert.Len(t, blockFetcher.BlockCache(), 2)

		ethClient.AssertExpectations(t)
	})
}
