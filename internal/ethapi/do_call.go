package ethapi

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

func TransactionDoCall(ctx context.Context, b Backend, args TransactionArgs, blockNrOrHash rpc.BlockNumberOrHash, overrides *StateOverride, timeout time.Duration, globalGasCap uint64) (*core.ExecutionResult, []*types.Log, error) {
	defer func(start time.Time) { log.Debug("Executing EVM call finished", "runtime", time.Since(start)) }(time.Now())
	state, header, err := b.StateAndHeaderByNumberOrHash(ctx, blockNrOrHash)
	if state == nil || err != nil {
		return nil, nil, err
	}
	if err := overrides.Apply(state); err != nil {
		return nil, nil, err
	}
	// Setup context so it may be cancelled the call has completed
	// or, in case of unmetered gas, setup a context with a timeout.
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	// Make sure the context is cancelled when the call has completed
	// this makes sure resources are cleaned up.
	defer cancel()

	// Get a new instance of the EVM.
	msg, err := args.ToMessage(globalGasCap, header.BaseFee)
	if err != nil {
		return nil, nil, err
	}
	evm, vmError, err := b.GetEVM(ctx, msg, state, header, &vm.Config{NoBaseFee: true})
	if err != nil {
		return nil, nil, err
	}
	// Wait for the context to be done and cancel the evm. Even if the
	// EVM has finished, cancelling may be done (repeatedly)
	go func() {
		<-ctx.Done()
		evm.Cancel()
	}()

	// Execute the message.
	gp := new(core.GasPool).AddGas(math.MaxUint64)
	//core.ApplyTransaction()
	result, err := core.ApplyMessage(evm, msg, gp)
	if err := vmError(); err != nil {
		return nil, nil, err
	}

	// If the timer caused an abort, return an appropriate error message
	if evm.Cancelled() {
		return nil, nil, fmt.Errorf("execution aborted (timeout = %v)", timeout)
	}
	if err != nil {
		return result, nil, fmt.Errorf("err: %w (supplied gas %d)", err, msg.Gas())
	}
	return result, state.Logs(), nil
}

func (b *BlockChainAPI) PredictDoCall(ctx context.Context, tx types.Transaction, blockNrOrHash rpc.BlockNumberOrHash, overrides *StateOverride) (hexutil.Bytes, []*types.Log, error) {
	chainId := b.b.ChainConfig().ChainID
	msg, err := tx.AsMessage(types.NewLondonSigner(chainId), nil)
	from := msg.From()
	gas := hexutil.Uint64(tx.Gas())
	gasPrice := hexutil.Big(*tx.GasPrice())
	value := hexutil.Big(*tx.Value())
	nonce := hexutil.Uint64(tx.Nonce())
	data := hexutil.Bytes(tx.Data())
	hexChainId := hexutil.Big(*chainId)
	args := TransactionArgs{
		From:     &from,
		To:       tx.To(),
		Gas:      &gas,
		GasPrice: &gasPrice,
		Value:    &value,
		Nonce:    &nonce,
		Data:     &data,
		Input:    &data,
		ChainID:  &hexChainId,
	}
	result, logs, err := TransactionDoCall(ctx, b.b, args, blockNrOrHash, overrides, b.b.RPCEVMTimeout(), b.b.RPCGasCap())
	if err != nil {
		log.Error("PredictDoCall err.....", err.Error())
		return nil, nil, err
	}
	// If the result contains a revert reason, try to unpack and return it.
	if len(result.Revert()) > 0 {
		return nil, nil, newRevertError(result)
	}
	return result.Return(), logs, result.Err
}

type RPCTransactionPlus struct {
	Tx     *RPCTransaction `json:"tx"`
	Logs   []*types.Log    `json:"logs"`
	Result string          `json:"result"`
	Revert string          `json:"revert"`
}

func (s *TransactionAPI) SetBlockChainAPI(bc *BlockChainAPI) {
	s.bc = bc
}

// GetTransactionByHash returns the transaction for the given hash
func (s *TransactionAPI) GetTransactionByHashAndPredictDoCall(ctx context.Context, hash common.Hash) (*RPCTransactionPlus, error) {
	// Try to return an already finalized transaction
	tx, blockHash, blockNumber, index, err := s.b.GetTransaction(ctx, hash)
	if err != nil {
		return nil, err
	}
	if tx != nil {
		header, err := s.b.HeaderByHash(ctx, blockHash)
		if err != nil {
			return nil, err
		}
		tp := newRPCTransaction(tx, blockHash, blockNumber, index, header.BaseFee, s.b.ChainConfig())

		return &RPCTransactionPlus{
			Tx: tp,
		}, nil
	}
	// No finalized transaction, try to retrieve it from the pool
	if tx := s.b.GetPoolTransaction(hash); tx != nil {
		block := s.b.CurrentBlock()

		blockNum := rpc.BlockNumber(block.Number().Int64())
		blackHash := block.Hash()
		blockorhash := rpc.BlockNumberOrHash{
			BlockNumber:      &blockNum,
			BlockHash:        &blackHash,
			RequireCanonical: false,
		}

		result, logs, revertErr := s.bc.PredictDoCall(ctx, *tx, blockorhash, nil)
		tp := newRPCPendingTransaction(tx, s.b.CurrentHeader(), s.b.ChainConfig())
		return &RPCTransactionPlus{
			Tx:     tp,
			Logs:   logs,
			Result: result.String(),
			Revert: revertErr.Error(),
		}, nil
	}

	// Transaction unknown, return as such
	return nil, nil

}
