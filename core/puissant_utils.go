package core

import (
	"bytes"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func commitTransaction(tx *types.Transaction, chain *BlockChain, chainConfig *params.ChainConfig, coinbase common.Address, envState *state.StateDB, envGasPool *GasPool, envHeader *types.Header, revertIfFailed, gasReq21000 bool, receiptProcessors ...ReceiptProcessor) (*types.Receipt, error) {

	var (
		snap = envState.Snapshot()
		gp   = envGasPool.Gas()
	)

	receipt, err := ApplyTransaction(chainConfig, chain, &coinbase, envGasPool, envState, envHeader, tx, &envHeader.GasUsed, *chain.GetVMConfig(), revertIfFailed, gasReq21000, receiptProcessors...)
	if err != nil {
		envState.RevertToSnapshot(snap)
		envGasPool.SetGas(gp)
		return nil, err
	}

	return receipt, nil
}

func CreateGasPool(srcGasPool *GasPool, chainConf *params.ChainConfig, header *types.Header) *GasPool {
	if srcGasPool != nil {
		return new(GasPool).AddGas(srcGasPool.Gas())
	}

	gasPool := new(GasPool).AddGas(header.GasLimit)
	gasPool.SubGas(params.SystemTxsGas)
	return gasPool
}

func transactionID(signer types.Signer, tx *types.Transaction) string {
	var (
		id        bytes.Buffer
		nonce     = tx.Nonce()
		sender, _ = types.Sender(signer, tx)
	)

	id.Grow(common.AddressLength + 4)
	id.Write(sender.Bytes())
	id.WriteByte(byte(nonce))
	id.WriteByte(byte(nonce >> 8))
	id.WriteByte(byte(nonce >> 16))
	id.WriteByte(byte(nonce >> 24))

	return id.String()
}
