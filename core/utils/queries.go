package utils

import (
	gethCommon "github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

const EthMaxInFlightTransactionsWarningLabel = `WARNING: You may need to increase ETH_MAX_IN_FLIGHT_TRANSACTIONS to boost your node's transaction throughput, however you do this at your own risk. You MUST first ensure your ethereum node is configured not to ever evict local transactions that exceed this number otherwise the node can get permanently stuck.`

// CheckEthTxQueueCapacity returns an error if inserting this transaction would
// exceed the maximum queue size.
//
// NOTE: This is in the utils package to avoid import cycles, since it is used
// in both offchainreporting and adapters. Tests can be found in
// bulletprooftxmanager_test.go
func CheckEthTxQueueCapacity(db *gorm.DB, fromAddress gethCommon.Address, maxQueuedTransactions uint64) (err error) {
	if maxQueuedTransactions == 0 {
		return nil
	}
	var count uint64
	err = db.Raw(`SELECT count(*) FROM eth_txes WHERE from_address = $1 AND state = 'unstarted'`, fromAddress).Scan(&count).Error
	if err != nil {
		err = errors.Wrap(err, "bulletprooftxmanager.CheckEthTxQueueCapacity query failed")
		return
	}

	if count >= maxQueuedTransactions {
		err = errors.Errorf("cannot create transaction; too many unstarted transactions in the queue (%v/%v). %s", count, maxQueuedTransactions, EthMaxInFlightTransactionsWarningLabel)
	}
	return
}
